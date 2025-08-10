package main

/*
	tor-dl - fast large file downloader over locally installed Tor
	Optimized queue-based segmented downloader for uniform throughput.

	Copyright © 2025 Bryan Cuneo <https://github.com/BryanCuneo/tor-dl/>
	Based on torget by Michał Trojnara <https://github.com/mtrojnar/torget>

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
	torBlockDefault = 8192 // сетевой буфер ~8KiB, используем кратный этому в копировании
)

// ===== CLI flags (совместимо с твоей версией) =====
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

// Новые (опциональные)
var cliSegmentSize int64
var cliMaxRetries int

// ===== глобальные рендеры вывода =====
var errorWriter io.Writer
var messageWriter io.Writer

// ===== утилиты =====
func humanReadableSize(sizeInBytes float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for sizeInBytes >= 1024 && i < len(units)-1 {
		sizeInBytes /= 1024
		i++
	}
	return fmt.Sprintf("%.2f %s", sizeInBytes, units[i])
}

func httpClient(user string) *http.Client {
	// сохраняем твою логику socks5 через ProxyURL
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
		ForceAttemptHTTP2:   false, // HTTP/2 через SOCKS не нужен, лишние сложности на Range
	}
	return &http.Client{
		Transport: tr,
		Timeout:   60 * time.Second,
	}
}

// ===== сегменты и состояние =====
type segment struct {
	start   int64
	length  int64
	attempt int
}

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

type State struct {
	ctx        context.Context
	src        string
	output     string
	totalSize  int64
	wroteBytes int64

	acceptRanges bool

	// конфиг
	circuits    int
	minLifetime time.Duration
	verbose     bool

	// прогресс
	prevBytes int64

	// логи
	log chan string

	// терминал?
	terminal bool

	// очереди и упр.
	queue           *workQueue
	segSize         int64
	minSegSize      int64
	maxRetries      int
	rebalanceTicker *time.Ticker

	// синхронизация
	progressMu sync.Mutex
}

// ===== инициализация/CLI =====
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

	// новые — по умолчанию адаптивно, но можно задать руками
	flag.Int64Var(&cliSegmentSize, "segment-size", 2*1024*1024, "Initial segment size in bytes (adaptive).")
	flag.IntVar(&cliMaxRetries, "max-retries", 5, "Max retries per segment before giving it to another worker.")

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
  -segment-size         Initial segment size (bytes, default 2MiB, adaptive)
  -max-retries          Per-segment retries before giving up (default 5)`
		fmt.Fprintln(w, msg)
	}
}

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(0)
	}

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

// ===== State impl =====
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
		minSegSize:  256 * 1024, // меньше нет смысла
		segSize:     cliSegmentSize,
		maxRetries:  cliMaxRetries,
	}
	if s.segSize < s.minSegSize {
		s.segSize = s.minSegSize
	}
	return s
}

func (s *State) printPermanent(txt string) {
	if s.terminal {
		fmt.Fprintf(messageWriter, "\r%-80s\n", txt)
	} else {
		fmt.Fprintln(messageWriter, txt)
	}
}
func (s *State) printTemporary(txt string) {
	if s.terminal {
		fmt.Fprintf(messageWriter, "\r%-80s", txt)
	}
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

func (s *State) Fetch(src string) int {
	s.src = src
	s.getOutputFilepath()

	if _, err := os.Stat(s.output); err == nil && !force {
		fmt.Fprintf(errorWriter, "ERROR: \"%s\" already exists. Use -force to overwrite.\n", s.output)
		return 1
	}
	fmt.Fprintf(messageWriter, "Output file:\t\t%s\n", s.output)

	// HEAD через общий client
	headCli := httpClient("tordl")
	ok, cl, acceptRanges := s.headInfo(headCli)
	if !ok {
		return 1
	}
	s.totalSize = cl
	s.acceptRanges = acceptRanges
	fmt.Fprintf(messageWriter, "Download filesize:\t%s\n", humanReadableSize(float64(s.totalSize)))

	// Создаём/обрезаем файл и предвыделяем
	f, err := os.Create(s.output)
	if f != nil {
		_ = f.Close()
	}
	if err != nil {
		fmt.Fprintln(errorWriter, err.Error())
		return 1
	}
	if err := os.Truncate(s.output, s.totalSize); err != nil {
		// sparse тоже ок — всё равно пишем по оффсетам
		fmt.Fprintf(messageWriter, "WARNING: Truncate failed: %v\n", err)
	}

	startTime := time.Now()

	// Если сервер не поддерживает Range — качаем в один поток
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

	// Иначе — высокоуровневый план: бьём на маленькие сегменты и запускаем воркеры
	s.queue = newQueue(s.circuits * 16)
	s.fillInitialSegments()

	// статус/прогресс
	var stopStatus chan bool
	if !quiet && !silent {
		stopStatus = make(chan bool, 1)
		go s.progressLoop(stopStatus)
	}

	// стартуем воркеры
	var wg sync.WaitGroup
	wg.Add(s.circuits)
	for i := 0; i < s.circuits; i++ {
		go func(workerID int) {
			defer wg.Done()
			s.worker(workerID)
		}(i)
	}

	// пока воркеры работают, можем подбрасывать сегменты, если очередь пустеет
	go s.adaptiveFeeder()

	wg.Wait()
	if !quiet && !silent {
		stopStatus <- true
	}

	// проверка: всё ли скачано
	if atomic.LoadInt64(&s.wroteBytes) != s.totalSize {
		// доскачка не удалась
		missed := s.totalSize - atomic.LoadInt64(&s.wroteBytes)
		fmt.Fprintf(errorWriter, "ERROR: Incomplete download, missing %d bytes.\n", missed)
		return 1
	}

	howLong := time.Since(startTime)
	avg := float64(s.totalSize) / howLong.Seconds()
	// чистим строку прогресса
	s.printTemporary(strings.Repeat(" ", 80))
	s.printPermanent(fmt.Sprintf("Download completed in:\t%s (%s/s)", howLong.Round(time.Second), humanReadableSize(avg)))
	return 0
}

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

func (s *State) fillInitialSegments() {
	// гарантируем приличный запас работы, чтобы не было «хвоста»
	desired := s.circuits * 8
	if desired < 32 {
		desired = 32
	}
	seg := s.segSize
	if seg < s.minSegSize {
		seg = s.minSegSize
	}
	// если файл маленький — увеличим сегмент, чтобы количество задач не было слишком большим
	if s.totalSize/seg > int64(desired*4) {
		seg = s.totalSize / int64(desired*4)
		if seg < s.minSegSize {
			seg = s.minSegSize
		}
	}
	var offset int64 = 0
	for offset < s.totalSize {
		l := seg
		if offset+l > s.totalSize {
			l = s.totalSize - offset
		}
		s.queue.push(&segment{start: offset, length: l})
		offset += l
	}
}

func (s *State) adaptiveFeeder() {
	// следим за очередью: если она опустела из-за того, что сегменты слишком крупные,
	// подрежем будущие сегменты (на практике начальный пакет уже покрывает весь файл,
	// но этот поток оставлен для будущих улучшений/мульти-URL)
	// Здесь просто засыпаем — вся работа уже в очереди.
	for range time.Tick(5 * time.Second) {
		if s.queue == nil {
			return
		}
		// no-op: сегменты созданы на весь файл; при ошибках они перекидываются обратно.
	}
}

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
			eta := time.Duration(float64(remain)/speed) * time.Second
			etaStr = fmt.Sprintf("ETA %d:%02d:%02d",
				int(eta.Hours()), int(eta.Minutes())%60, int(eta.Seconds())%60)
		} else {
			etaStr = "ETA --:--:--"
		}
		msg = fmt.Sprintf("%s/s, %s", humanReadableSize(speed), etaStr)
	}
	status := fmt.Sprintf("%6.2f%% done, %s",
		100*float64(curr)/float64(s.totalSize), msg)
	if s.verbose {
		s.flushLogs()
	} else {
		s.drainLogs()
	}
	s.printTemporary(status)
	s.prevBytes = curr
}

func (s *State) flushLogs() {
	n := len(s.log)
	if n == 0 {
		return
	}
	logs := make([]string, 0, n)
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
			if cnt > 1 {
				prev = fmt.Sprintf("%s (%d times)", prev, cnt)
			}
			s.printPermanent(prev)
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

// ===== worker =====
func (s *State) worker(id int) {
	usernameBase := fmt.Sprintf("tg%d", id)
	client := httpClient(usernameBase)
	lastRotate := time.Now()

	for {
		seg, ok := s.queue.pop()
		if !ok {
			return
		}

		// ротация цепи по таймеру
		if time.Since(lastRotate) >= s.minLifetime {
			// Каждый раз добавляем «поколение» к имени — Tor даст новую цепь
			usernameBase = fmt.Sprintf("tg%d_%d", id, time.Now().UnixNano())
			client = httpClient(usernameBase)
			lastRotate = time.Now()
		}

		if err := s.fetchSegment(client, seg); err != nil {
			seg.attempt++
			if seg.attempt <= s.maxRetries {
				// бэк-офф минимальный — очередь и так сбалансирует
				s.queue.push(seg)
			} else {
				// после N попыток — попробуем порезать сегмент пополам,
				// это часто спасает на проблемных точках (узкие CDN куски)
				if seg.length > s.minSegSize*2 {
					left := &segment{start: seg.start, length: seg.length / 2}
					right := &segment{start: seg.start + seg.length/2, length: seg.length - seg.length/2}
					s.queue.push(left)
					s.queue.push(right)
				} else {
					// Совсем беда — последняя попытка ещё раз отправить как есть
					seg.attempt = 0
					s.queue.push(seg)
				}
			}
			continue
		}
	}
}

func (s *State) fetchSegment(client *http.Client, seg *segment) error {
	// создаём Range-запрос
	rangeHeader := fmt.Sprintf("bytes=%d-%d", seg.start, seg.start+seg.length-1)
	req, _ := http.NewRequestWithContext(s.ctx, http.MethodGet, s.src, nil)
	req.Header.Set("Range", rangeHeader)
	// Снижаем шанс «буферизации» где-то на прокси — просим закрывать соединение
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
		// сервер не уважил Range → не рискуем
		if s.verbose {
			s.log <- fmt.Sprintf("Unexpected HTTP status %d for %s", resp.StatusCode, rangeHeader)
		}
		return fmt.Errorf("range not honored: %d", resp.StatusCode)
	}

	// Пишем по оффсету
	file, err := os.OpenFile(s.output, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Seek(seg.start, io.SeekStart); err != nil {
		return err
	}

	// ограничиваем чтение строго длиной сегмента
	lim := io.LimitReader(resp.Body, seg.length)
	buf := make([]byte, 256*1024) // большой буфер — меньше syscall'ов
	written, err := io.CopyBuffer(file, lim, buf)
	if err != nil && !errors.Is(err, io.EOF) {
		if s.verbose {
			s.log <- fmt.Sprintf("copy err (%s): %v", rangeHeader, err)
		}
		// частичная запись: досегментируем остаток
		if written > 0 && written < seg.length {
			remain := seg.length - written
			s.queue.push(&segment{start: seg.start + written, length: remain, attempt: seg.attempt})
			atomic.AddInt64(&s.wroteBytes, written)
			return nil // оставим остаток как новую задачу
		}
		return err
	}

	// ok: весь сегмент докачан
	atomic.AddInt64(&s.wroteBytes, written)
	return nil
}
