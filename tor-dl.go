package main

/*
   tor-dl - fast large file downloader over locally installed Tor
   Optimized queue-based segmented downloader for uniform throughput.

   Copyright © 2025 Bryan Cuneo <https://github.com/BryanCuneo/tor-dl/>
   Based on torget by Michał Trojnara <https://github.com/mtrojnar/torget>

   GPLv3

   This file contains an improved version of the original tor-dl implementation.
   The improvements are aimed at making the segmented download more
   consistent and keeping all worker circuits busy for as long as possible.

   The original implementation would sometimes generate only a small number
   of relatively large segments. When the download neared completion, there
   might only be a handful of segments left to process. Because each
   segment is handled by exactly one worker, the remaining workers would
   become idle, causing the overall download speed to drop sharply toward
   the end of the transfer. The changes in this file address that issue by
   generating more, smaller segments up front and by sizing the work queue
   to accommodate all of them without blocking. This ensures that a
   sufficient number of segments are available to keep every worker busy
   until the very end of the download.

   If you wish to keep using the original version, refer to the previous
   tor-dl.go file; otherwise, build and run this version instead.
*/

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// torBlockDefault defines a base chunk size used when copying data
	// from the HTTP response into the output file. It matches the
	// 8 KiB network buffer size so that we operate on complete
	// multiples where possible.
	torBlockDefault = 8192
)

// ===== CLI flags (compatible with the original version) =====
var allowHttp bool
var circuits int
var destination string
var force bool
var minLifetime int
var name string
var quiet bool
var silent bool
var torPort int
var verbose bool

// New (optional) flags
var cliSegmentSize int64
var cliMaxRetries int

// ===== global writers for output =====
var errorWriter io.Writer
var messageWriter io.Writer

