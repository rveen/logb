package example

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/google/uuid"
	"github.com/rveen/logb"
)

// BigOptions controls GenerateBig.
type BigOptions struct {
	// Records is how many records to write. 100 million at the default frame
	// size is roughly a gigabyte before compression.
	Records int
	// PerFrame is records per DATA frame. This is the granularity of both the
	// frame index and of any decode, so it is the main knob a viewer feels:
	// smaller frames mean finer seeking and more index, larger frames mean
	// cheaper compression and coarser overviews.
	PerFrame int
	// FramesPerSegment bounds how much a reader must skip to resynchronise
	// after damage.
	FramesPerSegment int
	Codec            logb.Codec
	Filter           logb.Filter
}

func (o *BigOptions) setDefaults() {
	if o.Records <= 0 {
		o.Records = 1_000_000
	}
	if o.PerFrame <= 0 {
		o.PerFrame = 65536
	}
	if o.FramesPerSegment <= 0 {
		o.FramesPerSegment = 64
	}
}

// bigRecordBytes is the fixed record size of the stream GenerateBig writes.
const bigRecordBytes = 10

// GenerateBig writes a large synthetic recording, for exercising a viewer at
// scale rather than for conformance.
//
// The signals are deliberately not noise. Compressible, smoothly varying data
// is what a real measurement file looks like, and noise would make the file
// incompressible and the timings meaningless.
//
// It carries one deliberately awkward feature: boost is guarded on mode, so it
// is genuinely absent from most records. A viewer that treats absence as zero
// draws a plausible-looking signal pinned to the bottom of the chart, and this
// is the file that shows it.
func GenerateBig(w io.Writer, o BigOptions) error {
	o.setDefaults()

	vw, err := logb.NewWriter(w)
	if err != nil {
		return err
	}
	vw.Codec = o.Codec
	vw.Filter = o.Filter

	s := bigSchema()
	if err := vw.AddStream(s); err != nil {
		return err
	}

	// 100 kHz: fast enough that a few seconds of it is already more samples
	// than a screen has pixels.
	const stepNs = 10_000

	buf := make([]byte, o.PerFrame*bigRecordBytes)
	frames := 0
	for done := 0; done < o.Records; {
		n := o.PerFrame
		if r := o.Records - done; r < n {
			n = r
		}
		if frames%o.FramesPerSegment == 0 {
			if err := vw.BeginSegment(int64(done) * stepNs); err != nil {
				return err
			}
		}

		rec := buf[:n*bigRecordBytes]
		for i := 0; i < n; i++ {
			encodeBigRec(rec[i*bigRecordBytes:], done+i)
		}
		if err := vw.WriteData(s, logb.TickVal(int64(done)*stepNs), 0, uint32(n), rec); err != nil {
			return err
		}
		done += n
		frames++
	}

	if err := vw.WriteMeta("title", fmt.Sprintf("Logb scale fixture — %d records", o.Records)); err != nil {
		return err
	}
	if err := vw.WriteMeta("time.base", "monotonic"); err != nil {
		return err
	}
	return vw.Close()
}

func bigSchema() *logb.Schema {
	return &logb.Schema{
		UUID:       uuid.NewSHA1(uuid.NameSpaceOID, []byte("logb/example/big")),
		Name:       "powertrain",
		RecordBits: bigRecordBytes * 8,
		AxisKind:   logb.AxisTime,
		AxisMode:   logb.AxisImplicit,
		AxisExp:    -9,
		AxisUnit:   "s",
		AxisStep:   logb.TickVal(10_000),
		Fields: []logb.Field{
			{Name: "rpm", BitOffset: 0, BitWidth: 16, Type: logb.TypeUint, Unit: "1/min",
				Conv: logb.Linear{B: 0.5}},
			{Name: "coolant", BitOffset: 16, BitWidth: 16, Type: logb.TypeUint, Unit: "degC",
				Conv: logb.Linear{A: -40, B: 0.1}},
			{Name: "speed", BitOffset: 32, BitWidth: 16, Type: logb.TypeUint, Unit: "km/h",
				Conv: logb.Linear{B: 0.01}},
			// A gear-like enumeration: a state band, not a line.
			{Name: "mode", BitOffset: 48, BitWidth: 8, Type: logb.TypeUint,
				Conv: logb.ValueToText{
					Keys:    []float64{0, 1, 2, 3},
					Texts:   []string{"idle", "cruise", "boost", "regen"},
					Default: "?",
				}},
			// Present only in boost mode. The guard compares raw bits, and an
			// unsatisfied guard means absent — not zero (SPEC §6.2).
			{Name: "boost", BitOffset: 56, BitWidth: 16, Type: logb.TypeUint, Unit: "kPa",
				Conv:       logb.Linear{B: 0.1},
				Guarded:    true,
				GuardField: 3,
				GuardValue: 2},
			{Name: "fault", BitOffset: 72, BitWidth: 1, Type: logb.TypeBool},
		},
	}
}

// encodeBigRec writes record i.
//
// Everything is a slow deterministic function of the index, so the file is
// reproducible, compressible, and has features at several time scales for a
// viewer to find when zooming.
func encodeBigRec(b []byte, i int) {
	t := float64(i)

	rpm := 1600 + 1200*math.Sin(t/50_000) + 200*math.Sin(t/613)
	coolant := 900 + 250*math.Sin(t/2_000_000)
	speed := 4000 + 3500*math.Sin(t/120_000)

	// A mode that dwells rather than flickering, so state bands have runs to
	// merge and the guarded field has long absent stretches.
	mode := byte((i / 250_000) % 4)

	binary.LittleEndian.PutUint16(b[0:], uint16(rpm))
	binary.LittleEndian.PutUint16(b[2:], uint16(coolant))
	binary.LittleEndian.PutUint16(b[4:], uint16(speed))
	b[6] = mode
	// Written unconditionally; the guard is what decides whether it is a value.
	binary.LittleEndian.PutUint16(b[7:], uint16(1000+500*math.Sin(t/7_000)))
	b[9] = 0
	// A rare single-record spike, to prove decimation preserves it.
	if i%5_000_000 == 4_999_999 {
		binary.LittleEndian.PutUint16(b[0:], 65535)
		b[9] = 1
	}
}
