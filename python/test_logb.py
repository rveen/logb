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


class TestOverlay(unittest.TestCase):
    """Signals over a raw CAN payload are read from it, never written into it."""

    def stream(self):
        return logb.Stream("can", [
            logb.Field("id", "uint", 32),
            logb.Field("payload", "bytes", 64),
            # DBC start bit 31, Motorola, 16 bits: payload bit 24 (CAN.md).
            logb.Field("Throttle", "uint", 16, offset=32 + 24, big_endian=True),
            logb.Field("Gear", "uint", 4, offset=32 + 40),
            logb.Field("Speed", "uint", 8, offset=32 + 48, guard=("id", 0x100)),
        ])

    PAYLOAD = bytes([0, 0, 0, 0x12, 0x34, 0x05, 0x99, 0])

    def test_derived(self):
        s = self.stream()
        rec = s.pack({"id": 0x100, "payload": self.PAYLOAD})[0]
        self.assertEqual(rec, bytes([0, 1, 0, 0]) + self.PAYLOAD)
        self.assertEqual(s.pack((0x100, self.PAYLOAD, None, None, None))[0], rec)

    def test_consistent_value(self):
        s = self.stream()
        rec = s.pack({"id": 0x100, "payload": self.PAYLOAD})[0]
        self.assertEqual(s.pack((0x100, self.PAYLOAD, 0x1234, 5, 0x99))[0], rec)

    def test_mismatch(self):
        s = self.stream()
        for k, v in [("Throttle", 0x1235), ("Gear", 6), ("Speed", 0x98)]:
            with self.assertRaises(ValueError, msg=k):
                s.pack({"id": 0x100, "payload": self.PAYLOAD, k: v})

    def test_partial_overlap_not_derived(self):
        # Gear's bytes are not all inside the payload, so it is an ordinary
        # field and needs a value of its own.
        s = logb.Stream("s", [logb.Field("payload", "bytes", 8),
                              logb.Field("Gear", "uint", 8, offset=4)])
        with self.assertRaises(ValueError):
            s.pack({"payload": b"\x00"})

    def test_absent_payload(self):
        # With its bytes field absent, an overlaid field writes its own bits.
        s = logb.Stream("s", [logb.Field("kind", "uint", 8),
                              logb.Field("payload", "bytes", 8, guard=("kind", 1)),
                              logb.Field("x", "uint", 8, offset=8)])
        self.assertEqual(s.pack((0, None, 7))[0], bytes([0, 7]))


# The database dbc/dbc_test.go uses, plus a multi-line comment.
SAMPLE_DBC = """VERSION "test"

NS_ :
\tCM_
\tBA_DEF_

BS_:

BU_: ECM ABS Tester

BO_ 256 EngineData: 8 ECM
 SG_ EngineSpeed : 0|16@1+ (0.25,0) [0|16383.75] "rpm"  ABS,Tester
 SG_ CoolantTemp : 16|8@1+ (1,-40) [-40|215] "degC"  Tester
 SG_ EngineRunning : 37|1@1+ (1,0) [0|1] ""  Tester

BO_ 512 VehicleStatus: 8 ABS
 SG_ VehicleSpeed : 7|16@0+ (0.01,0) [0|655.35] "km/h"  Tester
 SG_ Odometer : 23|24@0+ (0.1,0) [0|1677721.5] "km"  Tester
 SG_ Gear : 40|4@1+ (1,0) [0|15] ""  Tester

BO_ 2566844926 J1939Message: 8 Vector__XXX
 SG_ Torque : 0|8@1-  (1,-125) [-125|125] "%"  Vector__XXX

BO_ 100 Mux: 8 ECU
 SG_ Selector M : 0|8@1+ (1,0) [0|255] ""  Tester
 SG_ Temperature m1 : 8|16@1+ (0.1,-40) [0|0] "degC"  Tester
 SG_ Pressure m2 : 8|16@1+ (0.5,0) [0|0] "kPa"  Tester
 SG_ Ranged : 24|8@1+ (1,0) [0|0] ""  Tester

BO_ 101 Nested: 8 ECU
 SG_ Service M : 0|8@1+ (1,0) [0|255] ""  Tester
 SG_ PID m1M : 8|8@1+ (1,0) [0|255] ""  Tester
 SG_ Value m3 : 16|16@1+ (1,0) [0|0] ""  Tester

CM_ BO_ 256 "Engine data, 10 ms";
CM_ SG_ 256 EngineSpeed "Crankshaft speed,
measured at the flywheel";
VAL_ 512 Gear 0 "Neutral" 1 "First" 2 "Second" 15 "Reverse" ;
SIG_VALTYPE_ 100 Ranged : 1;
SG_MUL_VAL_ 100 Ranged Selector 3-4;
"""