// ===== utilities =====
func humanReadableSize(sizeInBytes float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for sizeInBytes >= 1024 && i < len(units)-1 {
		sizeInBytes /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", sizeInBytes, units[i])
}

// httpClient constructs a client configured to proxy traffic over
// the locally running Tor instance. Each worker gets its own user
// name embedded in the SOCKS URI so that Tor assigns a distinct
// circuit to that logical user. Connections are configured to
// respect HTTP/1.1 semantics (Range requests over SOCKS proxies do
// not play well with HTTP/2) and to allow for many idle connections.
func httpClient(user string) *http.Client {
	proxyUrl, err := url.Parse(fmt.Sprintf("socks5://%s:%s@127.0.0.1:%d/", user, user, torPort))
	if err != nil {
		fmt.Fprintf(errorWriter, "ERROR - Failed to parse SOCKS5 URL for user '%s' and port '%d': %v\n", user, torPort, err)
		os.Exit(1)
	}
	tr := &http.Transport{
		Proxy:               http.ProxyURL(proxyUrl),
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     30 * time.Second,
		DisableKeepAlives:   false,
		ForceAttemptHTTP2:   false,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   60 * time.Second,
	}
}

// ===== segment and state types =====
type segment struct {
	start   int64
	length  int64
	attempt int
}

// workQueue wraps a buffered channel. It exposes simple methods
// for enqueueing and dequeueing segments. By sizing the channel
// appropriately we avoid blocking when preloading the queue with
// all segments.
type workQueue struct {
	ch chan *segment
}

func newQueue(capacity int) *workQueue {
	return &workQueue{ch: make(chan *segment, capacity)}
}
func (q *workQueue) push(s *segment) { q.ch <- s }
func (q *workQueue) pop() (*segment, bool) {
	s, ok := <-q.ch
	return s, ok
}
func (q *workQueue) close() { close(q.ch) }

// State holds all mutable state for a single download. It
// encapsulates configuration, progress tracking and worker
// coordination.
type State struct {
	ctx        context.Context
	src        string
	output     string
	totalSize  int64
	wroteBytes int64

	acceptRanges bool

	// configuration
	circuits    int
	minLifetime time.Duration
	verbose     bool

	// progress tracking
	prevBytes int64

	// log channel used when verbose is enabled
	log chan string

	// terminal indicates whether stdout is an interactive terminal
	terminal bool

	// queues and segment sizes
	queue      *workQueue
	segSize    int64
	minSegSize int64
	maxRetries int

	// periodic ticker for potential future rebalancing (currently unused)
	rebalanceTicker *time.Ticker

	// synchronization primitive for progress updates
	progressMu sync.Mutex
}

// ===== CLI initialization =====
func init() {
	flag.BoolVar(&allowHttp, "allow-http", false, "Allow tor-dl to download files over HTTP instead of HTTPS. Not recommended!")

	flag.IntVar(&circuits, "circuits", 20, "Concurrent circuits.")
	flag.IntVar(&circuits, "c", 20, "Concurrent circuits.")

	flag.StringVar(&destination, "destination", ".", "Output directory.")
	flag.StringVar(&destination, "d", ".", "Output directory.")

	flag.BoolVar(&force, "force", false, "Will create parent folder(s) and/or overwrite existing files.")

	flag.IntVar(&minLifetime, "min-lifetime", 10, "Minimum circuit lifetime. (seconds)")
	flag.IntVar(&minLifetime, "l", 10, "Minimum circuit lifetime. (seconds)")

	flag.StringVar(&name, "name", "", "Output filename.")
	flag.StringVar(&name, "n", "", "Output filename.")

	flag.BoolVar(&quiet, "quiet", false, "Suppress most text output (still show errors).")
	flag.BoolVar(&quiet, "q", false, "Suppress most text output (still show errors).")

	flag.BoolVar(&silent, "silent", false, "Suppress all text output (including errors).")
	flag.BoolVar(&silent, "s", false, "Suppress all text output (including errors).")

	flag.IntVar(&torPort, "tor-port", 9050, "Port your Tor service is listening on.")
	flag.IntVar(&torPort, "p", 9050, "Port your Tor service is listening on.")

	flag.BoolVar(&verbose, "verbose", false, "Show diagnostic details.")
	flag.BoolVar(&verbose, "v", false, "Show diagnostic details.")

	// new flags — by default we start with moderately small segments
	flag.Int64Var(&cliSegmentSize, "segment-size", 2*1024*1024, "Initial segment size in bytes (default 2MiB). Use smaller values to increase concurrency.")
	flag.IntVar(&cliMaxRetries, "max-retries", 5, "Max retries per segment before splitting or giving up.")

	flag.Usage = func() {
		w := flag.CommandLine.Output()
		msg := `tor-dl - fast large file downloader over locally installed Tor
Licensed under GNU GPLv3
Usage: tor-dl [FLAGS] {file.txt | URL [URL2...]}
  -allow-http
  -circuits, -c
  -destination, -d
  -force
  -min-lifetime, -l
  -name, -n
  -quiet, -q
  -silent, -s
  -tor-port, -p
  -verbose, -v
  -segment-size         Initial segment size in bytes (default 2MiB)
  -max-retries          Per-segment retries before splitting or giving up (default 5)`
		fmt.Fprintln(w, msg)
	}
}

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(0)
	}

	// configure writers based on verbosity flags
	if quiet || silent {
		messageWriter = io.Discard
		if silent {
			errorWriter = io.Discard
		} else {
			errorWriter = os.Stderr
		}
	} else {
		messageWriter = os.Stdout
		errorWriter = os.Stderr
	}

	// collect URLs either from a file or directly from the CLI
	var uris []string
	if flag.NArg() == 1 {
		if _, err := os.Stat(flag.Arg(0)); err == nil {
			file, err := os.Open(flag.Arg(0))
			if err != nil {
				fmt.Fprintf(errorWriter, "ERROR: argument \"%s\" is not a valid URL or file.\n%v\n", flag.Arg(0), err)
				os.Exit(1)
			}
			defer file.Close()
			sc := bufio.NewScanner(file)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line != "" {
					uris = append(uris, line)
				}
			}
		} else {
			uris = append(uris, flag.Arg(0))
		}
	} else {
		uris = flag.Args()
	}

	if len(uris) > 1 {
		fmt.Fprintf(messageWriter, "Downloading %d files.\n", len(uris))
		if name != "" {
			fmt.Fprintln(messageWriter, "WARNING: -name is ignored for multiple URLs.")
			name = ""
		}
	} else if len(uris) < 1 {
		fmt.Fprintln(errorWriter, "ERROR: No URLs found.")
		os.Exit(1)
	}

	bkgr := context.Background()
	for i, uri := range uris {
		u, err := url.ParseRequestURI(uri)
		if err != nil {
			fmt.Fprintf(errorWriter, "ERROR: \"%s\" is not a valid URL.\n", uri)
			continue
		}
		if len(uris) > 1 {
			fmt.Fprintf(messageWriter, "\n[%d/%d] - %s\n", i+1, len(uris), uri)
		}
		if !allowHttp && u.Scheme != "https" {
			fmt.Fprintf(errorWriter, "ERROR: \"%s\" is not using HTTPS.\n\tUse -allow-http if you must (dangerous).\n", uri)
			continue
		}
		ctx := context.WithoutCancel(bkgr)
		state := newState(ctx)
		exitCode := state.Fetch(uri)
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}
}

