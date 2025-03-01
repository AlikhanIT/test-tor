package main

/*
	torget is a fast large file downloader over locally installed Tor
	Copyright © 2021-2023 Michał Trojnara <Michal.Trojnara@stunnel.org>

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
	"time"
)

type chunk struct {
	start   int64
	length  int64
	circuit int
	bytes   int64
	since   time.Time
	cancel  context.CancelFunc
}

type State struct {
	ctx         context.Context
	src         string
	output      string
	bytesTotal  int64
	bytesPrev   int64
	circuits    int
	minLifetime time.Duration
	verbose     bool
	chunks      []chunk
	done        chan int
	log         chan string
	terminal    bool
	rwmutex     sync.RWMutex
}

const torBlock = 8000 // The longest plain text block in Tor

func httpClient(user string) *http.Client {
	proxyUrl, _ := url.Parse(fmt.Sprint("socks5://"+user+":"+user+"@127.0.0.1:%d/", torPort))
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyUrl)},
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
	s.done = make(chan int)
	s.log = make(chan string, 10)
	st, _ := os.Stdout.Stat()
	s.terminal = st.Mode()&os.ModeCharDevice == os.ModeCharDevice
	return &s
}

func (s *State) printPermanent(txt string) {
	if s.terminal {
		fmt.Printf("\r%-40s\n", txt)
	} else {
		fmt.Println(txt)
	}
}

func (s *State) printTemporary(txt string) {
	if s.terminal {
		fmt.Printf("\r%-40s", txt)
	}
}

func (s *State) chunkInit(id int) (client *http.Client, req *http.Request) {
	s.chunks[id].bytes = 0
	s.chunks[id].since = time.Now()
	ctx, cancel := context.WithCancel(s.ctx)
	s.chunks[id].cancel = cancel
	client = httpClient(fmt.Sprintf("tg%d", s.chunks[id].circuit))
	req, _ = http.NewRequestWithContext(ctx, "GET", s.src, nil)
	req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d",
		s.chunks[id].start, s.chunks[id].start+s.chunks[id].length-1))
	return
}

func (s *State) chunkFetch(id int, client *http.Client, req *http.Request) {
	defer func() {
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
		s.log <- fmt.Sprintf("Client Do: %s", err.Error())
		return
	}
	if resp.Body == nil {
		s.log <- "Client Do: No response body"
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		s.log <- fmt.Sprintf("Client Do: Unexpected HTTP status: %d", resp.StatusCode)
		return
	}

	// Open the output file
	file, err := os.OpenFile(s.output, os.O_WRONLY, 0)
	if err != nil {
		s.log <- fmt.Sprintf("os OpenFile: %s", err.Error())
		return
	}
	defer file.Close()

	_, err = file.Seek(s.chunks[id].start, io.SeekStart)
	if err != nil {
		s.log <- fmt.Sprintf("File Seek: %s", err.Error())
		return
	}

	// Copy network data to the output file
	buffer := make([]byte, torBlock)
	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			file.Write(buffer[:n])
			// Enough to RLock(), as we only modify our own chunk
			s.rwmutex.RLock()
			if int64(n) < s.chunks[id].length {
				s.chunks[id].start += int64(n)
				s.chunks[id].length -= int64(n)
				s.chunks[id].bytes += int64(n)
			} else {
				s.chunks[id].length = 0
			}
			s.rwmutex.RUnlock()
			if s.chunks[id].length == 0 {
				break
			}
		}
		if err != nil {
			s.log <- fmt.Sprintf("ReadCloser Read: %s", err.Error())
			break
		}
	}
}

func (s *State) getExitNode(id int, client *http.Client) error {
	req, err := http.NewRequest(http.MethodGet, "https://check.torproject.org/api/ip", nil)
	if err != nil {
		return fmt.Errorf("http NewRequest: %s", err.Error())
	}

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
	curr := s.bytesTotal
	s.rwmutex.RLock()
	for id := 0; id < s.circuits; id++ {
		curr -= s.chunks[id].length
	}
	s.rwmutex.RUnlock()

	if curr == s.bytesPrev {
		status = fmt.Sprintf("%6.2f%% done, stalled",
			100*float32(curr)/float32(s.bytesTotal))
	} else {
		speed := float32(curr-s.bytesPrev) / 1000
		prefix := "K"
		if speed >= 1000 {
			speed /= 1000
			prefix = "M"
		}
		if speed >= 1000 {
			speed /= 1000
			prefix = "G"
		}
		seconds := (s.bytesTotal - curr) / (curr - s.bytesPrev)
		status = fmt.Sprintf("%6.2f%% done, %6.2f %sB/s, ETA %d:%02d:%02d",
			100*float32(curr)/float32(s.bytesTotal),
			speed, prefix,
			seconds/3600, seconds/60%60, seconds%60)
	}

	s.bytesPrev = curr
	return
}

func (s *State) progress() {
	for {
		time.Sleep(time.Second)
		if s.verbose {
			s.printLogs()
		} else {
			s.ignoreLogs()
		}
		s.printTemporary(s.statusLine())
	}
}

// Kill the worst performing circuit
func (s *State) darwin() {
	victim := -1
	var slowest float64
	now := time.Now()

	s.rwmutex.RLock()
	for id := 0; id < s.circuits; id++ {
		if s.chunks[id].cancel == nil {
			continue
		}
		eplased := now.Sub(s.chunks[id].since)
		if eplased < s.minLifetime {
			continue
		}
		throughput := float64(s.chunks[id].bytes) / eplased.Seconds()
		if victim >= 0 && throughput >= slowest {
			continue
		}
		victim = id
		slowest = throughput
	}
	if victim >= 0 {
		s.chunks[victim].cancel()
		s.chunks[victim].cancel = nil
	}
	s.rwmutex.RUnlock()
}

func (s *State) getOutputFilepath() {
	var filename string

	_, err := os.Stat(destination)
	if errors.Is(err, fs.ErrNotExist) {
		if force {
			os.MkdirAll(destination, os.ModePerm)
		} else {
			fmt.Printf("WARNING: Unable to find destination \"%s\".\nTrying current directory instead.\n", destination)
			destination = ""
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
			fmt.Printf("WARNING: Cannot decode \"%s\" - %v\n", filename, err)
		} else {
			filename = decoded
		}
	} else {
		filename = name
	}

	s.output = filepath.Join(destination, filename)
}

func (s *State) Fetch(src string) int {
	s.src = src

	s.getOutputFilepath()
	// If the file already exists and the -force argument was not used, exit
	if _, err := os.Stat(s.output); err == nil && !force {
		fmt.Printf("ERROR: \"%s\" already exists. Skipping.\n", s.output)
		return 1
	}
	fmt.Println("Output file:", s.output)

	// Get the target length
	client := httpClient("torget")
	resp, err := client.Head(s.src)
	if err != nil {
		fmt.Println(err.Error())
		return 1
	}
	if resp.ContentLength <= 0 {
		fmt.Println("Failed to retrieve download length")
		return 1
	}
	s.bytesTotal = resp.ContentLength
	fmt.Println("Download length:", s.bytesTotal, "bytes")

	// Create the output file. This will overwrite an existing file
	file, err := os.Create(s.output)
	if file != nil {
		file.Close()
	}
	if err != nil {
		fmt.Println(err.Error())
		return 1
	}

	// Initialize chunks
	chunkLen := s.bytesTotal / int64(s.circuits)
	seq := 0
	for id := range s.circuits {
		s.chunks[id].start = int64(id) * chunkLen
		s.chunks[id].length = chunkLen
		s.chunks[id].circuit = seq
		seq++
	}
	s.chunks[s.circuits-1].length += s.bytesTotal % int64(s.circuits)

	// Spawn initial fetchers
	go s.progress()
	go func() {
		for id := range s.circuits {
			client, req := s.chunkInit(id)
			go s.chunkFetch(id, client, req)
			time.Sleep(499 * time.Millisecond) // Be gentle to the local tor daemon
		}
	}()

	// Spawn additional fetchers as needed
	for {
		select {
		case id := <-s.done:
			if s.chunks[id].length > 0 {
				// Error. Resume in a new and hopefully faster circuit
				s.chunks[id].circuit = seq
				seq++
			} else {
				// Completed
				longest := 0
				s.rwmutex.RLock()
				for i := 1; i < s.circuits; i++ {
					if s.chunks[i].length > s.chunks[longest].length {
						longest = i
					}
				}
				s.rwmutex.RUnlock()

				if s.chunks[longest].length == 0 {
					// All done
					s.printPermanent("Download complete")
					return 0
				}
				if s.chunks[longest].length <= 5*torBlock {
					// Too short to split
					continue
				}

				// This circuit is faster, so we split 80%/20%
				s.rwmutex.Lock()
				s.chunks[id].length = s.chunks[longest].length * 4 / 5
				s.chunks[longest].length -= s.chunks[id].length
				s.chunks[id].start = s.chunks[longest].start + s.chunks[longest].length
				s.rwmutex.Unlock()
			}

			client, req := s.chunkInit(id)
			go s.chunkFetch(id, client, req)
		case <-time.After(time.Second * 30):
			s.darwin()
		}
	}
}

var circuits int
var destination string
var force bool
var minLifetime int
var name string
var torPort int
var verbose bool

func init() {
	// Set up CLI arguments
	flag.IntVar(&circuits, "circuits", 20, "Concurrent circuits.")
	flag.IntVar(&circuits, "c", 20, "Concurrent circuits.")

	flag.StringVar(&destination, "destination", "", "Output directory.")
	flag.StringVar(&destination, "d", "", "Output directory.")

	// No short version of force since it is a comparatively dangerous flag
	flag.BoolVar(&force, "force", false, "Will create parent folder(s) and/or overwrite existing files.")

	flag.IntVar(&minLifetime, "min-lifetime", 10, "Minimum circuit lifetime. (seconds)")
	flag.IntVar(&minLifetime, "l", 10, "Minimum circuit lifetime. (seconds)")

	flag.StringVar(&name, "name", "", "Output filename.")
	flag.StringVar(&name, "n", "", "Output filename.")

	flag.IntVar(&torPort, "tor-port", 9050, "Port your Tor service is listening on.")
	flag.IntVar(&torPort, "p", 9050, "Port your Tor service is listening on.")

	flag.BoolVar(&verbose, "verbose", false, "Show iagnostic details.")
	flag.BoolVar(&verbose, "v", false, "Show iagnostic details.")

	flag.Usage = func() {
		w := flag.CommandLine.Output()

		fmt.Fprintln(w, "torget 2.0, a fast large file downloader over locally installed Tor")
		fmt.Fprintln(w, "Copyright © 2021-2023 Michał Trojnara <Michal.Trojnara@stunnel.org>")
		fmt.Fprintln(w, "Licensed under GNU GPL version 3 <https://www.gnu.org/licenses/>")
		fmt.Fprintln(w, "\nUsage: torget [FLAGS] {file.txt | URL [URL2...]}")

		// Custom print out of the arguments to avoid duplicate entries for long and short versions
		fmt.Fprintln(w, "  -circuits, -c int")
		fmt.Fprintln(w, "        Concurrent circuits. (default 20)")
		fmt.Fprintln(w, "  -destination, -d string")
		fmt.Fprintln(w, "        Output directory. (default current directory)")
		fmt.Fprintln(w, "  -force bool")
		fmt.Fprintln(w, "        Will create parent folder(s) and/or overwrite existing files.")
		fmt.Fprintln(w, "  -min-lifetime, -l int")
		fmt.Fprintln(w, "        Minimum circuit lifetime (seconds). (default 10)")
		fmt.Fprintln(w, "  -name, -n string")
		fmt.Fprintln(w, "        Output filename. (default filename from URL)")
		fmt.Fprintln(w, "  -tor-port, -p int")
		fmt.Fprintln(w, "        Port your Tor service is listening on. (default 9050)")
		fmt.Fprintln(w, "  -verbose, -v")
		fmt.Fprintln(w, "        Show diagnostic details.")
	}
}

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	var uris []string

	if flag.NArg() == 1 {
		// Only one non-flag argument. Check if it's a URL or a text file
		if _, err := os.Stat(flag.Arg(0)); err == nil {
			// Found a file on disk, read URLs from it
			file, err := os.Open(flag.Arg(0))
			if err != nil {
				fmt.Printf("ERROR: argument \"%s\" is not a valid URL or file.\n%v\n", flag.Arg(0), err)
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
		fmt.Printf("Downloading %d files.\n", len(uris))

		// Ignore the -name argument when multiple files are provided, just use the URL's filename
		if name != "" {
			fmt.Println("The -name argument is not usable when multiple URLs are provided. Ignoring.")
			name = ""
		}
	} else if len(uris) < 1 {
		fmt.Println("ERROR: No URLs found.")
		os.Exit(1)
	}

	ctx := context.Background()

	// Iterate over each URL passed as an argument and download the file
	for i, uri := range uris {
		_, err := url.ParseRequestURI(uri)
		if err != nil {
			fmt.Printf("ERROR: \"%s\" is not a valid URL.\n", uri)
			continue
		}

		if len(uris) < 1 {
			fmt.Printf("\n[%d/%d] - %s\n", i+1, len(uris), uri)
		}

		state := NewState(ctx)
		state.Fetch(uri)
	}
}
