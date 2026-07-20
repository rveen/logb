package example

import (
	"encoding/binary"
	"io"
	"math"

	"github.com/google/uuid"
	"github.com/rveen/logb"
)

// SweepOptions controls GenerateSweep.
type SweepOptions struct {
	// Runs is how many times the sweep is repeated, each under different
	// conditions. SPEC §6.5's stepped sweep.
	Runs int
	// PointsPerDecade sets the log-axis resolution.
	PointsPerDecade int
	// Decades is how many decades of frequency the sweep covers.
	Decades int
	Codec   logb.Codec
}

func (o *SweepOptions) setDefaults() {
	if o.Runs <= 0 {
		o.Runs = 4
	}
	if o.PointsPerDecade <= 0 {
		o.PointsPerDecade = 50
	}
	if o.Decades <= 0 {
		o.Decades = 5
	}
}

// GenerateSweep writes a frequency-domain fixture: an AC sweep repeated at
// several temperatures.
//
// Nothing else in this repository exercises the two things it is here for, and
// both are places a viewer can be confidently wrong:
//
//   - **A non-time axis.** SPEC §5 makes the axis a tagged union: time is an
//     int64 count of ticks, everything else is an IEEE f64. A viewer that reads
//     one as the other gets a plausible-looking number rather than an error.
//     Here the axis is frequency in Hz, and it is logarithmic — a decade sweep
//     bucketed linearly puts nine tenths of its buckets in the last decade and
//     draws the first decade as a single column.
//   - **Runs.** A stepped sweep is N traces sharing an axis, not one trace
//     (§6.5). Merging them produces a plot that looks like a very noisy single
//     measurement, which is exactly what it is not.
//
// The response is a second-order low-pass whose corner frequency moves with
// temperature, so the runs are visibly distinct and overlap where they should.
func GenerateSweep(w io.Writer, o SweepOptions) error {
	o.setDefaults()

	vw, err := logb.NewWriter(w)
	if err != nil {
		return err
	}
	vw.Codec = o.Codec

	points := o.Decades * o.PointsPerDecade
	s := sweepSchema(o.PointsPerDecade)
	if err := vw.AddStream(s); err != nil {
		return err
	}

	// Each run is the same sweep at a different temperature. The parameter is
	// what distinguishes the traces on screen, so it goes in Params rather than
	// being left implicit in the run index.
	temps := []int{-40, 25, 85, 125, 150, 175, 200, 225}
	for i := 0; i < o.Runs; i++ {
		t := temps[i%len(temps)]
		if err := vw.AddRun(&logb.Run{
			ID:     uint32(i),
			Index:  uint32(i),
			Params: map[string]string{"temperature": itoa(t) + " degC"},
		}); err != nil {
			return err
		}
	}

	if err := vw.BeginSegment(0); err != nil {
		return err
	}

	const recBytes = 8
	rec := make([]byte, recBytes*points)
	for run := 0; run < o.Runs; run++ {
		tempC := float64(temps[run%len(temps)])
		// The corner drifts down as the part heats up.
		corner := 12000.0 * math.Pow(0.9985, tempC+40)

		for i := 0; i < points; i++ {
			f := 10.0 * math.Pow(10, float64(i)/float64(o.PointsPerDecade))
			r := f / corner
			// Second-order response, in dB and degrees.
			mag := -20 * math.Log10(math.Sqrt((1-r*r)*(1-r*r)+r*r))
			phase := -math.Atan2(r, 1-r*r) * 180 / math.Pi

			b := rec[i*recBytes:]
			binary.LittleEndian.PutUint32(b[0:], math.Float32bits(float32(mag)))
			binary.LittleEndian.PutUint32(b[4:], math.Float32bits(float32(phase)))
		}
		// A log axis carries its base in the frame, as an f64 rather than as
		// ticks: this is not a time stream (§5).
		if err := vw.WriteData(s, logb.FloatVal(10), uint32(run), uint32(points), rec); err != nil {
			return err
		}
	}

	if err := vw.WriteMeta("title", "Logb sweep fixture — AC response over temperature"); err != nil {
		return err
	}
	if err := vw.WriteMeta("axis.kind", "frequency"); err != nil {
		return err
	}
	return vw.Close()
}

func sweepSchema(perDecade int) *logb.Schema {
	return &logb.Schema{
		UUID:       uuid.NewSHA1(uuid.NameSpaceOID, []byte("logb/example/sweep")),
		Name:       "ac_sweep",
		RecordBits: 8 * 8,
		AxisKind:   logb.AxisFrequency,
		// Log spacing costs zero bytes per record: the axis is base * ratio^i.
		AxisMode: logb.AxisLog,
		AxisUnit: "Hz",
		AxisStep: logb.FloatVal(math.Pow(10, 1/float64(perDecade))),
		Fields: []logb.Field{
			{Name: "gain", BitOffset: 0, BitWidth: 32, Type: logb.TypeFloat, Unit: "dB"},
			{Name: "phase", BitOffset: 32, BitWidth: 32, Type: logb.TypeFloat, Unit: "deg"},
		},
	}
}

// itoa avoids pulling strconv in for one call site in a fixture.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