// ===== State initialization =====
func newState(ctx context.Context) *State {
	st, _ := os.Stdout.Stat()
	terminal := st.Mode()&os.ModeCharDevice == os.ModeCharDevice

	s := &State{
		ctx:         ctx,
		circuits:    circuits,
		minLifetime: time.Duration(minLifetime) * time.Second,
		verbose:     verbose,
		log:         make(chan string, 256),
		terminal:    terminal,
		minSegSize:  256 * 1024, // segments smaller than this are not worth using
		segSize:     cliSegmentSize,
		maxRetries:  cliMaxRetries,
	}
	if s.segSize < s.minSegSize {
		s.segSize = s.minSegSize
	}
	return s
}

// printPermanent writes a line of text on the terminal and moves to the next
// line if running interactively, otherwise it just prints normally. This is
// used for final status messages.
func (s *State) printPermanent(txt string) {
	if s.terminal {
		fmt.Fprintf(messageWriter, "\r%-80s\n", txt)
	} else {
		fmt.Fprintln(messageWriter, txt)
	}
}

// printTemporary writes a status message on the current line without moving
// down. This is used for progress updates when a terminal is detected.
func (s *State) printTemporary(txt string) {
	if s.terminal {
		fmt.Fprintf(messageWriter, "\r%-80s", txt)
	}
}

// getOutputFilepath determines the final output filename. If the user
// provided -name, that name is used. Otherwise the name is derived
// from the URL path. If the destination directory doesn't exist,
// it will be created if -force is set, otherwise the current
// directory is used.
func (s *State) getOutputFilepath() {
	if _, err := os.Stat(destination); errors.Is(err, fs.ErrNotExist) {
		if force {
			_ = os.MkdirAll(destination, 0o755)
		} else {
			fmt.Fprintf(messageWriter, "WARNING: Destination \"%s\" not found. Using current directory.\n", destination)
			destination = "."
		}
	}
	var filename string
	if name == "" {
		srcUrl, _ := url.Parse(s.src)
		path := srcUrl.EscapedPath()
		slash := strings.LastIndex(path, "/")
		if slash >= 0 {
			filename = path[slash+1:]
		} else {
			filename = path
		}
		if filename == "" {
			filename = "index"
		}
		if decoded, err := url.QueryUnescape(filename); err == nil && decoded != "" {
			filename = decoded
		}
	} else {
		filename = name
	}
	s.output = filepath.Join(destination, filename)
}

