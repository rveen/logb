// mdf2logb converts an ASAM MDF4 measurement file to Logb.
//
//	mdf2logb [flags] file.mf4
//
// One channel group becomes one stream, the master channel becomes the axis,
// and every other channel becomes a field at the bit offset it already had —
// the record is copied as it stands, because Logb numbers bits the way MDF
// does. Conversions become Logb conversions where the two formats agree;
// invalidation bits become guarded fields; a CAN payload stored out of line
// comes back inline, where a bus payload belongs.
//
// A bus recording holds frames, not signals — what the payload bytes mean is
// never in the recording. Given -dbc it is decoded at import: one stream per
// database message beside the raw frames, multiplexed signals as guarded fields,
// and the database itself embedded as an attachment so the result explains
// itself without it.
//
// What cannot be carried across is printed to stderr rather than dropped in
// silence: an algebraic conversion is a text formula Logb declines to evaluate,
// and event blocks have no equivalent yet.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/rveen/logb"
	"github.com/rveen/logb/dbc"
	"github.com/rveen/logb/mdf"
)

var (
	out       = flag.String("o", "", "output file; default is the input with .logb, - for stdout")
	codec     = flag.String("codec", "zstd", "DATA frame codec: none or zstd")
	transpose = flag.Bool("transpose", false, "group each record byte together before compressing (§8)")
	perFrame  = flag.Int("frame", 0, "records per DATA frame (default 65536)")
	database  = flag.String("dbc", "", "CAN database; decodes a bus recording's frames into signals")
	quiet     = flag.Bool("q", false, "do not report what the mapping could not carry across")
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("mdf2logb: ")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "mdf2logb [flags] file.mf4\n\nConverts an ASAM MDF4 file to Logb.\n")
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
	}
	in := flag.Arg(0)

	o := mdf.Options{Codec: logb.CodecZstd, PerFrame: *perFrame}
	switch *codec {
	case "zstd":
	case "none":
		o.Codec = logb.CodecNone
	default:
		log.Fatalf("unknown codec %q", *codec)
	}
	if *transpose {
		o.Filter = logb.FilterTranspose
	}
	if !*quiet {
		o.Warn = func(format string, a ...any) {
			fmt.Fprintf(os.Stderr, "mdf2logb: %s\n", fmt.Sprintf(format, a...))
		}
	}

	// A bus recording holds frames, not signals. The database is what turns the
	// second into the first, and nothing else in the file can.
	if *database != "" {
		db, err := dbc.ParseFile(*database)
		if err != nil {
			log.Fatal(err)
		}
		o.DBC = db
		if !*quiet {
			fmt.Fprintf(os.Stderr, "mdf2logb: %s: %d messages\n", *database, len(db.Messages))
		}
	}

	f, err := os.Open(in)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	dst := *out
	if dst == "" {
		dst = strings.TrimSuffix(strings.TrimSuffix(in, ".mf4"), ".mdf") + ".logb"
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
	if err := mdf.Convert(f, bw, o); err != nil {
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
		src, err := os.Stat(in)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes, from %d)\n", dst, st.Size(), src.Size())
	}
}