class TestDBC(unittest.TestCase):
    def setUp(self):
        self.db = logb.DBC(SAMPLE_DBC)

    def byname(self, fields):
        return {f.name: f for f in fields}

    def test_parse(self):
        db = self.db
        self.assertEqual(db.version, "test")
        j = db.message("J1939Message")
        self.assertTrue(j.extended)
        self.assertEqual(j.id, 2566844926 & 0x7FFFFFFF)
        self.assertIs(db.message(256), db.message("EngineData"))
        self.assertEqual(db.message(256).desc, "Engine data, 10 ms")
        with self.assertRaises(KeyError):
            db.message("Nope")

    def test_fields(self):
        f = self.byname(self.db.fields("EngineData", offset=64))
        e = f["EngineSpeed"]
        self.assertEqual((e.offset, e.bits, e.type, e.big_endian), (64, 16, "uint", False))
        self.assertEqual((e.conv.a, e.conv.b), (0, 0.25))
        self.assertEqual(e.desc, "Crankshaft speed,\nmeasured at the flywheel")
        self.assertEqual(e.meta, {"dbc.min": "0", "dbc.max": "16383.75"})
        self.assertEqual(f["CoolantTemp"].conv.a, -40)
        self.assertEqual(f["EngineRunning"].type, "bool")
        self.assertEqual(self.byname(self.db.fields("J1939Message"))["Torque"].type, "sint")

    def test_motorola_and_values(self):
        f = self.byname(self.db.fields("VehicleStatus"))
        # CAN.md: start bits 7 and 23 become 0 and 16.
        self.assertEqual((f["VehicleSpeed"].offset, f["VehicleSpeed"].big_endian), (0, True))
        self.assertEqual(f["Odometer"].offset, 16)
        g = f["Gear"]
        self.assertIsInstance(g.conv, logb.ValueToText)
        self.assertEqual(dict(g.conv.texts)[15], "Reverse")
        self.assertIsNone(g.guard)

    def test_multiplex(self):
        warned = []
        fs = self.db.fields("Mux", offset=64, prefix="mux.", warn=warned.append)
        self.assertEqual([f.name for f in fs], ["mux.Selector", "mux.Temperature", "mux.Pressure"])
        self.assertEqual(fs[1].guard, ("mux.Selector", 1))
        self.assertEqual(fs[2].guard, ("mux.Selector", 2))
        self.assertEqual(fs[1].offset, fs[2].offset)
        # A range of selector values is a second kind of selection; reported, not declared.
        self.assertEqual(len(warned), 1)
        self.assertIn("Ranged", warned[0])
        s = logb.Stream("Mux", [logb.Field("t", "sint", 64), logb.Field("payload", "bytes", 64)] + fs,
                        axis_mode="explicit")
        self.assertEqual(s.fields[3].guard, (2, 1))

    def test_refusals(self):
        with self.assertRaisesRegex(ValueError, "more than one level"):
            self.db.fields("Nested")
        with self.assertRaisesRegex(ValueError, "do not chain"):
            self.db.fields("Mux", guard=("id", 100), warn=lambda s: None)
        with self.assertRaisesRegex(ValueError, "whole byte"):
            self.db.fields("EngineData", offset=4)
        db = logb.DBC("BO_ 1 Short: 2 X\n SG_ Far : 16|8@1+ (1,0) [0|0] \"\" X\n"
                      " SG_ Lost m3 : 0|8@1+ (1,0) [0|0] \"\" X\n")
        warned = []
        self.assertEqual(db.fields("Short", warn=warned.append), [])
        self.assertEqual(len(warned), 2)
        with self.assertRaises(ValueError):
            logb.DBC("not a database\n")
        with self.assertRaises(ValueError):
            logb.DBC("BO_ 100 M: 8 X\n SG_ Broken : bad\n")

    def test_go_g(self):
        # Checked against Go's fmt %g; Python's %g would round the first.
        for v, want in [(16383.75, "16383.75"), (1677721.5, "1.6777215e+06"), (1e6, "1e+06"),
                        (999999.0, "999999"), (-40.0, "-40"), (0.0, "0"), (1e-05, "1e-05"),
                        (0.0001, "0.0001")]:
            self.assertEqual(logb._go_g(v), want)

    def test_raw_overlay(self):
        # Signals over a raw trace's payload, guarded on the CAN id, and
        # written without decoding a frame.
        s = logb.Stream("can0", [logb.Field("id", "uint", 32), logb.Field("payload", "bytes", 64)]
                        + self.db.fields("VehicleStatus", offset=32, guard=("id", 512)))
        payload = bytes([0x12, 0x34, 0x00, 0x01, 0x02, 0x03, 0, 0])
        rec = s.pack({"id": 512, "payload": payload})[0]
        self.assertEqual(rec[4:], payload)
        self.assertEqual(s.fields[2].guard, (0, 512))
        with self.assertRaises(ValueError):
            s.pack({"id": 512, "payload": payload, "VehicleSpeed": 0x1235})
        s.pack({"id": 512, "payload": payload, "VehicleSpeed": 0x1234, "Odometer": 0x102})


if __name__ == "__main__":
    unittest.main()