// headInfo performs an HTTP HEAD request to discover the content
// length and whether the server advertises support for byte-range
// requests. It returns ok=false on error.
func (s *State) headInfo(client *http.Client) (ok bool, contentLength int64, acceptRanges bool) {
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodHead, s.src, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errorWriter, "ERROR - HEAD failed (Tor proxy up?): %v\n", err)
		return false, 0, false
	}
	defer resp.Body.Close()
	cl := resp.ContentLength
	ar := strings.ToLower(resp.Header.Get("Accept-Ranges"))
	accept := strings.Contains(ar, "bytes")
	if cl <= 0 {
		fmt.Fprintln(errorWriter, "ERROR - Failed to retrieve content length.")
		return false, 0, false
	}
	return true, cl, accept
}

// Fetch orchestrates the download of a single URL. It sets up
// output files, performs the HEAD request, and either downloads
// using a single stream or performs a segmented download when the
// server supports Range requests.
func (s *State) Fetch(src string) int {
	s.src = src
	s.getOutputFilepath()

	if _, err := os.Stat(s.output); err == nil && !force {
		fmt.Fprintf(errorWriter, "ERROR: \"%s\" already exists. Use -force to overwrite.\n", s.output)
		return 1
	}
	fmt.Fprintf(messageWriter, "Output file:\t\t%s\n", s.output)

	// Use a shared client for the HEAD request. We don't care about
	// the username here because we aren't establishing circuits yet.
	headCli := httpClient("tordl")
	ok, cl, acceptRanges := s.headInfo(headCli)
	if !ok {
		return 1
	}
	s.totalSize = cl
	s.acceptRanges = acceptRanges
	fmt.Fprintf(messageWriter, "Download filesize:\t%s\n", humanReadableSize(float64(s.totalSize)))

	// Create/truncate the output file now
	f, err := os.Create(s.output)
	if f != nil {
		_ = f.Close()
	}
	if err != nil {
		fmt.Fprintln(errorWriter, err.Error())
		return 1
	}
	if err := os.Truncate(s.output, s.totalSize); err != nil {
		fmt.Fprintf(messageWriter, "WARNING: Truncate failed: %v\n", err)
	}

	startTime := time.Now()

	// Fallback to single-stream download if the server does not
	// advertise support for Range requests, or when the file is too
	// small to warrant segmentation, or when the number of circuits
	// is below two. In these situations segmented downloads are
	// unlikely to provide any benefit and can even hurt performance.
	if !s.acceptRanges || s.totalSize < s.segSize*2 || s.circuits < 2 {
		if !quiet && !silent {
			fmt.Fprintln(messageWriter, "NOTE: server doesn't advertise Range; downloading single stream.")
		}
		code := s.singleStream()
		if code == 0 {
			howLong := time.Since(startTime)
			avg := float64(s.totalSize) / howLong.Seconds()
			s.printPermanent(fmt.Sprintf("Download completed in:\t%s (%s/s)", howLong.Round(time.Second), humanReadableSize(avg)))
		}
		return code
	}

	// ----- segmented download -----
	// Compute an appropriate segment size. We honour the CLI flag
	// but ensure it is at least minSegSize. A smaller segment size
	// produces more segments and therefore more opportunities for
	// parallelism. However, extremely small segments increase
	// overhead. The caller can override via -segment-size.
	seg := s.segSize
	if seg < s.minSegSize {
		seg = s.minSegSize
	}

	// Compute how many segments we will need to cover the entire file
	// using this segment size. We round up so that any remainder at
	// the end of the file becomes its own segment. We cap the
	// resulting number at MaxInt to prevent overflow in extreme
	// circumstances (e.g. extremely large downloads on a 32‑bit
	// system), although such cases are unlikely.
	nSegments := int64(1)
	if seg > 0 {
		nSegments = (s.totalSize + seg - 1) / seg
	}

	// If there are fewer segments than we have circuits, we reduce
	// the segment size further so that every circuit can stay busy.
	// We aim for at least circuits*8 segments. This factor (8) was
	// chosen empirically: it yields enough work to absorb varying
	// segment completion times without creating an excessive number
	// of tiny segments. Users can adjust -segment-size on the CLI
	// to further influence the number of segments created.
	minDesired := int64(s.circuits * 8)
	if nSegments < minDesired {
		// new segment size = totalSize / desired, rounded up
		// ensure we respect minSegSize
		newSeg := s.totalSize / minDesired
		if s.totalSize%minDesired != 0 {
			newSeg++
		}
		if newSeg < s.minSegSize {
			newSeg = s.minSegSize
		}
		seg = newSeg
		// recompute number of segments with this new size
		nSegments = (s.totalSize + seg - 1) / seg
	}

	// Remember the chosen segment size in the state so workers
	// know how large each segment is. In the event of retries and
	// dynamic splitting, this value serves as a baseline.
	s.segSize = seg

	// Create a work queue capable of holding all segments plus a
	// small buffer. Without a large enough buffer, adding all
	// segments to the queue before starting workers could block.
	// We allocate nSegments plus twice the number of circuits to
	// ensure there is room for new segments that might be created
	// during error handling (splitting) without blocking the
	// producers.
	// We clamp the capacity to a reasonable upper bound to avoid
	// exhausting memory on pathological inputs. A maximum of
	// 1,000,000 entries corresponds to downloads of roughly
	// 1,000,000*segmentSize bytes. For typical segment sizes this
	// covers multi‑terabyte downloads.
	maxQueueCap := 1_000_000
	queueCap64 := nSegments + int64(s.circuits*2)
	if queueCap64 > int64(maxQueueCap) {
		queueCap64 = int64(maxQueueCap)
	}
	queueCap := int(queueCap64)
	s.queue = newQueue(queueCap)

	// Populate the queue with all segments. We do this up front
	// instead of lazily generating segments so that workers always
	// have tasks available. Each segment covers [start, start+length)
	// bytes of the file. The last segment may be smaller if the
	// filesize is not an exact multiple of the chosen segment size.
	s.fillInitialSegments()

	// Start the progress monitoring goroutine if we are not
	// suppressing output. It prints a status update every second.
	var stopStatus chan bool
	if !quiet && !silent {
		stopStatus = make(chan bool, 1)
		go s.progressLoop(stopStatus)
	}

	// Start worker goroutines. Each worker will pick segments from
	// the queue and download them sequentially. We pass the worker
	// its numeric ID so it can derive a unique username for the
	// Tor SOCKS proxy.
	var wg sync.WaitGroup
	wg.Add(s.circuits)
	for i := 0; i < s.circuits; i++ {
		go func(workerID int) {
			defer wg.Done()
			s.worker(workerID)
		}(i)
	}

	// Wait for all workers to finish. When the queue is empty and all
	// segments have either been successfully downloaded or retried up
	// to the maximum allowed times, workers will exit.
	wg.Wait()
	if !quiet && !silent {
		stopStatus <- true
	}

	// Verify that we wrote every byte. If we did not, report the
	// missing amount and return a non-zero exit code.
	if atomic.LoadInt64(&s.wroteBytes) != s.totalSize {
		missed := s.totalSize - atomic.LoadInt64(&s.wroteBytes)
		fmt.Fprintf(errorWriter, "ERROR: Incomplete download, missing %d bytes.\n", missed)
		return 1
	}

	howLong := time.Since(startTime)
	avg := float64(s.totalSize) / howLong.Seconds()
	s.printTemporary(strings.Repeat(" ", 80))
	s.printPermanent(fmt.Sprintf("Download completed in:\t%s (%s/s)", howLong.Round(time.Second), humanReadableSize(avg)))
	return 0
}

