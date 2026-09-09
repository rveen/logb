package logb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// scopeSchema is the case Column exists for: an acquisition with a dense
// implicit axis, plus enough neighbouring fields that the column is strided
// rather than contiguous. bigEndian flips every multi-byte field, which for a
// byte-aligned field is the whole of what §6.3 has to say.
func scopeSchema(bigEndian bool) *Schema {
	return &Schema{
		UUID:       uid("scope/ch1"),
		Name:       "scope1",
		RecordBits: 8 * 15,
		AxisKind:   AxisTime,
		AxisMode:   AxisImplicit,
		AxisExp:    -12,
		AxisUnit:   "s",
		AxisStep:   TickVal(500_000), // 500 ns
		Fields: []Field{
			{Name: "ch1", BitOffset: 0, BitWidth: 16, Type: TypeSint, BigEndian: bigEndian,
				Unit: "V", Conv: Linear{A: 0, B: 1.0 / 32768}},
			{Name: "ch2", BitOffset: 16, BitWidth: 16, Type: TypeUint, BigEndian: bigEndian},
			{Name: "flags", BitOffset: 32, BitWidth: 8, Type: TypeUint},
			{Name: "aux32", BitOffset: 40, BitWidth: 32, Type: TypeFloat, BigEndian: bigEndian},
			{Name: "aux64", BitOffset: 72, BitWidth: 32, Type: TypeSint, BigEndian: bigEndian},
			{Name: "gate", BitOffset: 104, BitWidth: 8, Type: TypeSint},
		},
	}
}

func encodeScopeRec(s *Schema, i int) []byte {
	be := s.Fields[0].BigEndian
	bo := binary.ByteOrder(binary.LittleEndian)
	if be {
		bo = binary.BigEndian
	}
	b := make([]byte, s.RecordBytes())
	bo.PutUint16(b[0:], uint16(int16(i*37-20000)))
	bo.PutUint16(b[2:], uint16(i*11))
	b[4] = byte(i)
	bo.PutUint32(b[5:], math.Float32bits(float32(i)*0.25))
	bo.PutUint32(b[9:], uint32(int32(-i*1000)))
	b[13] = byte(int8(i - 60))
	return b
}

// scopeBatch writes n records through the real writer and reads the batch back,
// so Column is always tested against bytes that made a round trip rather than
// against a hand-built Batch.
func scopeBatch(t *testing.T, s *Schema, n int) *Batch {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	var recs []byte
	for i := 0; i < n; i++ {
		recs = append(recs, encodeScopeRec(s, i)...)
	}
	if err := w.WriteData(s, TickVal(0), 0, uint32(n), recs); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if b.Count != uint32(n) {
		t.Fatalf("count = %d, want %d", b.Count, n)
	}
	return b
}

// TestColumnMatchesRaw is the whole correctness claim: a column is the same
// numbers Raw gives, in the field's own type, for every supported width and
// both byte orders.
func TestColumnMatchesRaw(t *testing.T) {
	const n = 500
	for _, be := range []bool{false, true} {
		name := "littleEndian"
		if be {
			name = "bigEndian"
		}
		t.Run(name, func(t *testing.T) {
			s := scopeSchema(be)
			b := scopeBatch(t, s, n)

			ch1 := make([]int16, n)
			if got, err := Column(b, 0, ch1); err != nil || got != n {
				t.Fatalf("Column(ch1) = %d, %v", got, err)
			}
			ch2 := make([]uint16, n)
			if _, err := Column(b, 1, ch2); err != nil {
				t.Fatal(err)
			}
			flags := make([]uint8, n)
			if _, err := Column(b, 2, flags); err != nil {
				t.Fatal(err)
			}
			aux32 := make([]float32, n)
			if _, err := Column(b, 3, aux32); err != nil {
				t.Fatal(err)
			}
			aux64 := make([]int32, n)
			if _, err := Column(b, 4, aux64); err != nil {
				t.Fatal(err)
			}
			gate := make([]int8, n)
			if _, err := Column(b, 5, gate); err != nil {
				t.Fatal(err)
			}

			for i := 0; i < n; i++ {
				raw := func(f int) any {
					v, err := b.Raw(i, f)
					if err != nil {
						t.Fatalf("Raw(%d,%d): %v", i, f, err)
					}
					return v
				}
				if v := int16(raw(0).(int64)); v != ch1[i] {
					t.Fatalf("ch1[%d] = %d, Raw says %d", i, ch1[i], v)
				}
				if v := uint16(raw(1).(uint64)); v != ch2[i] {
					t.Fatalf("ch2[%d] = %d, Raw says %d", i, ch2[i], v)
				}
				if v := uint8(raw(2).(uint64)); v != flags[i] {
					t.Fatalf("flags[%d] = %d, Raw says %d", i, flags[i], v)
				}
				// Raw widens an f32 to float64 (§7's pipeline is float64), so
				// the comparison is against the widened column value.
				if v := raw(3).(float64); v != float64(aux32[i]) {
					t.Fatalf("aux32[%d] = %v, Raw says %v", i, aux32[i], v)
				}
				if v := int32(raw(4).(int64)); v != aux64[i] {
					t.Fatalf("aux64[%d] = %d, Raw says %d", i, aux64[i], v)
				}
				if v := int8(raw(5).(int64)); v != gate[i] {
					t.Fatalf("gate[%d] = %d, Raw says %d", i, gate[i], v)
				}
			}

			// And the values are what was written, not merely self-consistent.
			for i := 0; i < n; i++ {
				if want := int16(i*37 - 20000); ch1[i] != want {
					t.Fatalf("ch1[%d] = %d, want %d", i, ch1[i], want)
				}
				if want := float32(i) * 0.25; aux32[i] != want {
					t.Fatalf("aux32[%d] = %v, want %v", i, aux32[i], want)
				}
			}
		})
	}
}

