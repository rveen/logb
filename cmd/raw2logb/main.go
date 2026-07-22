// raw2logb converts a SPICE raw file to Logb.
//
//	raw2logb [flags] file.raw
//
// It reads the binary raw file LTspice writes and applies the mapping SPEC.md
// §11 specifies: the first variable becomes the stream's axis, the rest become
// fields carrying their type column, the header lines become metadata, and a
// stepped sweep's run boundaries — which the raw file leaves to be guessed at —
// become RUN frames.
//
// A sibling netlist is embedded when one is there: `test.raw` next to `test.net`
// produces a file that still knows what circuit it came from, which the raw file
// on its own does not.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/rveen/logb"
	"github.com/rveen/logb/spice"
)

var (
	out    = flag.String("o", "", "output file; default is the input with .logb, - for stdout")
	codec  = flag.String("codec", "zstd", "DATA frame codec: none or zstd")
	net    = flag.String("net", "", "netlist to attach; default is the sibling .net, - for none")
	stream = flag.String("name", "", "stream name; default is the analysis (tran, ac, …)")
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("raw2logb: ")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "raw2logb [flags] file.raw\n\nConverts a SPICE raw file to Logb.\n")
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
	}
	in := flag.Arg(0)

	o := spice.Options{Name: *stream, Codec: logb.CodecZstd}
	switch *codec {
	case "zstd":
	case "none":
		o.Codec = logb.CodecNone
	default:
		log.Fatalf("unknown codec %q", *codec)
	}

	// The netlist is the one thing a raw file is missing and the .asc it came
	// from has: attach it when it is sitting right there.
	netPath := *net
	if netPath == "" {
		netPath = strings.TrimSuffix(in, ".raw") + ".net"
		if _, err := os.Stat(netPath); err != nil {
			netPath = "-"
		}
	}
	if netPath != "-" {
		data, err := os.ReadFile(netPath)
		if err != nil {
			log.Fatal(err)
		}
		o.Attach = map[string][]byte{basename(netPath): data}
	}

	f, err := os.Open(in)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	dst := *out
	if dst == "" {
		dst = strings.TrimSuffix(in, ".raw") + ".logb"
	}
	w := os.Stdout
	if dst != "-" {
		g, err := os.Create(dst)
		if err != nil {
			log.Fatal(err)
		}
		defer g.Close()
		w = g
	}

	bw := bufio.NewWriterSize(w, 1<<20)
	if err := spice.Convert(f, bw, o); err != nil {
		log.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		log.Fatal(err)
	}

	if dst != "-" {
		st, err := os.Stat(dst)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", dst, st.Size())
	}
}

func basename(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