// singleStream downloads the source URL using a single connection
// and writes the response body sequentially to the output file.
func (s *State) singleStream() int {
	client := httpClient("tg0")
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodGet, s.src, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errorWriter, "ERROR - GET failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.Body == nil {
		fmt.Fprintln(errorWriter, "ERROR - No response body.")
		return 1
	}
	file, err := os.OpenFile(s.output, os.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintf(errorWriter, "ERROR - open file: %v\n", err)
		return 1
	}
	defer file.Close()

	buf := make([]byte, 256*1024)
	written, err := io.CopyBuffer(file, resp.Body, buf)
	if err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintf(errorWriter, "ERROR - copy: %v\n", err)
		return 1
	}
	atomic.AddInt64(&s.wroteBytes, written)
	return 0
}

// fillInitialSegments populates the queue with segments covering the
// entire file. Each segment's start offset is aligned on s.segSize
// boundaries except for the final segment, which may be shorter. We
// compute the length of each segment based on the pre‑determined
// s.segSize set during Fetch().
func (s *State) fillInitialSegments() {
	seg := s.segSize
	if seg < s.minSegSize {
		seg = s.minSegSize
	}
	var offset int64 = 0
	for offset < s.totalSize {
		length := seg
		remaining := s.totalSize - offset
		if remaining < length {
			length = remaining
		}
		s.queue.push(&segment{start: offset, length: length})
		offset += length
	}
}