// TestColumnAfterTransposeAndCodec pins the property that makes Column simple:
// Data is the decompressed, de-filtered records, so the strided read needs to
// know nothing about how the frame was stored.
func TestColumnAfterTransposeAndCodec(t *testing.T) {
	const n = 300
	s := scopeSchema(false)

	var out bytes.Buffer
	w, err := NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	w.Codec, w.Filter = CodecZstd, FilterTranspose
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	var recs []byte
	for i := 0; i < n; i++ {
		recs = append(recs, encodeScopeRec(s, i)...)
	}
	if err := w.WriteData(s, TickVal(0), 0, n, recs); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int16, n)
	if k, err := Column(b, 0, got); err != nil || k != n {
		t.Fatalf("Column = %d, %v", k, err)
	}
	for i := 0; i < n; i++ {
		if want := int16(i*37 - 20000); got[i] != want {
			t.Fatalf("ch1[%d] = %d, want %d (transposed+zstd)", i, got[i], want)
		}
	}
}

// TestColumnFloat32IsNotWidened is the reason Column is typed rather than
// returning float64 like the boxed path: an f32 stream stays f32, so a browser
// or a GPU gets the bytes the instrument produced.
func TestColumnFloat32IsNotWidened(t *testing.T) {
	s := scopeSchema(false)
	b := scopeBatch(t, s, 8)

	if _, err := Column(b, 3, make([]float64, 8)); !errors.Is(err, ErrColumnType) {
		t.Fatalf("f32 field into []float64: err = %v, want ErrColumnType", err)
	}
	got := make([]float32, 8)
	if _, err := Column(b, 3, got); err != nil {
		t.Fatal(err)
	}
	for i := range got {
		if want := float32(i) * 0.25; got[i] != want {
			t.Fatalf("aux32[%d] = %v, want %v", i, got[i], want)
		}
	}
}

