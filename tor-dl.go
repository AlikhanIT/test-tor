// tor-dl.go
package main

/*
   tor-dl - fast large file downloader over locally installed Tor
   Segmented downloader with:
     - Redirect-safe headers
     - Exponential backoff + jitter
     - Global RPS limiter
     - Proactive tail-burst sharding (быстрый хвост)
     - Immediate circuit rotate on 403/429
     - Aggressive split on 403/timeout
     - Multi-port Tor fanout
     - Shuffled segments (better balance)
     - Time/percent logger
     - Clean shutdown when done (no 100% spam)  <-- fixed
   GPLv3
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
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ===== CLI flags =====
var (
	allowHttp      bool
	circuits       int
	destination    string
	force          bool
	minLifetime    int
	name           string
	quiet          bool
	silent         bool
	verbose        bool
	cliSegmentSize int64
	cliMaxRetries  int

	userAgent     string
	referer       string
	torPortsCSV   string // comma-separated list; if empty, use -p
	torPortSingle int    // -p / -tor-port (compat)
	rps           int

	tailThresholdB int64
	tailMaxWorkers int
	retryBaseMs    int

	// tail sharding
	tailShardMin int64 // минимальный размер шардов в хвосте
	tailShardMax int64 // верхняя граница шардов

	errorWriter   io.Writer
	messageWriter io.Writer
)

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

// ===== rate limiter =====
type rateLimiter struct {
	tokens chan struct{}
	stop   chan struct{}
}

func newRateLimiter(rps int) *rateLimiter {
	if rps < 1 {
		rps = 1
	}
	rl := &rateLimiter{
		tokens: make(chan struct{}, rps),
		stop:   make(chan struct{}),
	}
	interval := time.Second / time.Duration(rps)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-rl.stop:
				return
			case <-t.C:
				select {
				case rl.tokens <- struct{}{}:
				default:
				}
			}
		}
	}()
	return rl
}
func (rl *rateLimiter) take(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-rl.tokens:
		return true
	}
}
func (rl *rateLimiter) close() { close(rl.stop) }

// HTTP client через SOCKS5 (Tor) без внешних зависимостей
func httpClient(user string, port int) *http.Client {
	socksAddr := fmt.Sprintf("127.0.0.1:%d", port)
	base := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	dialCtx := socks5DialContext(socksAddr, user, user, base)

	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		Proxy:               nil, // используем SOCKS5 через DialContext
		DialContext:         dialCtx,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 128,
		IdleConnTimeout:     45 * time.Second,
		DisableKeepAlives:   false,
		ForceAttemptHTTP2:   false, // HTTP/1.1 для Range
		DisableCompression:  true,
	}
	cli := &http.Client{
		Transport: tr,
		Jar:       jar,
		Timeout:   120 * time.Second,
	}
	cli.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) == 0 {
			return nil
		}
		last := via[len(via)-1]
		for k, vv := range last.Header {
			if _, ok := req.Header[k]; !ok {
				for _, v := range vv {
					req.Header.Add(k, v)
				}
			}
		}
		return nil
	}
	return cli
}

// Минимальный SOCKS5 DialContext с поддержкой no-auth и username/password (RFC1929)
func socks5DialContext(socksAddr, user, pass string, base *net.Dialer) func(ctx context.Context, network, targetAddr string) (net.Conn, error) {
	return func(ctx context.Context, network, targetAddr string) (net.Conn, error) {
		// соединяемся с SOCKS5
		conn, err := base.DialContext(ctx, "tcp", socksAddr)
		if err != nil {
			return nil, fmt.Errorf("socks5 connect: %w", err)
		}
		// уважаем дедлайн из контекста на время рукопожатия
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
			defer conn.SetDeadline(time.Time{})
		}

		// greeting: предлагаем no-auth (0x00) и user/pass (0x02)
		g := []byte{0x05, 0x02, 0x00, 0x02}
		if _, err := conn.Write(g); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 greeting write: %w", err)
		}
		reply := make([]byte, 2)
		if _, err := io.ReadFull(conn, reply); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 greeting read: %w", err)
		}
		if reply[0] != 0x05 {
			conn.Close()
			return nil, fmt.Errorf("socks5 bad version: %d", reply[0])
		}
		method := reply[1]
		switch method {
		case 0x00: // no auth
			// ничего не делаем
		case 0x02: // username/password
			u := []byte(user)
			p := []byte(pass)
			if len(u) > 255 || len(p) > 255 {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth: username/password too long")
			}
			auth := make([]byte, 0, 3+len(u)+len(p))
			auth = append(auth, 0x01, byte(len(u)))
			auth = append(auth, u...)
			auth = append(auth, byte(len(p)))
			auth = append(auth, p...)
			if _, err := conn.Write(auth); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth write: %w", err)
			}
			ar := make([]byte, 2)
			if _, err := io.ReadFull(conn, ar); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth read: %w", err)
			}
			if ar[1] != 0x00 {
				conn.Close()
				return nil, fmt.Errorf("socks5 auth failed (code %d)", ar[1])
			}
		default:
			conn.Close()
			return nil, fmt.Errorf("socks5: no acceptable auth (0x%02x)", method)
		}

		// формируем CONNECT-запрос
		host, portStr, err := net.SplitHostPort(targetAddr)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("split host/port: %w", err)
		}
		nport, err := strconv.Atoi(portStr)
		if err != nil || nport < 0 || nport > 65535 {
			conn.Close()
			return nil, fmt.Errorf("bad port: %s", portStr)
		}
		req := []byte{0x05, 0x01, 0x00} // VER=5, CMD=CONNECT, RSV=0

		if ip := net.ParseIP(host); ip != nil {
			if v4 := ip.To4(); v4 != nil {
				req = append(req, 0x01) // IPv4
				req = append(req, v4...)
			} else {
				req = append(req, 0x04) // IPv6
				req = append(req, ip.To16()...)
			}
		} else {
			if len(host) > 255 {
				conn.Close()
				return nil, fmt.Errorf("domain name too long")
			}
			req = append(req, 0x03, byte(len(host))) // DOMAIN
			req = append(req, []byte(host)...)
		}
		req = append(req, byte(nport>>8), byte(nport&0xff))

		if _, err := conn.Write(req); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect write: %w", err)
		}

		// читаем ответ: VER REP RSV ATYP [BND.ADDR] [BND.PORT]
		hdr := make([]byte, 4)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect read hdr: %w", err)
		}
		if hdr[0] != 0x05 {
			conn.Close()
			return nil, fmt.Errorf("socks5 bad reply version: %d", hdr[0])
		}
		if hdr[1] != 0x00 {
			conn.Close()
			return nil, fmt.Errorf("socks5 connect failed (REP=0x%02x)", hdr[1])
		}
		var toRead int
		switch hdr[3] {
		case 0x01:
			toRead = 4
		case 0x04:
			toRead = 16
		case 0x03:
			lb := make([]byte, 1)
			if _, err := io.ReadFull(conn, lb); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 read domain len: %w", err)
			}
			toRead = int(lb[0])
		default:
			conn.Close()
			return nil, fmt.Errorf("socks5 bad ATYP: 0x%02x", hdr[3])
		}
		// пропускаем адрес + порт
		if toRead > 0 {
			dummy := make([]byte, toRead)
			if _, err := io.ReadFull(conn, dummy); err != nil {
				conn.Close()
				return nil, fmt.Errorf("socks5 read bnd.addr: %w", err)
			}
		}
		dport := make([]byte, 2)
		if _, err := io.ReadFull(conn, dport); err != nil {
			conn.Close()
			return nil, fmt.Errorf("socks5 read bnd.port: %w", err)
		}

		// успех
		return conn, nil
	}
}

// ===== segments & queue =====
type segment struct {
	start   int64
	length  int64
	attempt int
}

type workQueue struct {
	ch     chan *segment
	mu     sync.Mutex
	closed bool
	ctx    context.Context
}

func newQueue(capacity int, ctx context.Context) *workQueue {
	return &workQueue{ch: make(chan *segment, capacity), ctx: ctx}
}
func (q *workQueue) push(s *segment) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	defer func() { _ = recover() }() // на случай race с close()
	q.ch <- s
}
func (q *workQueue) pop() (*segment, bool) {
	s, ok := <-q.ch
	return s, ok
}
func (q *workQueue) tryPop() (*segment, bool) { // неблокирующий pop для хвост-планировщика
	select {
	case s, ok := <-q.ch:
		if !ok {
			return nil, false
		}
		return s, true
	default:
		return nil, false
	}
}
func (q *workQueue) pushDelayed(s *segment, d time.Duration) {
	go func() {
		select {
		case <-q.ctx.Done():
			return
		case <-time.After(d):
			q.push(s)
		}
	}()
}
func (q *workQueue) len() int {
	return len(q.ch)
}
func (q *workQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}

// ===== errors =====
type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("http %d", e.code) }

// ===== State =====
type State struct {
	ctx    context.Context
	cancel context.CancelFunc

	src        string
	output     string
	totalSize  int64
	wroteBytes int64

	acceptRanges bool

	circuits    int
	minLifetime time.Duration
	verbose     bool

	prevBytes int64
	log       chan string
	terminal  bool

	queue      *workQueue
	segSize    int64
	minSegSize int64
	maxRetries int

	ua  string
	ref string

	tailThreshold int64
	tailWorkers   int
	activeTail    int32

	retryBase time.Duration

	ports []int
	rl    *rateLimiter

	startTime time.Time

	inFlight   int32
	finishOnce sync.Once
}

// ===== CLI init =====
func init() {
	flag.BoolVar(&allowHttp, "allow-http", false, "Allow HTTP (insecure).")
	flag.IntVar(&circuits, "circuits", 20, "Concurrent circuits.")
	flag.IntVar(&circuits, "c", 20, "Concurrent circuits.")
	flag.StringVar(&destination, "destination", ".", "Output directory.")
	flag.StringVar(&destination, "d", ".", "Output directory.")
	flag.BoolVar(&force, "force", false, "Create parent folders / overwrite output.")
	flag.IntVar(&minLifetime, "min-lifetime", 10, "Minimum circuit lifetime, seconds.")
	flag.IntVar(&minLifetime, "l", 10, "Minimum circuit lifetime, seconds.")
	flag.StringVar(&name, "name", "", "Output filename.")
	flag.StringVar(&name, "n", "", "Output filename.")
	flag.BoolVar(&quiet, "quiet", false, "Mute most output.")
	flag.BoolVar(&quiet, "q", false, "Mute most output.")
	flag.BoolVar(&silent, "silent", false, "Mute all output (incl. errors).")
	flag.BoolVar(&silent, "s", false, "Mute all output (incl. errors).")
	flag.BoolVar(&verbose, "verbose", false, "Verbose logs.")
	flag.BoolVar(&verbose, "v", false, "Verbose logs.")

	flag.Int64Var(&cliSegmentSize, "segment-size", 2*1024*1024, "Initial segment size, bytes (default 2MiB).")
	flag.IntVar(&cliMaxRetries, "max-retries", 5, "Max retries before split or requeue.")

	flag.StringVar(&userAgent, "user-agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
		"HTTP User-Agent header.")
	flag.StringVar(&referer, "referer", "https://www.youtube.com/", "HTTP Referer header.")

	// ports
	flag.StringVar(&torPortsCSV, "ports", "", "Comma-separated Tor SOCKS ports, e.g. \"9050,9150\".")
	flag.IntVar(&torPortSingle, "tor-port", 9050, "Single Tor SOCKS port (compat).")
	flag.IntVar(&torPortSingle, "p", 9050, "Single Tor SOCKS port (compat, short).")

	flag.IntVar(&rps, "rps", 8, "Global requests-per-second limit.")
	flag.Int64Var(&tailThresholdB, "tail-threshold", 32*1024*1024, "Tail-mode threshold, bytes.")
	flag.IntVar(&tailMaxWorkers, "tail-workers", 4, "Active workers allowed in tail-mode (upper bound).")
	flag.IntVar(&retryBaseMs, "retry-base-ms", 250, "Base backoff (ms).")

	flag.Int64Var(&tailShardMin, "tail-shard-min", 256*1024, "Minimal shard size in tail-mode.")
	flag.Int64Var(&tailShardMax, "tail-shard-max", 2*1024*1024, "Max shard size in tail-mode.")

	flag.Usage = func() {
		w := flag.CommandLine.Output()
		fmt.Fprintln(w, `tor-dl - fast downloader over Tor (GPLv3)
Usage: tor-dl [FLAGS] {file.txt | URL [URL2...]}
  -c, -circuits N          concurrent workers (default 20)
  -ports "9050,9150"       list of Tor SOCKS ports
  -p, -tor-port N          single Tor SOCKS port (default 9050)
  -rps N                   global requests/sec limit (default 8)
  -segment-size BYTES      initial segment size (default 2MiB)
  -max-retries N           retries before split/requeue (default 5)
  -tail-threshold BYTES    when remaining <= threshold, enter tail-mode
  -tail-workers N          parallel workers in tail-mode (default 4; autoscaled up to -c)
  -tail-shard-min BYTES    min shard size in tail (default 256KiB)
  -tail-shard-max BYTES    max shard size in tail (default 2MiB)
  -user-agent STR          UA header
  -referer URL             Referer header
  -v, -verbose             verbose diagnostics
  -q, -quiet / -s, -silent mutes output`)
	}
}

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(0)
	}
	// writers
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

	// collect URLs (file or args)
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
	rand.Seed(time.Now().UnixNano())

	// parse ports
	var ports []int
	if strings.TrimSpace(torPortsCSV) != "" {
		for _, p := range strings.Split(torPortsCSV, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				ports = append(ports, n)
			}
		}
	}
	if len(ports) == 0 {
		ports = []int{torPortSingle}
	}

	ctx := context.Background()
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
		state := newState(ctx, ports)
		exitCode := state.Fetch(uri)
		state.close()
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}
}

func newState(ctx context.Context, ports []int) *State {
	st, _ := os.Stdout.Stat()
	terminal := st.Mode()&os.ModeCharDevice == os.ModeCharDevice

	if tailThresholdB < 1 {
		tailThresholdB = 32 * 1024 * 1024
	}
	if tailMaxWorkers < 1 {
		tailMaxWorkers = 1
	}
	if retryBaseMs < 50 {
		retryBaseMs = 50
	}
	if rps < 1 {
		rps = 1
	}
	if tailShardMin < 64*1024 {
		tailShardMin = 64 * 1024
	}
	if tailShardMax < tailShardMin {
		tailShardMax = tailShardMin
	}

	cctx, cancel := context.WithCancel(ctx)
	s := &State{
		ctx:           cctx,
		cancel:        cancel,
		circuits:      circuits,
		minLifetime:   time.Duration(minLifetime) * time.Second,
		verbose:       verbose,
		log:           make(chan string, 2048),
		terminal:      terminal,
		minSegSize:    256 * 1024,
		segSize:       cliSegmentSize,
		maxRetries:    cliMaxRetries,
		ua:            userAgent,
		ref:           referer,
		tailThreshold: tailThresholdB,
		tailWorkers:   tailMaxWorkers,
		retryBase:     time.Duration(retryBaseMs) * time.Millisecond,
		ports:         ports,
		rl:            newRateLimiter(rps),
		startTime:     time.Now(),
	}
	if s.segSize < s.minSegSize {
		s.segSize = s.minSegSize
	}
	return s
}
func (s *State) close() {
	if s.rl != nil {
		s.rl.close()
	}
}

func (s *State) printPermanent(txt string) {
	if s.terminal {
		fmt.Fprintf(messageWriter, "\r%-80s\n", txt)
	} else {
		fmt.Fprintln(messageWriter, txt)
	}
}
func (s *State) stampf(format string, args ...any) {
	now := time.Now().Format("15:04:05")
	s.printPermanent("[" + now + "] " + fmt.Sprintf(format, args...))
}

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

// ---- HEAD + fallback probe ----
func (s *State) headInfo(client *http.Client) (ok bool, contentLength int64, acceptRanges bool) {
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodHead, s.src, nil)
	s.setCommonHeaders(req.Header)
	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.ContentLength > 0 {
			ar := strings.ToLower(resp.Header.Get("Accept-Ranges"))
			return true, resp.ContentLength, strings.Contains(ar, "bytes")
		}
	}
	return s.probeLengthViaRange(client)
}
func (s *State) probeLengthViaRange(client *http.Client) (bool, int64, bool) {
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodGet, s.src, nil)
	s.setCommonHeaders(req.Header)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(errorWriter, "ERROR - probe GET Range 0-0 failed: %v\n", err)
		return false, 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		fmt.Fprintf(errorWriter, "ERROR - probe expected 206, got %d\n", resp.StatusCode)
		return false, 0, false
	}
	cr := resp.Header.Get("Content-Range") // "bytes 0-0/12345"
	total := int64(0)
	if parts := strings.Split(cr, "/"); len(parts) == 2 {
		t := strings.TrimSpace(parts[1])
		if n, err := strconv.ParseInt(t, 10, 64); err == nil {
			total = n
		}
	}
	if total <= 0 {
		return false, 0, false
	}
	return true, total, true
}

func (s *State) Fetch(src string) int {
	s.src = src
	s.getOutputFilepath()

	if _, err := os.Stat(s.output); err == nil && !force {
		fmt.Fprintf(errorWriter, "ERROR: \"%s\" already exists. Use -force to overwrite.\n", s.output)
		return 1
	}
	fmt.Fprintf(messageWriter, "Output file:\t\t%s\n", s.output)

	headCli := httpClient("tordl", s.ports[0])
	ok, cl, acceptRanges := s.headInfo(headCli)
	if !ok || cl <= 0 {
		return 1
	}
	s.totalSize = cl
	s.acceptRanges = acceptRanges
	fmt.Fprintf(messageWriter, "Download filesize:\t%s\n", humanReadableSize(float64(s.totalSize)))

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

	s.startTime = time.Now()

	// Если сервер не поддерживает Range или мало воркеров — одиночный поток
	if !s.acceptRanges || s.circuits < 2 {
		fmt.Fprintln(messageWriter, "NOTE: single stream mode.")
		code := s.singleStream()
		if code == 0 {
			howLong := time.Since(s.startTime)
			avg := float64(s.totalSize) / howLong.Seconds()
			s.printPermanent(fmt.Sprintf("Download completed in:\t%s (%s/s)", howLong.Round(time.Second), humanReadableSize(avg)))
		}
		return code
	}

	// segmented
	seg := s.segSize
	if seg < s.minSegSize {
		seg = s.minSegSize
	}
	nSegments := (s.totalSize + seg - 1) / seg
	minDesired := int64(s.circuits * 6)
	if nSegments < minDesired {
		newSeg := s.totalSize / minDesired
		if s.totalSize%minDesired != 0 {
			newSeg++
		}
		if newSeg < s.minSegSize {
			newSeg = s.minSegSize
		}
		seg = newSeg
		nSegments = (s.totalSize + seg - 1) / seg
	}
	s.segSize = seg

	maxQueueCap := 1_000_000
	queueCap64 := nSegments + int64(s.circuits*2)
	if queueCap64 > int64(maxQueueCap) {
		queueCap64 = int64(maxQueueCap)
	}
	s.queue = newQueue(int(queueCap64), s.ctx)
	s.fillInitialSegments()

	stopStatus := make(chan bool, 1)
	go s.progressLoop(stopStatus)

	var wg sync.WaitGroup
	wg.Add(s.circuits)
	for i := 0; i < s.circuits; i++ {
		go func(workerID int) {
			defer wg.Done()
			s.worker(workerID)
		}(i)
	}
	wg.Wait()
	stopStatus <- true

	if atomic.LoadInt64(&s.wroteBytes) < s.totalSize {
		missed := s.totalSize - atomic.LoadInt64(&s.wroteBytes)
		fmt.Fprintf(errorWriter, "ERROR: Incomplete download, missing %d bytes.\n", missed)
		return 1
	}
	howLong := time.Since(s.startTime)
	avg := float64(s.totalSize) / howLong.Seconds()
	s.printPermanent(fmt.Sprintf("Download completed in:\t%s (%s/s)", howLong.Round(time.Second), humanReadableSize(avg)))
	return 0
}

func (s *State) singleStream() int {
	client := httpClient("tg0", s.ports[0])
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodGet, s.src, nil)
	s.setCommonHeaders(req.Header)
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

	// гарантируем, что получили весь файл
	if written < s.totalSize {
		fmt.Fprintf(errorWriter, "ERROR - short read: got %d of %d bytes\n", written, s.totalSize)
		return 1
	}

	s.addProgress(written)
	return 0
}

func (s *State) fillInitialSegments() {
	seg := s.segSize
	if seg < s.minSegSize {
		seg = s.minSegSize
	}
	var segs []*segment
	for off := int64(0); off < s.totalSize; off += seg {
		length := seg
		if rem := s.totalSize - off; rem < length {
			length = rem
		}
		segs = append(segs, &segment{start: off, length: length})
	}
	rand.Shuffle(len(segs), func(i, j int) { segs[i], segs[j] = segs[j], segs[i] })
	for _, sg := range segs {
		s.queue.push(sg)
	}
}

func (s *State) progressLoop(stop <-chan bool) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			// если уже всё собрано — мгновенно завершаемся, без лишних «100%»
			if atomic.LoadInt64(&s.wroteBytes) >= s.totalSize {
				s.maybeFinish()
				return
			}
			s.progressTick()
			s.tailPlannerTick()
			s.maybeFinish()
		}
	}
}

func (s *State) progressTick() {
	curr := atomic.LoadInt64(&s.wroteBytes)
	if curr > s.totalSize {
		curr = s.totalSize
	}
	diff := curr - s.prevBytes

	elapsed := time.Since(s.startTime).Round(time.Second)
	percent := 100 * float64(curr) / float64(s.totalSize)
	avg := float64(curr) / (float64(elapsed) / float64(time.Second))

	if diff <= 0 {
		s.stampf("%3.0f%%  +1s (total %s)  %s / %s, avg %s/s",
			math.Floor(percent),
			elapsed,
			humanReadableSize(float64(curr)),
			humanReadableSize(float64(s.totalSize)),
			humanReadableSize(avg),
		)
	} else {
		s.stampf("%3.0f%%  +1s (total %s)  %s / %s, avg %s/s",
			math.Floor(percent),
			elapsed,
			humanReadableSize(float64(curr)),
			humanReadableSize(float64(s.totalSize)),
			humanReadableSize(avg),
		)
	}

	if s.verbose {
		s.flushLogs()
	} else {
		s.drainLogs()
	}
	s.prevBytes = curr
}

// планировщик хвоста — всегда держим достаточно шардов и поднимаем tail-concurrency
func (s *State) tailPlannerTick() {
	remaining := s.totalSize - atomic.LoadInt64(&s.wroteBytes)
	if remaining <= 0 || remaining > s.tailThreshold {
		return
	}
	// поднимем конкуренцию хвоста до -c
	if s.tailWorkers < s.circuits {
		s.tailWorkers = s.circuits
	}
	// если в очереди мало задач — насыпем шардов
	want := s.tailWorkers * 2
	if s.queue.len() >= want {
		return
	}
	shard := s.segSize / 2
	if shard < tailShardMin {
		shard = tailShardMin
	}
	if shard > tailShardMax {
		shard = tailShardMax
	}
	// попробуем взять один крупный сегмент и порезать
	if seg, ok := s.queue.tryPop(); ok && seg != nil {
		if seg.length > shard*2 {
			chunks := splitSegmentInto(seg.start, seg.length, shard)
			rand.Shuffle(len(chunks), func(i, j int) { chunks[i], chunks[j] = chunks[j], chunks[i] })
			for _, c := range chunks {
				s.queue.push(c)
			}
			if s.verbose {
				s.log <- fmt.Sprintf("Tail-burst: split %d bytes into %d shards of ~%d", seg.length, len(chunks), shard)
			}
			return
		}
		// маленький — вернём как есть
		s.queue.push(seg)
		return
	}
	// если в очереди совсем пусто — засеем оставшийся хвост «как есть»
	start := s.totalSize - remaining
	chunks := splitSegmentInto(start, remaining, shard)
	rand.Shuffle(len(chunks), func(i, j int) { chunks[i], chunks[j] = chunks[j], chunks[i] })
	for _, c := range chunks {
		s.queue.push(c)
	}
	if s.verbose {
		s.log <- fmt.Sprintf("Tail-burst: seeded %d shards for remaining %d", len(chunks), remaining)
	}
}

func splitSegmentInto(start, length, shard int64) []*segment {
	var res []*segment
	for off := int64(0); off < length; off += shard {
		l := shard
		if rem := length - off; rem < l {
			l = rem
		}
		res = append(res, &segment{start: start + off, length: l})
	}
	return res
}

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
func (s *State) drainLogs() {
	for len(s.log) > 0 {
		<-s.log
	}
}

// ===== graceful finish =====
func (s *State) maybeFinish() {
	if atomic.LoadInt64(&s.wroteBytes) >= s.totalSize {
		s.finishOnce.Do(func() {
			if s.cancel != nil {
				s.cancel() // отменяем все отложенные пуши и текущие запросы
			}
			if s.queue != nil {
				s.queue.close() // закрываем очередь, воркеры сразу выйдут из pop()
			}
		})
	}
}

// ===== progress helper (clamped) =====
func (s *State) addProgress(n int64) {
	if n <= 0 {
		return
	}
	for {
		prev := atomic.LoadInt64(&s.wroteBytes)
		if prev >= s.totalSize {
			return
		}
		add := n
		if prev+add > s.totalSize {
			add = s.totalSize - prev
			if add < 0 {
				add = 0
			}
		}
		if add == 0 {
			return
		}
		if atomic.CompareAndSwapInt64(&s.wroteBytes, prev, prev+add) {
			return
		}
	}
}

// ===== Workers =====
func (s *State) worker(id int) {
	port := s.ports[id%len(s.ports)]
	username := fmt.Sprintf("w%d_%d", id, time.Now().UnixNano())
	client := httpClient(username, port)
	lastRotate := time.Now()

	for {
		// быстрый выход, если уже всё скачано
		if atomic.LoadInt64(&s.wroteBytes) >= s.totalSize {
			return
		}

		seg, ok := s.queue.pop()
		if !ok || seg == nil {
			return
		}

		// ещё одна защита: если вдруг между pop и началом работы мы уже добили 100%
		if atomic.LoadInt64(&s.wroteBytes) >= s.totalSize {
			return
		}

		atomic.AddInt32(&s.inFlight, 1)
		func() {
			defer func() {
				atomic.AddInt32(&s.inFlight, -1)
				s.maybeFinish()
			}()

			// ротация цепочки по времени
			if time.Since(lastRotate) >= s.minLifetime {
				username = fmt.Sprintf("w%d_%d", id, time.Now().UnixNano())
				client = httpClient(username, port)
				lastRotate = time.Now()
			}

			// если уже добили 100% — не делаем запрос
			if atomic.LoadInt64(&s.wroteBytes) >= s.totalSize || s.ctx.Err() != nil {
				return
			}

			err := s.fetchSegment(client, seg)
			// ... остальное без изменений ...
			if err != nil {
				if hs, ok := err.(*httpStatusError); ok && (hs.code == 403 || hs.code == 429) {
					username = fmt.Sprintf("w%d_%d", id, time.Now().UnixNano())
					client = httpClient(username, port)
					if seg.length > s.minSegSize*2 {
						shard := seg.length / 2
						if shard < s.minSegSize {
							shard = s.minSegSize
						}
						left := &segment{start: seg.start, length: shard}
						right := &segment{start: seg.start + shard, length: seg.length - shard}
						s.queue.pushDelayed(left, s.retryBase/2)
						s.queue.pushDelayed(right, s.retryBase/2)
						if s.verbose {
							s.log <- fmt.Sprintf("Split-on-403: %d into %d + %d", seg.length, left.length, right.length)
						}
						return
					}
				}
				delay := s.backoffFor(seg.attempt, err)
				seg.attempt++
				// не ре-кьюим, если уже 100% или контекст отменён
				if atomic.LoadInt64(&s.wroteBytes) >= s.totalSize || s.ctx.Err() != nil {
					return
				}
				if seg.attempt > s.maxRetries && seg.length > s.minSegSize*2 {
					half := seg.length / 2
					left := &segment{start: seg.start, length: half}
					right := &segment{start: seg.start + half, length: seg.length - half}
					s.queue.pushDelayed(left, delay/2)
					s.queue.pushDelayed(right, delay/2)
				} else {
					s.queue.pushDelayed(seg, delay)
				}
				return
			}
		}()
	}
}

func (s *State) backoffFor(attempt int, err error) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := s.retryBase
	mult := 1.0
	if err != nil {
		if hs, ok := err.(*httpStatusError); ok {
			if hs.code == 403 || hs.code == 429 {
				mult = 1.2 // мягче, мы ещё и сплитим
			} else if hs.code >= 500 && hs.code < 600 {
				mult = 1.5
			}
		}
	}
	delay := time.Duration(float64(base) * math.Pow(2, float64(attempt)) * mult)
	if delay > 2500*time.Millisecond {
		delay = 2500 * time.Millisecond
	}
	j := float64(delay) * (0.75 + rand.Float64()*0.5)
	return time.Duration(j)
}

func (s *State) setCommonHeaders(h http.Header) {
	h.Set("User-Agent", s.ua)
	if s.ref != "" {
		h.Set("Referer", s.ref)
	}
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "en-US,en;q=0.9,ru;q=0.8")
	h.Set("Origin", "https://www.youtube.com")
	h.Set("Connection", "keep-alive")
}

func (s *State) fetchSegment(client *http.Client, seg *segment) error {
	if s.rl != nil && !s.rl.take(s.ctx) {
		return fmt.Errorf("rps limiter closed")
	}
	// быстрая отмена
	if s.ctx.Err() != nil || atomic.LoadInt64(&s.wroteBytes) >= s.totalSize {
		return nil
	}

	rangeHeader := fmt.Sprintf("bytes=%d-%d", seg.start, seg.start+seg.length-1)
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodGet, s.src, nil)
	s.setCommonHeaders(req.Header)
	req.Header.Set("Range", rangeHeader)

	resp, err := client.Do(req)
	if err != nil {
		if s.verbose {
			s.log <- fmt.Sprintf("ClientDo err (%s): %v", rangeHeader, err)
		}
		return err
	}
	if resp.Body == nil {
		return fmt.Errorf("no body")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		if s.verbose {
			s.log <- fmt.Sprintf("Unexpected HTTP status %d for %s", resp.StatusCode, rangeHeader)
		}
		return &httpStatusError{code: resp.StatusCode}
	}

	file, err := os.OpenFile(s.output, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(seg.start, io.SeekStart); err != nil {
		return err
	}

	buf := make([]byte, 256*1024)
	lim := io.LimitReader(resp.Body, seg.length)
	w, err := io.CopyBuffer(file, lim, buf)
	if err != nil && !errors.Is(err, io.EOF) {
		if s.verbose {
			s.log <- fmt.Sprintf("copy err (%s): %v", rangeHeader, err)
		}
		if w > 0 && w < seg.length {
			remain := seg.length - w
			s.addProgress(w)
			// дозапланируем остаток только если ещё не добили 100%
			if atomic.LoadInt64(&s.wroteBytes) < s.totalSize && s.ctx.Err() == nil {
				s.queue.push(&segment{start: seg.start + w, length: remain, attempt: seg.attempt})
			}
			return nil
		}
		return err
	}

	if w < seg.length {
		remain := seg.length - w
		if w > 0 {
			s.addProgress(w)
		}
		if remain > 0 && atomic.LoadInt64(&s.wroteBytes) < s.totalSize && s.ctx.Err() == nil {
			s.queue.push(&segment{start: seg.start + w, length: remain, attempt: seg.attempt})
		}
		return nil
	}

	s.addProgress(w)
	return nil
}