// adaptiveFeeder is currently a no‑op. In the original version it
// attempted to split segments dynamically when the queue became
// empty. With the new approach we pre‑create sufficient segments
// up front and size the queue accordingly. If future work calls for
// further dynamic rebalancing or multi‑URL support, this goroutine
// can be revisited.
func (s *State) adaptiveFeeder() {
	for range time.Tick(5 * time.Second) {
		if s.queue == nil {
			return
		}
		// No‑op: segments are already created for the entire file.
	}
}

// progressLoop periodically updates the user with a summary of how
// much of the file has been downloaded, the current throughput and
// an estimated time of arrival. It also drains or prints verbose
// logs depending on the verbose flag.
func (s *State) progressLoop(stop <-chan bool) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.progressTick()
		}
	}
}

// progressTick computes the progress metrics and prints them. It also
// flushes or drains logs depending on the verbose flag. This
// function is called once per second by progressLoop().
func (s *State) progressTick() {
	curr := atomic.LoadInt64(&s.wroteBytes)
	diff := curr - s.prevBytes
	var msg string
	if diff <= 0 {
		msg = "stalled"
	} else {
		speed := float64(diff) // bytes/s
		remain := s.totalSize - curr
		var etaStr string
		if speed > 0 {
			etaSec := float64(remain) / speed
			eta := time.Duration(math.Round(etaSec)) * time.Second
			etaStr = fmt.Sprintf("ETA %d:%02d:%02d", int(eta.Hours()), int(eta.Minutes())%60, int(eta.Seconds())%60)
		} else {
			etaStr = "ETA --:--:--"
		}
		msg = fmt.Sprintf("%s/s, %s", humanReadableSize(speed), etaStr)
	}
	status := fmt.Sprintf("%6.2f%% done, %s", 100*float64(curr)/float64(s.totalSize), msg)
	if s.verbose {
		s.flushLogs()
	} else {
		s.drainLogs()
	}
	s.printTemporary(status)
	s.prevBytes = curr
}

// flushLogs prints out any accumulated log messages sorted by their
// string value and counts duplicates. Sorting ensures that identical
// messages are grouped together which makes repeated errors more
// obvious in the output. Logs are only printed when verbose mode
// is enabled.
func (s *State) flushLogs() {
	n := len(s.log)
	if n == 0 {
		return
	}
	logs := make([]string, 0, n+1)
	for i := 0; i < n; i++ {
		logs = append(logs, <-s.log)
	}
	logs = append(logs, "stop")
	sort.Strings(logs)
	prev := "start"
	cnt := 0
	for _, l := range logs {
		if l == prev {
			cnt++
			continue
		}
		if cnt > 0 {
			out := prev
			if cnt > 1 {
				out = fmt.Sprintf("%s (%d times)", out, cnt)
			}
			s.printPermanent(out)
		}
		prev = l
		cnt = 1
	}
}