func TestColumnRefusals(t *testing.T) {
	s := scopeSchema(false)
	b := scopeBatch(t, s, 16)

	// Wrong element type for the field's type, and for its width.
	if _, err := Column(b, 0, make([]uint16, 16)); !errors.Is(err, ErrColumnType) {
		t.Errorf("sint16 into []uint16: %v, want ErrColumnType", err)
	}
	if _, err := Column(b, 0, make([]int32, 16)); !errors.Is(err, ErrColumnType) {
		t.Errorf("sint16 into []int32: %v, want ErrColumnType", err)
	}
	if _, err := Column(b, 4, make([]int64, 16)); !errors.Is(err, ErrColumnType) {
		t.Errorf("sint32 into []int64: %v, want ErrColumnType", err)
	}
	if _, err := Column(b, 99, make([]int16, 16)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("out-of-range field: %v, want ErrCorrupt", err)
	}

	// A 12-bit signed field is an ordinary thing for a schema to hold and has
	// no Go type. It is refused on shape rather than on type: the objection is
	// that it is not byte-sized, and no slice would have made it readable.
	lb := func() *Batch {
		ls := loggerSchema()
		var out bytes.Buffer
		w, _ := NewWriter(&out)
		if err := w.AddStream(ls); err != nil {
			t.Fatal(err)
		}
		var recs []byte
		for i := 0; i < 10; i++ {
			recs = append(recs, encodeLoggerRec(uint16(i), int16(i), i%2 == 0)...)
		}
		if err := w.WriteData(ls, TickVal(0), 0, 10, recs); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := NewReader(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		bb, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		return bb
	}()

	if _, err := Column(lb, 1, make([]int16, 10)); !errors.Is(err, ErrColumnShape) {
		t.Errorf("12-bit sint: %v, want ErrColumnShape", err)
	}
	// rpm is a 16-bit uint at bit 0 and is a legal column even though its
	// neighbours are not; the refusals above are about the field, not the stream.
	if k, err := Column(lb, 0, make([]uint16, 10)); err != nil || k != 10 {
		t.Errorf("rpm column = %d, %v", k, err)
	}
	// A bool has no column representation.
	if _, err := Column(lb, 2, make([]uint8, 10)); !errors.Is(err, ErrColumnShape) {
		t.Errorf("bool field: %v, want ErrColumnShape", err)
	}
}

func TestColumnRefusesGuardedField(t *testing.T) {
	s := &Schema{
		UUID:       uid("column/guarded"),
		Name:       "guarded",
		RecordBits: 32,
		AxisKind:   AxisTime,
		AxisMode:   AxisImplicit,
		AxisExp:    -9,
		AxisStep:   TickVal(1000),
		Fields: []Field{
			{Name: "sel", BitOffset: 0, BitWidth: 8, Type: TypeUint},
			{Name: "a", BitOffset: 8, BitWidth: 16, Type: TypeSint,
				Guarded: true, GuardField: 0, GuardValue: 1},
		},
	}
	var out bytes.Buffer
	w, err := NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddStream(s); err != nil {
		t.Fatal(err)
	}
	recs := make([]byte, 4*4)
	if err := w.WriteData(s, TickVal(0), 0, 4, recs); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}

	// The guard is unsatisfied in every record here, so a slice would be filled
	// with four zeros that mean "absent" — which is exactly the confusion §6.2
	// exists to prevent. Refuse instead.
	if _, err := Column(b, 1, make([]int16, 4)); !errors.Is(err, ErrColumnGuarded) {
		t.Fatalf("guarded field: %v, want ErrColumnGuarded", err)
	}
	if _, err := b.Raw(0, 1); !errors.Is(err, ErrFieldAbsent) {
		t.Fatalf("Raw on the same field: %v, want ErrFieldAbsent", err)
	}
}

func TestColumnShortAndLongDst(t *testing.T) {
	s := scopeSchema(false)
	b := scopeBatch(t, s, 100)

	short := make([]int16, 10)
	n, err := Column(b, 0, short)
	if err != nil || n != 10 {
		t.Fatalf("short dst: %d, %v", n, err)
	}
	for i := range short {
		if want := int16(i*37 - 20000); short[i] != want {
			t.Fatalf("short[%d] = %d, want %d", i, short[i], want)
		}
	}

	long := make([]int16, 150)
	for i := range long {
		long[i] = -1
	}
	n, err = Column(b, 0, long)
	if err != nil || n != 100 {
		t.Fatalf("long dst: %d, %v", n, err)
	}
	for i := 100; i < 150; i++ {
		if long[i] != -1 {
			t.Fatalf("long[%d] = %d, want the tail untouched", i, long[i])
		}
	}

	if n, err := Column(b, 0, []int16{}); n != 0 || err != nil {
		t.Fatalf("empty dst: %d, %v", n, err)
	}
}

// BenchmarkColumn against BenchmarkValue is the §5.3 claim in numbers: the same
// megapoint acquisition, boxed and not.
func benchBatch(b *testing.B, n int) *Batch {
	b.Helper()
	s := scopeSchema(false)
	var out bytes.Buffer
	w, err := NewWriter(&out)
	if err != nil {
		b.Fatal(err)
	}
	if err := w.AddStream(s); err != nil {
		b.Fatal(err)
	}
	recs := make([]byte, 0, n*s.RecordBytes())
	for i := 0; i < n; i++ {
		recs = append(recs, encodeScopeRec(s, i)...)
	}
	if err := w.WriteData(s, TickVal(0), 0, uint32(n), recs); err != nil {
		b.Fatal(err)
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	r, err := NewReader(bytes.NewReader(out.Bytes()))
	if err != nil {
		b.Fatal(err)
	}
	batch, err := r.Next()
	if err != nil {
		b.Fatal(err)
	}
	return batch
}

func BenchmarkColumn(b *testing.B) {
	const n = 1 << 20
	batch := benchBatch(b, n)
	dst := make([]int16, n)
	b.SetBytes(int64(n) * 2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Column(batch, 0, dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRawBoxed(b *testing.B) {
	const n = 1 << 20
	batch := benchBatch(b, n)
	dst := make([]int16, n)
	b.SetBytes(int64(n) * 2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < n; j++ {
			v, err := batch.Raw(j, 0)
			if err != nil {
				b.Fatal(err)
			}
			dst[j] = int16(v.(int64))
		}
	}
}
