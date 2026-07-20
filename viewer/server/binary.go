package server

import (
	"encoding/binary"
	"math"
	"net/http"

	"github.com/rveen/logb/viewer/decimate"
)

// The binary series encoding.
//
// JSON costs a parse and roughly twice the bytes, and it cannot represent NaN —
// which is why the JSON path has to convert absent bounds to null. Here NaN
// carries absence directly, which is both smaller and closer to what it means:
// the bucket has no value, rather than a value that happens to be spelled null.
//
//	 0  4  magic "LGBS"
//	 4  2  version, currently 1
//	 6  1  flags: bit 0 set when the range was decoded exactly
//	 7  1  reserved
//	 8  4  bucket count n
//	12  4  reserved, zero
//	16  8n x     float64
//	   8n min    float64, NaN where the bucket is empty
//	   8n max    float64, NaN where the bucket is empty
//	   4n n      int32, zero where the bucket is empty
//
// The header is 16 bytes rather than the 12 it needs, purely so the float64
// arrays start on an 8-byte boundary. A browser reads these as TypedArray views
// over the response buffer, and `new Float64Array(buf, 12, n)` throws outright —
// the payload must be aligned or the client cannot read it without copying.
// Go's encoding/binary has no such constraint, which is exactly why the Go
// round-trip test passed while every chart was blank.
//
// Little-endian throughout, matching both the format itself and every machine
// a browser runs on; JavaScript's TypedArray views are host-endian, so a
// big-endian client would have to swap. That is noted rather than handled
// because no such client exists to test against.
const (
	seriesMagic   = "LGBS"
	seriesVersion = 1
	flagExact     = 1 << 0
	// seriesHeader is the fixed header size, chosen for alignment.
	seriesHeader = 16
)

// writeSeriesBinary streams an envelope as typed arrays.
func writeSeriesBinary(w http.ResponseWriter, e decimate.Envelope, tier query_tier) {
	n := len(e.X)
	buf := make([]byte, seriesHeader+n*(8+8+8+4))

	copy(buf[0:], seriesMagic)
	binary.LittleEndian.PutUint16(buf[4:], seriesVersion)
	if e.Exact {
		buf[6] |= flagExact
	}
	binary.LittleEndian.PutUint32(buf[8:], uint32(n))

	off := seriesHeader
	for _, v := range e.X {
		binary.LittleEndian.PutUint64(buf[off:], math.Float64bits(v))
		off += 8
	}
	// Min and Max already carry NaN where a bucket held no present sample, so
	// absence needs no separate encoding here. The client must still consult
	// the counts rather than testing for NaN, because a NaN bound and a zero
	// count mean the same thing and only one of them is a number.
	for _, v := range e.Min {
		binary.LittleEndian.PutUint64(buf[off:], math.Float64bits(v))
		off += 8
	}
	for _, v := range e.Max {
		binary.LittleEndian.PutUint64(buf[off:], math.Float64bits(v))
		off += 8
	}
	for _, v := range e.N {
		binary.LittleEndian.PutUint32(buf[off:], uint32(v))
		off += 4
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The tier is a header rather than a body field so the payload stays a
	// clean run of typed arrays the client can view without copying.
	w.Header().Set("X-Logb-Tier", string(tier))
	w.Write(buf)
}

// query_tier mirrors query.Tier without importing it into this file's
// signature, keeping the encoding independent of the query package's names.
type query_tier = string
