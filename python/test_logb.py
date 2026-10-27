"""Tests for logb.py. Run with: python3 -m unittest test_logb"""

import io
import unittest

import logb

# SPEC.md §6.2, the normative bit-numbering table: record bytes, big endian,
# bit_offset, bit_width, raw value. Packing the raw value into a zeroed record
# must reproduce exactly the bits the field covers.
VECTORS = [
    ("1234", False, 0, 16, 0x3412),
    ("1234", True, 0, 16, 0x1234),
    ("1234", False, 4, 12, 0x341),
    ("1234", True, 3, 12, 0x91A),
    ("0020", False, 13, 1, 1),
    ("0020", True, 10, 1, 1),
    ("78563412", False, 0, 32, 0x12345678),
    ("12345678", True, 0, 32, 0x12345678),
    ("FF0F", False, 0, 12, 0xFFF),
    ("FFF0", True, 0, 12, 0xFFF),
    ("A5", False, 1, 6, 0x12),
    ("A5", True, 1, 6, 0x12),
]


def covered(rec, big, off, width):
    """rec with every bit outside the field cleared."""
    out = bytearray(len(rec))
    for p in range(off, off + width):
        mask = 1 << (7 - p % 8) if big else 1 << (p % 8)
        out[p // 8] |= rec[p // 8] & mask
    return bytes(out)


class TestConformanceVectors(unittest.TestCase):
    def test_vectors(self):
        for hexrec, big, off, width, raw in VECTORS:
            rec = bytes.fromhex(hexrec)
            pad = logb.Field("pad", "uint", 8 * len(rec), offset=0)
            f = logb.Field("f", "uint", width, offset=off, big_endian=big)
            s = logb.Stream("s", [pad, f])
            fixed, _ = s.pack((0, raw))
            self.assertEqual(fixed, covered(rec, big, off, width), (hexrec, big, off, width))

    def test_sint(self):
        s = logb.Stream("s", [logb.Field("f", "sint", 12, big_endian=True),
                              logb.Field("pad", "uint", 4)])
        self.assertEqual(s.pack((-1, 0))[0], bytes.fromhex("FFF0"))


class TestWriter(unittest.TestCase):
    def test_crc32c(self):
        self.assertEqual(logb.crc32c(b"123456789"), 0xE3069283)

    def test_header(self):
        buf = io.BytesIO()
        logb.Writer(buf)
        h = buf.getvalue()
        self.assertEqual(h[:8], b"\x89LOGB\r\n\x1a")
        self.assertEqual(int.from_bytes(h[12:16], "little"), logb.crc32c(h[:12]))

    def test_runs_contiguous(self):
        w = logb.Writer(io.BytesIO())
        s = w.add_stream("s", [logb.Field("x", "uint", 8)], axis_step=1)
        w.write(s, [(1,)], run_id=0)
        w.write(s, [(1,)], run_id=1)
        with self.assertRaises(ValueError):
            w.write(s, [(1,)], run_id=0)
        w.begin_segment()
        w.write(s, [(1,)], run_id=0)

    def test_axis_step_needs_segment(self):
        w = logb.Writer(io.BytesIO())
        s = w.add_stream("s", [logb.Field("x", "uint", 8)], axis_step=1)
        w.write(s, [(1,)])
        s.axis_step = 2
        with self.assertRaises(ValueError):
            w.write(s, [(1,)])
        w.begin_segment()
        w.write(s, [(1,)])

    def test_rejects(self):
        for fields, kw in [
            ([logb.Field("x", "string", 12)], {}),                       # unaligned blob
            ([logb.Field("x", "float", 16)], {}),                        # f16 unsupported
            ([logb.Field("x", "string", 8, conv=logb.Linear(0, 1))], {}),
            ([logb.Field("x", "uint", 8)], {"axis_mode": "log", "axis_step": 2.0}),
            ([logb.Field("x", "float", 32, guard=("x", 1))], {}),
        ]:
            with self.assertRaises(ValueError):
                logb.Stream("s", fields, **kw)

    def test_value_range(self):
        s = logb.Stream("s", [logb.Field("x", "uint", 4), logb.Field("y", "sint", 4)])
        with self.assertRaises(ValueError):
            s.pack((16, 0))
        with self.assertRaises(ValueError):
            s.pack((0, 8))
        with self.assertRaises(ValueError):
            s.pack({"x": 1})  # y missing and not guarded


if __name__ == "__main__":
    unittest.main()
