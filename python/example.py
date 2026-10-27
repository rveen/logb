"""Write an example Logb file from Python.

    python3 example.py out.logb
    go run ./cmd/logbdump out.logb      # from the repository root

Four streams, each showing one thing the format is for:

- EngineData: periodic sampling on an implicit time axis, zero bytes of
  timestamp per record, raw integers with a linear conversion.
- can0.raw: a CAN trace on an explicit time axis, with the payload kept as
  bytes and signals over the same bits: those of message 0x100 declared from
  a DBC, those of 0x200 by hand, so both ways can be compared.
- events: the event convention of SPEC.md §6.7, a severity enum and a
  variable-length message.
- ac: an AC sweep on a log-spaced frequency axis with complex values, stepped
  over two runs as a simulator writing a .step sweep would.
"""

import math
import sys
import time
import uuid

import logb

# A name-based uuid stays the same on every run, so recordings of the same
# stream concatenate into one (§6.6). uuid.uuid4() would keep them apart.
NS = uuid.UUID("6f1b1c2e-9a4d-4a7e-8f3e-2d5b6c7a8e90")


# The database for can0.raw's message 0x100. It travels in the file as an
# attachment, so the recording still names its source when this script is gone.
DBC = """VERSION ""

BU_: ECM

BO_ 256 Throttle: 8 ECM
 SG_ ThrottlePos : 31|16@0+ (0.01,0) [0|100] "%"  Vector__XXX

CM_ SG_ 256 ThrottlePos "Throttle pedal position";
"""


def main(path):
    db = logb.DBC(DBC)
    with open(path, "wb") as f, logb.Writer(f, codec="deflate", transpose=True) as w:
        w.write_meta("time.base", "unix")
        w.write_attach("example.dbc", DBC.encode())
        w.write_meta("vehicle.vin", "WVWZZZ1JZXW000001")

        engine = w.add_stream(
            "EngineData",
            uuid=uuid.uuid5(NS, "EngineData"),
            fields=[
                logb.Field("EngineSpeed", "uint", 16, unit="rpm", conv=logb.Linear(0, 0.25)),
                logb.Field("CoolantTemp", "uint", 8, unit="degC", conv=logb.Linear(-40, 1)),
                logb.Field("MIL", "bool"),
            ],
            axis_step=10_000_000,  # 10 ms in ns ticks
        )

        can = w.add_stream(
            "can0.raw",
            uuid=uuid.uuid5(NS, "can0.raw"),
            axis_mode="explicit",
            axis_field="t",
            axis_scale=1000,  # t counts microseconds
            fields=[
                logb.Field("t", "uint", 32, unit="us"),
                logb.Field("id", "uint", 32),
                logb.Field("dlc", "uint", 8),
                logb.Field("payload", "bytes", 64,
                           meta={"payload.encoding": "can", "payload.schema": "example.dbc"}),
                # The signals of message 0x100, over the payload at bit 72 and
                # present only in frames with that id. ThrottlePos is Motorola
                # start bit 31, which lands at payload bit 24 (CAN.md).
                *db.fields("Throttle", offset=72, guard=("id", 0x100)),
                # The signals of message 0x200, declared by hand: what
                # db.fields does, done once per signal. Each DBC line is
                # given above its field. The offset is the payload's bit 72
                # plus the signal's position in Logb's numbering: for Intel
                # (@1) that is the start bit itself, for Motorola (@0) it is
                # 8*(start//8) + 7 - start%8, never the start bit (CAN.md).
                #   SG_ WheelSpeed : 0|16@1+ (0.01,0) [0|655.35] "km/h"
                logb.Field("WheelSpeed", "uint", 16, offset=72 + 0,
                           unit="km/h", conv=logb.Linear(0, 0.01), guard=("id", 0x200)),
                #   SG_ BrakePressure : 39|12@0+ (0.1,0) [0|409.5] "bar"
                #   Motorola start 39 is 8*4 + 7 - 7 = Logb bit 32.
                logb.Field("BrakePressure", "uint", 12, offset=72 + 32, big_endian=True,
                           unit="bar", conv=logb.Linear(0, 0.1), guard=("id", 0x200)),
                #   SG_ BrakeOn : 48|1@1+ (1,0) [0|1] ""
                logb.Field("BrakeOn", "bool", offset=72 + 48, guard=("id", 0x200)),
            ],
        )

        events = w.add_stream(
            "events",
            uuid=uuid.uuid5(NS, "events"),
            axis_mode="explicit",
            axis_field="t",
            fields=[
                logb.Field("t", "uint", 64, unit="ns"),
                logb.Field("severity", "uint", 8,
                           conv=logb.ValueToText({0: "info", 1: "warning", 2: "error"}, "?")),
                logb.Field("message", "string", variable=True),
            ],
        )

        ac = w.add_stream(
            "ac",
            uuid=uuid.uuid5(NS, "ac"),
            axis_kind="frequency",
            axis_mode="log",
            axis_unit="Hz",
            axis_step=10 ** (1 / 10),  # ten points per decade
            fields=[logb.Field("v(out)", "complex", 128, unit="V")],
            meta={"sim.analysis": "ac"},
        )

        now = time.time_ns()
        w.begin_segment(now)

        # 100 samples, one DATA frame. A real logger batches a few thousand.
        w.write(engine, [(3200 + 4 * i, 80 + i // 10, i > 90) for i in range(100)], base=now)

        # Frames alternate between the two messages. Only the wire bytes are
        # given: every signal is read from the payload, however it was declared.
        frames = []
        for i in range(20):
            if i % 2 == 0:
                throttle = 1000 + 50 * i  # 10.00 % .. 19.00 %
                id, payload = 0x100, bytes([0, 0, 0, throttle >> 8, throttle & 0xFF, 0, 0, 0])
            else:
                speed = 5000 + 10 * i  # 50.10 km/h ..
                bar = 20 * i           # 2.0 bar .. in 0.1 bar, Motorola across bytes 4 and 5
                id, payload = 0x200, bytes([speed & 0xFF, speed >> 8, 0, 0,
                                            bar >> 4, (bar & 0xF) << 4, i > 10, 0])
            frames.append({"t": 5000 * i, "id": id, "dlc": 8, "payload": payload})
        w.write(can, frames, base=now)

        w.write(events, [(now, 0, "engine started"), (now + 910_000_000, 1, "MIL on")])

        # Two runs of an RC low-pass: 1 kHz and 10 kHz corners, 10 Hz .. 1 MHz.
        for run, fc in enumerate((1e3, 1e4)):
            w.add_run(run, params={"fc": "%g" % fc})
            pts = [1 / (1 + 1j * (10 * 10 ** (k / 10)) / fc) for k in range(51)]
            w.write(ac, [(z,) for z in pts], base=10.0, run_id=run)
            w.end_run(run)

        # A new segment restates every schema, so a reader handed only the
        # bytes from here on still decodes the rest of the file.
        w.begin_segment(time.time_ns())
        w.write(engine, [(3600, 90, True)] * 10, base=now + 1_000_000_000)


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "example.logb")