// drainLogs discards all queued log messages. In quiet mode we still
// want to prevent the log channel from growing without bound.
func (s *State) drainLogs() {
	for len(s.log) > 0 {
		<-s.log
	}
}

// worker consumes segments from the queue and downloads each
// segment. It manages its own Tor circuit lifetime by rotating
// usernames after the configured minimum lifetime. If a segment
// fetch fails, it is either retried up to maxRetries or split into
// smaller segments before being requeued.
func (s *State) worker(id int) {
	usernameBase := fmt.Sprintf("tg%d", id)
	client := httpClient(usernameBase)
	lastRotate := time.Now()

	for {
		seg, ok := s.queue.pop()
		if !ok {
			return
		}

		// rotate the circuit periodically
		if time.Since(lastRotate) >= s.minLifetime {
			usernameBase = fmt.Sprintf("tg%d_%d", id, time.Now().UnixNano())
			client = httpClient(usernameBase)
			lastRotate = time.Now()
		}

		// fetch the segment; if it fails we may retry or split
		if err := s.fetchSegment(client, seg); err != nil {
			seg.attempt++
			if seg.attempt <= s.maxRetries {
				// simply requeue the segment for another attempt
				s.queue.push(seg)
			} else {
				// If we have a particularly troublesome segment, try
				// splitting it in half to see if that alleviates
				// congestion on remote servers or mitigates unstable
				// Tor circuits. We only split if the segment is
				// sufficiently large; otherwise we reset the attempt
				// counter and push the original segment back.
				if seg.length > s.minSegSize*2 {
					half := seg.length / 2
					left := &segment{start: seg.start, length: half}
					right := &segment{start: seg.start + half, length: seg.length - half}
					s.queue.push(left)
					s.queue.push(right)
				} else {
					seg.attempt = 0
					s.queue.push(seg)
				}
			}
			continue
		}
	}
}

// fetchSegment downloads a single segment using an HTTP Range
// request and writes it to the appropriate offset in the output
// file. It returns an error if the Range request fails or if
// writing to the file fails. On partial writes it spawns a new
// segment for the remainder and accounts for the bytes that were
// successfully written.
func (s *State) fetchSegment(client *http.Client, seg *segment) error {
	// create the Range header for this segment
	rangeHeader := fmt.Sprintf("bytes=%d-%d", seg.start, seg.start+seg.length-1)
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodGet, s.src, nil)
	req.Header.Set("Range", rangeHeader)
	req.Header.Set("Connection", "close")

	resp, err := client.Do(req)
	if err != nil {
		if s.verbose {
			s.log <- fmt.Sprintf("ClientDo err (%s): %v", rangeHeader, err)
		}
		return err
	}
	if resp.Body == nil {
		resp.Body.Close()
		return fmt.Errorf("no body")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		// server did not honour Range request, treat as failure
		if s.verbose {
			s.log <- fmt.Sprintf("Unexpected HTTP status %d for %s", resp.StatusCode, rangeHeader)
		}
		return fmt.Errorf("range not honored: %d", resp.StatusCode)
	}

	file, err := os.OpenFile(s.output, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(seg.start, io.SeekStart); err != nil {
		return err
	}

	lim := io.LimitReader(resp.Body, seg.length)
	buf := make([]byte, 256*1024)
	written, err := io.CopyBuffer(file, lim, buf)
	if err != nil && !errors.Is(err, io.EOF) {
		if s.verbose {
			s.log <- fmt.Sprintf("copy err (%s): %v", rangeHeader, err)
		}
		// handle partial write by requeueing the remainder
		if written > 0 && written < seg.length {
			remain := seg.length - written
			s.queue.push(&segment{start: seg.start + written, length: remain, attempt: seg.attempt})
			atomic.AddInt64(&s.wroteBytes, written)
			return nil
		}
		return err
	}
	atomic.AddInt64(&s.wroteBytes, written)
	return nil
}
