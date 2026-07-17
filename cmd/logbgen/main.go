// logbgen writes the worked Logb example file: a short CAN recording with
// invented messages.
//
//	logbgen [-o file]
//
// The output is byte-identical on every run, which is what lets it be a golden
// fixture. The file it produces is checked in at testdata/; if
// regenerating changes the bytes, the format changed, and that is the point.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/rveen/logb/internal/example"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("logbgen: ")

	out := flag.String("o", "testdata/can-example.logb", "output file, or - for stdout")
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

	bw := bufio.NewWriter(w)
	if err := example.Generate(bw); err != nil {
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
