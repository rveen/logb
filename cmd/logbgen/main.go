// logbgen writes the worked Logb example file: a short CAN recording with
// invented messages.
//
//	logbgen [-o file]
//	logbgen -big 100000000 -o big.logb
//
// The default output is byte-identical on every run, which is what lets it be a
// golden fixture. The file it produces is checked in at testdata/; if
// regenerating changes the bytes, the format changed, and that is the point.
//
// -big instead writes a large synthetic recording for exercising a viewer at
// scale, and -sweep writes a frequency-domain sweep over several runs. Neither
// is a conformance fixture and neither is checked in.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/rveen/logb"
	"github.com/rveen/logb/internal/example"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("logbgen: ")

	out := flag.String("o", "testdata/can-example.logb", "output file, or - for stdout")
	big := flag.Int("big", 0, "write a scale fixture of this many records instead of the golden example")
	sweep := flag.Int("sweep", 0, "write a frequency-sweep fixture with this many runs instead")
	perFrame := flag.Int("frame", 65536, "records per DATA frame, with -big")
	codec := flag.String("codec", "zstd", "codec for -big: none or zstd")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "logbgen [-o file]\n\nWrites the worked Logb example: invented CAN traffic.\n")
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()

	w := os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		w = f
	}

	bw := bufio.NewWriterSize(w, 1<<20)
	if *big > 0 {
		c := logb.CodecZstd
		if *codec == "none" {
			c = logb.CodecNone
		}
		err := example.GenerateBig(bw, example.BigOptions{
			Records: *big, PerFrame: *perFrame, Codec: c,
		})
		if err != nil {
			log.Fatal(err)
		}
	} else if *sweep > 0 {
		// A log-spaced non-time axis with several runs: the two things the CAN
		// example has none of, and both are places a viewer can be confidently
		// wrong rather than visibly broken.
		if err := example.GenerateSweep(bw, example.SweepOptions{Runs: *sweep}); err != nil {
			log.Fatal(err)
		}
	} else if err := example.Generate(bw); err != nil {
		log.Fatal(err)
	}
	if err := bw.Flush(); err != nil {
		log.Fatal(err)
	}

	if *out != "-" {
		st, err := os.Stat(*out)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", *out, st.Size())
	}
}
