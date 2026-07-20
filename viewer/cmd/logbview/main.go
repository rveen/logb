// Command logbview opens a Logb file in a browser.
//
//	logbview file.logb
//
// It indexes the file, starts a local HTTP server, and opens the default
// browser at it. The server binds the loopback interface only: this reads a
// file off the user's disk and serves its contents, and nothing about running
// a viewer implies consent to publish that on the network. Pass -addr to
// override deliberately.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/rveen/logb/viewer/index"
	"github.com/rveen/logb/viewer/query"
	"github.com/rveen/logb/viewer/server"
	"github.com/rveen/logb/viewer/web"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("logbview: ")

	addr := flag.String("addr", "127.0.0.1:0", "listen address; port 0 picks a free one")
	open := flag.Bool("open", true, "open the default browser")
	cacheMB := flag.Int("cache", 128, "decoded-frame cache budget, in MiB")
	noCache := flag.Bool("nocache", false, "ignore and do not write the sidecar index")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: logbview [flags] file.logb\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	path := flag.Arg(0)

	// The server comes up before indexing finishes. A large file takes tens of
	// seconds to index and the format offers no shortcut — nothing points
	// forward, so there is no footer to read — but there is no reason to make
	// the browser stare at a blank page while it happens.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	url := "http://" + ln.Addr().String() + "/"
	fmt.Printf("serving %s\n", url)

	state := server.NewState()
	go func() {
		start := time.Now()
		bar := progressBar()
		f, err := index.OpenWith(path, index.Options{
			NoCache: *noCache,
			Progress: func(done, total int64) {
				bar(done, total)
				state.Progress(done, total)
			},
			OnCacheMiss: func(err error) {
				// No cache yet is the ordinary first open, not news.
				if !errors.Is(err, fs.ErrNotExist) {
					fmt.Fprintf(os.Stderr, "reindexing: %v\n", err)
				}
			},
		})
		if err != nil {
			state.Fail(err)
			log.Printf("%v", err)
			return
		}

		// The accessor keeps the file open so any frame can be decoded on
		// demand; the index holds only per-frame statistics, not samples.
		acc, err := index.NewAccessor(path, f.Frames)
		if err != nil {
			state.Fail(err)
			log.Printf("%v", err)
			return
		}
		q := query.New(f, acc)
		if *cacheMB > 0 {
			q.SetCacheBytes(int64(*cacheMB) << 20)
		}

		report(f, time.Since(start))
		state.Ready(f, q)
	}()

	if *open {
		go openBrowser(url)
	}

	srv := &http.Server{
		Handler: server.NewWithState(state, web.Handler()),
		// A generous read timeout with no write timeout: a decimation request
		// over a large range can legitimately take a while, and cutting it off
		// mid-response would look like corruption to the client.
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.Serve(ln))
}

// progressBar prints a single rewriting line while a scan runs.
//
// Rate limited rather than printed per frame: a large file has hundreds of
// thousands of frames, and writing a line for each would cost more than the
// decompression it is reporting on.
func progressBar() func(done, total int64) {
	last := time.Now()
	started := last
	return func(done, total int64) {
		if total <= 0 || time.Since(last) < 100*time.Millisecond {
			return
		}
		last = time.Now()
		pct := float64(done) * 100 / float64(total)
		fmt.Fprintf(os.Stderr, "\rindexing %5.1f%%  %s", pct, time.Since(started).Round(time.Second))
	}
}

// report prints what the file turned out to contain, so the terminal is useful
// even when the browser is not.
func report(f *index.File, took time.Duration) {
	fmt.Fprintf(os.Stderr, "\r%40s\r", "")

	how := "indexed"
	switch {
	case f.Extended:
		how = "extended a cached index"
	case f.Cached:
		how = "loaded a cached index"
	}
	fmt.Printf("%s — %d bytes, %s in %s\n", f.Path, f.Size, how, took.Round(time.Millisecond))

	switch {
	case f.Truncated:
		// Rule 2: this is a valid file containing every record up to the last
		// intact frame, not a failure.
		fmt.Println("  TRUNCATED — scan stopped at damage; records up to that point are intact")
	case f.Closed:
		fmt.Println("  a writer closed this file cleanly (END frame seen)")
	default:
		fmt.Println("  input ended without an END frame — still a valid file (rule 2)")
	}
	for _, u := range f.Unsupported {
		fmt.Printf("  unsupported: %s\n", u)
	}

	fmt.Printf("  %d frames in %d segments, %d records total\n",
		len(f.Frames.Data), len(f.Frames.Segments), f.Records())

	for _, s := range f.Streams {
		plottable := 0
		for i := range s.Fields {
			if !s.Fields[i].IsAxis && s.Fields[i].Class != index.ClassBlob {
				plottable++
			}
		}
		fmt.Printf("  %-16s %s/%s  %d records, %d fields (%d plottable)",
			s.Name, s.AxisKind, s.AxisMode, s.Records, len(s.Fields), plottable)
		if len(s.Runs) > 1 {
			fmt.Printf(", %d runs", len(s.Runs))
		}
		fmt.Println()
	}
}

// openBrowser is best effort. A viewer that failed to open a browser but did
// print its URL is still usable, so nothing here is fatal.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	if err := exec.Command(cmd, append(args, url)...).Start(); err != nil {
		log.Printf("could not open a browser (%v); visit %s", err, url)
	}
}
