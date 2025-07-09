package main

/*
	tor-dl - fast large file downloader over locally installed Tor
	Copyright © 2025 Bryan Cuneo <https://github.com/BryanCuneo/tor-dl/>

	Based on torget by Michał Trojnara <https://github.com/mtrojnar/torget>

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU General Public License as published by
	the Free Software Foundation, either version 3 of the License.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU General Public License for more details.

	You should have received a copy of the GNU General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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

type chunk struct {
	start      int64
	length     int64
	circuit    int
	bytes      int64
	since      time.Time
	cancel     context.CancelFunc
	throughput float64
	lastUpdate time.Time
	active     bool
	retries    int
}

type State struct {
	ctx               context.Context
	src               string
	output            string
	bytesTotal        int64
	bytesPrev         int64
	circuits          int
	minLifetime       time.Duration
	verbose           bool
	chunks            []chunk
	done              chan int
	log               chan string
	terminal          bool
	rwmutex           sync.RWMutex
	completedChunks   int64
	minChunkSize      int64
	maxRetries        int
	rebalanceInterval time.Duration
	darwinInterval    time.Duration
}

const torBlock = 8000 // The longest plain text block in Tor

// Basic function to determine human-readable file sizes
func humanReadableSize(sizeInBytes float32) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0

	for {
		if sizeInBytes >= 1024 && i < len(units) {
			sizeInBytes /= 1024
			i += 1
		} else {
			break
		}
	}

	return fmt.Sprintf("%.2f %s", sizeInBytes, units[i])
}

func httpClient(user string) *http.Client {
	proxyUrl, err := url.Parse(fmt.Sprintf("socks5://%s:%s@127.0.0.1:%d/", user, user, torPort))
	if err != nil {
		fmt.Fprintf(errorWriter, "ERROR - Failed to parse URL with user '%s' and port '%d'\n%v", user, torPort, err)
		os.Exit(1)
	}

	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(proxyUrl),
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
		},
		Timeout: 60 * time.Second,
	}
}

func NewState(ctx context.Context) *State {
	var s State
	s.circuits = circuits
	s.output = ""
	s.minLifetime = time.Duration(minLifetime) * time.Second
	s.verbose = verbose
	s.chunks = make([]chunk, s.circuits)
	s.ctx = ctx
	s.done = make(chan int, s.circuits)   // Buffered channel to prevent blocking
	s.log = make(chan string, 50)         // Increased buffer size
	s.minChunkSize = int64(torBlock * 10) // Minimum chunk size to avoid too many small chunks
	s.maxRetries = 3
	s.rebalanceInterval = 5 * time.Second
	s.darwinInterval = 15 * time.Second
	st, _ := os.Stdout.Stat()
	s.terminal = st.Mode()&os.ModeCharDevice == os.ModeCharDevice

	return &s
}

func (s *State) printPermanent(txt string) {
	if s.terminal {
		fmt.Fprintf(messageWriter, "\r%-40s\n", txt)
	} else {
		fmt.Fprintln(messageWriter, txt)
	}
}

func (s *State) printTemporary(txt string) {
	if s.terminal {
		fmt.Fprintf(messageWriter, "\r%-40s", txt)
	}
}

func (s *State) chunkInit(id int) (client *http.Client, req *http.Request) {
	s.rwmutex.Lock()
	s.chunks[id].bytes = 0
	s.chunks[id].since = time.Now()
	s.chunks[id].lastUpdate = time.Now()
	s.chunks[id].active = true
	s.chunks[id].throughput = 0
	ctx, cancel := context.WithCancel(s.ctx)
	s.chunks[id].cancel = cancel
	s.rwmutex.Unlock()

	client = httpClient(fmt.Sprintf("tg%d", s.chunks[id].circuit))
	req, _ = http.NewRequestWithContext(ctx, "GET", s.src, nil)
	req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d",
		s.chunks[id].start, s.chunks[id].start+s.chunks[id].length-1))
	return
}

func (s *State) chunkFetch(id int, client *http.Client, req *http.Request) {
	defer func() {
		s.rwmutex.Lock()
		s.chunks[id].active = false
		s.rwmutex.Unlock()
		s.done <- id
	}()

	if s.verbose {
		err := s.getExitNode(id, client)
		if err != nil {
			s.log <- fmt.Sprintf("getExitNode: %s", err.Error())
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		s.rwmutex.Lock()
		s.chunks[id].retries++
		s.rwmutex.Unlock()
		s.log <- fmt.Sprintf("Client Do (chunk %d): %s", id, err.Error())
		return
	}
	if resp.Body == nil {
		s.log <- fmt.Sprintf("Client Do (chunk %d): No response body", id)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		s.log <- fmt.Sprintf("Client Do (chunk %d): Unexpected HTTP status: %d", id, resp.StatusCode)
		return
	}

	// Open the output file
	file, err := os.OpenFile(s.output, os.O_WRONLY, 0)
	if err != nil {
		s.log <- fmt.Sprintf("os OpenFile (chunk %d): %s", id, err.Error())
		return
	}
	defer file.Close()

	_, err = file.Seek(s.chunks[id].start, io.SeekStart)
	if err != nil {
		s.log <- fmt.Sprintf("File Seek (chunk %d): %s", id, err.Error())
		return
	}

	// Copy network data to the output file
	buffer := make([]byte, torBlock*2) // Increased buffer size
	lastProgressUpdate := time.Now()

	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			file.Write(buffer[:n])
			now := time.Now()

			s.rwmutex.Lock()
			if int64(n) < s.chunks[id].length {
				s.chunks[id].start += int64(n)
				s.chunks[id].length -= int64(n)
				s.chunks[id].bytes += int64(n)

				// Update throughput calculation more frequently
				if now.Sub(lastProgressUpdate) > time.Second {
					elapsed := now.Sub(s.chunks[id].since)
					if elapsed > 0 {
						s.chunks[id].throughput = float64(s.chunks[id].bytes) / elapsed.Seconds()
					}
					s.chunks[id].lastUpdate = now
					lastProgressUpdate = now
				}
			} else {
				s.chunks[id].bytes += s.chunks[id].length
				s.chunks[id].length = 0
			}
			s.rwmutex.Unlock()

			if s.chunks[id].length == 0 {
				break
			}
		}
		if err != nil {
			if err != io.EOF {
				s.log <- fmt.Sprintf("ReadCloser Read (chunk %d): %s", id, err.Error())
			}
			break
		}
	}
}

func (s *State) getExitNode(id int, client *http.Client) error {
	req, _ := http.NewRequest(http.MethodGet, "https://check.torproject.org/api/ip", nil)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("client Do: %s", err.Error())
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("client Do: Unexpected HTTP status: %d", resp.StatusCode)
	}
	if resp.Body == nil {
		return fmt.Errorf("client Do: No response body")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("io ReadAll: %s", err.Error())
	}

	s.log <- fmt.Sprintf("Circuit #%d: Exit node: %s", id, body)
	return nil
}

func (s *State) printLogs() {
	n := len(s.log)
	if n == 0 {
		return
	}

	logs := make([]string, n+1)
	for i := 0; i < n; i++ {
		logs[i] = <-s.log
	}
	logs[n] = "stop" // Not an expected log line
	sort.Strings(logs)
	prevLog := "start" // Not an expected log line
	cnt := 0
	for _, log := range logs {
		if log == prevLog {
			cnt++
		} else {
			if cnt > 0 {
				if cnt > 1 {
					prevLog = fmt.Sprintf("%s (%d times)", prevLog, cnt)
				}
				s.printPermanent(prevLog)
			}
			prevLog = log
			cnt = 1
		}
	}
}

func (s *State) ignoreLogs() {
	for len(s.log) > 0 {
		<-s.log
	}
}

func (s *State) statusLine() (status string) {
	// Calculate bytes transferred since the previous invocation
	var progressMessage string
	curr := s.bytesTotal

	s.rwmutex.RLock()
	activeChunks := 0
	for id := range s.circuits {
		curr -= s.chunks[id].length
		if s.chunks[id].active {
			activeChunks++
		}
	}
	s.rwmutex.RUnlock()

	if curr == s.bytesPrev {
		progressMessage = "stalled"
	} else {
		bytesPerSecond := curr - s.bytesPrev
		if bytesPerSecond > 0 {
			seconds := (s.bytesTotal - curr) / bytesPerSecond
			progressMessage = fmt.Sprintf("%s/s, ETA %d:%02d:%02d (%d active)",
				humanReadableSize(float32(bytesPerSecond)),
				seconds/3600, seconds/60%60, seconds%60, activeChunks)
		} else {
			progressMessage = "calculating..."
		}
	}
	status = fmt.Sprintf("%6.2f%% done, %s",
		100*float32(curr)/float32(s.bytesTotal), progressMessage)

	s.bytesPrev = curr
	return
}

func (s *State) progress() {
	if s.verbose {
		s.printLogs()
	} else {
		s.ignoreLogs()
	}
	s.printTemporary(s.statusLine())
}

// Improved chunk rebalancing algorithm
func (s *State) rebalanceChunks() {
	s.rwmutex.Lock()
	defer s.rwmutex.Unlock()

	// Find the chunk with the most remaining data
	var longestLength int64 = 0

	// Also track fast and slow performers
	var fastChunks []int
	var slowChunks []int
	now := time.Now()

	for i := 0; i < s.circuits; i++ {
		if s.chunks[i].length > longestLength {
			longestLength = s.chunks[i].length

		}

		// Classify chunks by performance
		if s.chunks[i].active && s.chunks[i].length > 0 {
			elapsed := now.Sub(s.chunks[i].since)
			if elapsed > 5*time.Second { // Only consider chunks that have been running for a while
				if s.chunks[i].throughput > 50000 { // Fast: > 50KB/s
					fastChunks = append(fastChunks, i)
				} else if s.chunks[i].throughput < 10000 { // Slow: < 10KB/s
					slowChunks = append(slowChunks, i)
				}
			}
		}
	}

	// Cancel slow chunks and redistribute their work to fast chunks
	for _, slowIdx := range slowChunks {
		if s.chunks[slowIdx].length > s.minChunkSize && len(fastChunks) > 0 {
			// Cancel the slow chunk
			if s.chunks[slowIdx].cancel != nil {
				s.chunks[slowIdx].cancel()
				s.chunks[slowIdx].cancel = nil
			}

			// Redistribute its work to the fastest chunk
			if len(fastChunks) > 0 {
				fastestIdx := fastChunks[0]
				maxThroughput := s.chunks[fastestIdx].throughput
				for _, idx := range fastChunks {
					if s.chunks[idx].throughput > maxThroughput {
						maxThroughput = s.chunks[idx].throughput
						fastestIdx = idx
					}
				}

				// Transfer work from slow to fast chunk
				s.chunks[fastestIdx].length += s.chunks[slowIdx].length
				s.chunks[slowIdx].length = 0
			}
		}
	}
}

// Kill the worst performing circuit
func (s *State) darwin() {
	victim := -1
	var slowest float64 = 1e9 // Start with a very high value
	now := time.Now()

	s.rwmutex.RLock()
	for id := range s.circuits {
		if s.chunks[id].cancel == nil || !s.chunks[id].active {
			continue
		}
		elapsed := now.Sub(s.chunks[id].since)
		if elapsed < s.minLifetime {
			continue
		}

		// Consider both throughput and retry count
		throughput := s.chunks[id].throughput
		retryPenalty := float64(s.chunks[id].retries) * 1000 // Penalize retries
		adjustedThroughput := throughput - retryPenalty

		if adjustedThroughput < slowest && s.chunks[id].length > s.minChunkSize {
			victim = id
			slowest = adjustedThroughput
		}
	}
	s.rwmutex.RUnlock()

	if victim >= 0 {
		s.rwmutex.Lock()
		if s.chunks[victim].cancel != nil {
			s.chunks[victim].cancel()
			s.chunks[victim].cancel = nil
		}
		s.rwmutex.Unlock()
	}
}

func (s *State) getOutputFilepath() {
	var filename string

	_, err := os.Stat(destination)
	if errors.Is(err, fs.ErrNotExist) {
		if force {
			os.MkdirAll(destination, os.ModePerm)
		} else {
			fmt.Fprintf(messageWriter, "WARNING: Unable to find destination \"%s\". Trying current directory instead.\n", destination)
			destination = "."
		}
	}

	// If no -name argument provided, extract the filename from the URL
	if name == "" {
		srcUrl, _ := url.Parse(s.src) // We've already parsed the URL, ignore errors here
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

		// Remove URL formatting (e.g. "%20" -> " ", "%C3" -> "ö")
		decoded, err := url.QueryUnescape(filename)
		if err != nil {
			fmt.Fprintf(messageWriter, "WARNING: Cannot decode \"%s\" - %v\n", filename, err)
		} else {
			filename = decoded
		}
	} else {
		filename = name
	}

	s.output = filepath.Join(destination, filename)
}

func (s *State) Fetch(src string) int {
	var stop_status chan bool
	s.src = src
	startTime := time.Now()

	s.getOutputFilepath()
	// If the file already exists and the -force argument was not used, exit
	if _, err := os.Stat(s.output); err == nil && !force {
		fmt.Fprintf(errorWriter, "ERROR: \"%s\" already exists. Skipping.\n", s.output)
		return 1
	}
	fmt.Fprintf(messageWriter, "Output file:\t\t%s\n", s.output)

	// Get the target length
	client := httpClient("tordl")
	resp, err := client.Head(s.src)
	if err != nil {
		fmt.Fprintf(errorWriter, "ERROR - Unable to connect to Tor proxy. Is it running?: %v\n", err)
		return 1
	}
	if resp.ContentLength <= 0 {
		fmt.Fprintln(errorWriter, "ERROR - Failed to retrieve download length")
		return 1
	}
	s.bytesTotal = resp.ContentLength
	fmt.Fprintf(messageWriter, "Download filesize:\t%s\n", humanReadableSize(float32(s.bytesTotal)))

	// Create the output file. This will overwrite an existing file
	file, err := os.Create(s.output)
	if file != nil {
		file.Close()
	}
	if err != nil {
		fmt.Fprintln(messageWriter, err.Error())
		return 1
	}

	// Initialize chunks with better distribution
	baseChunkLen := s.bytesTotal / int64(s.circuits)
	remainder := s.bytesTotal % int64(s.circuits)

	seq := 0
	for id := range s.circuits {
		s.chunks[id].start = int64(id) * baseChunkLen
		s.chunks[id].length = baseChunkLen
		s.chunks[id].circuit = seq
		s.chunks[id].retries = 0

		// Distribute remainder among first few chunks
		if int64(id) < remainder {
			s.chunks[id].length++
			// Adjust start positions for subsequent chunks
			for j := id + 1; j < s.circuits; j++ {
				s.chunks[j].start++
			}
		}
		seq++
	}

	// If not -quiet or -silent, update status message every 1 second
	if !quiet && !silent {
		stop_status = make(chan bool, 1)
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-stop_status:
					return
				case <-ticker.C:
					s.progress()
				}
			}
		}()
	}

	// Start rebalancing goroutine
	rebalanceTicker := time.NewTicker(s.rebalanceInterval)
	defer rebalanceTicker.Stop()
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-rebalanceTicker.C:
				s.rebalanceChunks()
			}
		}
	}()

	// Start darwin goroutine
	darwinTicker := time.NewTicker(s.darwinInterval)
	defer darwinTicker.Stop()
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-darwinTicker.C:
				s.darwin()
			}
		}
	}()

	// Spawn initial fetchers
	go func() {
		for id := range s.circuits {
			client, req := s.chunkInit(id)
			go s.chunkFetch(id, client, req)
			time.Sleep(200 * time.Millisecond) // Reduced delay
		}
	}()

	// Main download loop
	for {
		select {
		case id := <-s.done:
			s.rwmutex.RLock()
			chunkLength := s.chunks[id].length
			retries := s.chunks[id].retries
			s.rwmutex.RUnlock()

			if chunkLength > 0 {
				// Error occurred or chunk was cancelled
				if retries < s.maxRetries {
					// Retry with new circuit
					s.rwmutex.Lock()
					s.chunks[id].circuit = seq
					seq++
					s.rwmutex.Unlock()

					client, req := s.chunkInit(id)
					go s.chunkFetch(id, client, req)
				} else {
					// Too many retries, redistribute work to the chunk with most remaining data
					s.rwmutex.Lock()
					bestIdx := -1
					maxLength := int64(0)

					for i := 0; i < s.circuits; i++ {
						if i != id && s.chunks[i].length > maxLength {
							maxLength = s.chunks[i].length
							bestIdx = i
						}
					}

					if bestIdx >= 0 {
						s.chunks[bestIdx].length += s.chunks[id].length
						s.chunks[id].length = 0
					}
					s.rwmutex.Unlock()
				}
			} else {
				// Chunk completed successfully
				atomic.AddInt64(&s.completedChunks, 1)

				// Check if all chunks are completed and find longest remaining chunk
				s.rwmutex.RLock()
				allDone := true
				targetIdx := -1
				maxLength := int64(0)

				for i := 0; i < s.circuits; i++ {
					if s.chunks[i].length > 0 {
						allDone = false
						if s.chunks[i].length > maxLength {
							maxLength = s.chunks[i].length
							targetIdx = i
						}
					}
				}
				s.rwmutex.RUnlock()

				if allDone {
					// All done!
					howLong := time.Since(startTime)
					averageSpeed := humanReadableSize(float32(s.bytesTotal) / float32(howLong.Seconds()))

					// Clear progress
					s.printTemporary(strings.Repeat(" ", 50))
					s.printPermanent(fmt.Sprintf("Download completed in:\t%s (%s/s)",
						howLong.Round(time.Second), averageSpeed))

					if !quiet && !silent {
						stop_status <- true
					}
					return 0
				}

				// Split work with the longest remaining chunk
				if targetIdx >= 0 && maxLength > s.minChunkSize*2 {
					s.rwmutex.Lock()
					splitSize := maxLength / 2
					s.chunks[id].length = splitSize
					s.chunks[targetIdx].length = maxLength - splitSize
					s.chunks[id].start = s.chunks[targetIdx].start + s.chunks[targetIdx].length
					s.chunks[id].circuit = seq
					s.chunks[id].retries = 0
					seq++
					s.rwmutex.Unlock()

					client, req := s.chunkInit(id)
					go s.chunkFetch(id, client, req)
				}
			}
		}
	}
}

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

var errorWriter io.Writer
var messageWriter io.Writer

func init() {
	// Set up CLI arguments
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

	// Custom usage message to avoid duplicate entries for long & short flags
	flag.Usage = func() {
		w := flag.CommandLine.Output()
		msg := `tor-dl - fast large file downloader over locally installed Tor
Copyright © 2025 Bryan Cuneo <https://github.com/BryanCuneo/tor-dl/>
Licensed under GNU GPL version 3 <https://www.gnu.org/licenses/>
Based on torget by Michał Trojnara <https://github.com/mtrojnar/torget>

Usage: tor-dl [FLAGS] {file.txt | URL [URL2...]}
  -allow-http bool
        Allow tor-dl to download files over HTTP instead of HTTPS. Not recommended!
  -circuits, -c int
        Concurrent circuits. (default 20)
  -destination, -d string
        Output directory. (default current directory)
  -force bool
        Will create parent folder(s) and/or overwrite existing files.
  -min-lifetime, -l int
        Minimum circuit lifetime (seconds). (default 10)
  -name, -n string
        Output filename. (default filename from URL)
  -quiet, -q bool
        Suppress most text output (still show errors).
  -silent, -s bool
        Suppress all text output (including errors).
  -tor-port, -p int
        Port your Tor service is listening on. (default 9050)
  -verbose, -v
        Show diagnostic details.`

		fmt.Fprintln(w, msg)
	}
}

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(0)
	}

	// If the -quiet or -silent argument was used, don't print non-error text
	if quiet || silent {
		messageWriter = io.Discard

		// if -silent argument was used, also don't print errors
		if silent {
			errorWriter = io.Discard
		} else {
			errorWriter = os.Stderr
		}
	} else {
		messageWriter = os.Stdout
		errorWriter = os.Stderr
	}

	var uris []string

	if flag.NArg() == 1 {
		// Only one non-flag argument. Check if it's a URL or a text file
		if _, err := os.Stat(flag.Arg(0)); err == nil {
			// Found a file on disk, read URLs from it
			file, err := os.Open(flag.Arg(0))
			if err != nil {
				fmt.Fprintf(errorWriter, "ERROR: argument \"%s\" is not a valid URL or file.\n%v\n", flag.Arg(0), err)
				os.Exit(1)
			}
			defer file.Close()

			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := scanner.Text()
				// Filter out blank lines
				if line != "" {
					uris = append(uris, line)
				}
			}
		} else {
			// No file found on disk, treating argument as URL
			uris = append(uris, flag.Arg(0))
		}
	} else {
		// Multiple URLs passed as non-flag arguments
		uris = flag.Args()
	}

	if len(uris) > 1 {
		fmt.Fprintf(messageWriter, "Downloading %d files.\n", len(uris))

		// Ignore the -name argument when multiple files are provided, just use the URL's filename
		if name != "" {
			fmt.Fprintln(messageWriter, "WARNING: The -name argument is not usable when multiple URLs are provided. Ignoring.")
			name = ""
		}
	} else if len(uris) < 1 {
		fmt.Fprintln(errorWriter, "ERROR: No URLs found.")
		os.Exit(1)
	}

	bkgr := context.Background()

	// Iterate over each URL passed as an argument and download the file
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
			fmt.Fprintf(errorWriter, "ERROR: \"%s\" is not using HTTPS.\n\tIf you absolutely must use HTTP, use the -allow-http flag. This is dangerous and not recommended!\n", uri)
			continue
		}

		ctx := context.WithoutCancel(bkgr)
		state := NewState(ctx)
		state.Fetch(uri)
	}
}
