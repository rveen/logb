# logb for Python

A Logb writer in one file, [`logb.py`](logb.py), with no dependencies beyond the
standard library. Copy it next to your program, or put this directory on
`PYTHONPATH`.

It writes what the Go writer writes: given the same streams, records and uuids,
the output is byte-identical. It does not read; use the Go package,
`cmd/logbdump`, or the [viewer](../viewer/) for that.

```python
import logb

with open("engine.logb", "wb") as f, logb.Writer(f, codec="deflate") as w:
    s = w.add_stream("EngineData",
        fields=[
            logb.Field("EngineSpeed", "uint", 16, unit="rpm", conv=logb.Linear(0, 0.25)),
            logb.Field("CoolantTemp", "uint", 8, unit="degC", conv=logb.Linear(-40, 1)),
        ],
        axis_step=10_000_000)                 # 10 ms, in ns ticks
    w.write(s, [(3200, 80), (3204, 80), (3208, 81)], base=0)
```

```sh
python3 example.py /tmp/py.logb               # four streams, two segments
go run ./cmd/logbdump /tmp/py.logb            # from the repository root
python3 -m unittest test_logb                 # SPEC.md §6.2 conformance vectors
```

[`example.py`](example.py) shows the rest: an explicit time axis, a CAN payload
with a guarded big-endian signal over it, the §6.7 event stream with a
variable-length message, and a stepped AC sweep on a log axis with complex values.

## What maps to what

| Python | SPEC.md |
|---|---|
| `Writer(f, codec=, transpose=)` | file header; DATA `codec` and `filter`, §8 |
| `add_stream(name, fields, uuid=, axis_kind=, axis_mode=, axis_exp=, axis_step=, axis_scale=, axis_field=, meta=)` | SCHEMA, §5 and §6.1 |
| `Field(name, type, bits, offset=, big_endian=, variable=, unit=, desc=, conv=, guard=, meta=)` | field, §6.2 |
| `Linear`, `Rational`, `Table`, `ValueToText`, `RangeToText` | conversions, §7 |
| `write(stream, records, base=, run_id=)` | one DATA frame, §8 |
| `write_raw(stream, data, count, ...)` | the same, from already-packed bytes |
| `begin_segment(wall_time_ns)` | SYNC and the schema/run restatement, §4 |
| `add_run` / `end_run` | RUN, §6.5 |
| `write_meta`, `write_attach` | META, ATTACH |
| `close()` (or leaving the `with`) | INDEX and END, §9 |

A few points worth knowing:

- **Records** are tuples in field order or dicts by name. `None` means absent,
  and is allowed only for a guarded field or a variable one. A value that does
  not fit its field raises an error rather than being truncated.
- **Offsets** are placed one after another unless you give `offset=`. A field's
  offset counts bits in its own byte order (§6.2), so a big-endian field
  sharing a byte with a little-endian one needs an explicit offset.
- **Time axes count integer ticks** of 10^`axis_exp` s (default −9, ns), so
  `axis_step`, `axis_scale` and `base` are ints. Other axis kinds use floats.
- **Each `write()` call is one frame.** Batch a few thousand records per call;
  one record per call is valid but pays 40 bytes of framing each.
- The writer enforces what the Go writer enforces: runs contiguous within a
  segment, and `axis_step` changed only at a new segment.
- **Not implemented:** HOLD frames (§6.10) and `lz4`. `zstd` works when the
  `zstandard` package is installed; `deflate` works everywhere.
- Default stream uuids are random. Pass a stable one, e.g. `uuid.uuid5(...)`,
  when files from separate runs should concatenate into one recording (§6.6).
