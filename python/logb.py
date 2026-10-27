"""A Logb v0.1 writer in pure Python.

Logb is a self-describing binary format for time-series measurement, bus-trace,
and simulation recording; see SPEC.md in the repository root. This module writes
it and does nothing else. It is one file with no dependencies beyond the
standard library, so it can be copied next to whatever program needs it.

It follows the Go reference writer (writer.go) frame for frame: given the same
streams, records, and uuids it emits the same bytes. Like that writer it only
appends and never seeks, so any binary file object works, including a pipe or a
socket, and a file cut short by power loss is still a valid file.

    import logb

    with open("engine.logb", "wb") as f, logb.Writer(f) as w:
        s = w.add_stream("EngineData",
            fields=[
                logb.Field("EngineSpeed", "uint", 16, unit="rpm",
                           conv=logb.Linear(0, 0.25)),
                logb.Field("CoolantTemp", "uint", 8, unit="degC",
                           conv=logb.Linear(-40, 1)),
            ],
            axis_step=10_000_000)      # one record every 10 ms, in ns ticks
        w.write_meta("vehicle.vin", "WVWZZZ1JZXW000001")
        w.write(s, [(3200, 80), (3204, 80), (3208, 81)], base=0)

Records are tuples in field order or dicts by field name. Each write() call is
one DATA frame, and a frame is the unit the format is written in (SPEC.md §8.1):
batch a few thousand records per call rather than one.

Not implemented: HOLD frames (§6.10), and the zstd and lz4 codecs, which the
standard library lacks. Codec "deflate" is available everywhere and "zstd" is
used when the zstandard package is installed.
"""

import struct
import uuid as _uuid
import zlib

__all__ = [
    "Writer", "Stream", "Field", "Run",
    "Linear", "Rational", "Table", "ValueToText", "RangeToText",
]

VERSION_MAJOR = 0
VERSION_MINOR = 1

MAGIC = b"\x89LOGB\r\n\x1a"
SYNC_PATTERN = b"LOGBSYNC\xa7\x3e\x91\xd2\x5c\x68\x0b\xf4"

FRAME_SYNC = 0x01
FRAME_SCHEMA = 0x10
FRAME_META = 0x11
FRAME_ATTACH = 0x12
FRAME_RUN = 0x13
FRAME_DATA = 0x20
FRAME_INDEX = 0x30
FRAME_END = 0x40

AXIS_KINDS = {"time": 0, "frequency": 1, "angle": 2, "distance": 3, "index": 4, "other": 5}
AXIS_MODES = {"implicit": 0, "explicit": 1, "log": 2}
TYPES = {"uint": 0, "sint": 1, "float": 2, "bool": 3, "bytes": 4, "string": 5, "complex": 6}
CODECS = {"none": 0, "zstd": 1, "deflate": 3}


# --- crc32c -----------------------------------------------------------------

def _crc_table():
    t = []
    for i in range(256):
        c = i
        for _ in range(8):
            c = (c >> 1) ^ 0x82F63B78 if c & 1 else c >> 1
        t.append(c)
    return t


_CRC = _crc_table()


def crc32c(data, crc=0):
    """CRC-32 Castagnoli. Pass the previous result as crc to continue a sum."""
    c = crc ^ 0xFFFFFFFF
    t = _CRC
    for b in data:
        c = t[(c ^ b) & 0xFF] ^ (c >> 8)
    return c ^ 0xFFFFFFFF


# --- little-endian encoding of the format's own structures --------------------

def _u8(v): return struct.pack("<B", v)
def _i8(v): return struct.pack("<b", v)
def _u16(v): return struct.pack("<H", v)
def _u32(v): return struct.pack("<I", v)
def _u64(v): return struct.pack("<Q", v)
def _i64(v): return struct.pack("<q", v)
def _f64(v): return struct.pack("<d", v)


def _str(s):
    b = s.encode("utf-8")
    return _u32(len(b)) + b


def _kv(m):
    # Sorted, as the Go writer does, so identical input gives identical bytes.
    m = m or {}
    out = [_u32(len(m))]
    for k in sorted(m):
        out.append(_str(k))
        out.append(_str(str(m[k])))
    return b"".join(out)


# --- conversions (§7) ---------------------------------------------------------

class Linear:
    """a + b*x"""

    def __init__(self, a, b):
        self.a, self.b = a, b

    def encode(self):
        return _u8(1) + _f64(self.a) + _f64(self.b)


class Rational:
    """(p1x² + p2x + p3) / (p4x² + p5x + p6)"""

    def __init__(self, p1, p2, p3, p4, p5, p6):
        self.p = (p1, p2, p3, p4, p5, p6)

    def encode(self):
        return _u8(2) + b"".join(_f64(p) for p in self.p)


class Table:
    """Lookup by key; interp selects linear interpolation between keys."""

    def __init__(self, pairs, interp=False):
        self.pairs = sorted(dict(pairs).items())
        self.interp = interp

    def encode(self):
        out = [_u8(4 if self.interp else 3), _u32(len(self.pairs))]
        for k, v in self.pairs:
            out.append(_f64(k) + _f64(v))
        return b"".join(out)


class ValueToText:
    """Enum decode: {raw value: text}, and the text for anything else."""

    def __init__(self, texts, default=""):
        self.texts = list(dict(texts).items())
        self.default = default

    def encode(self):
        out = [_u8(5), _u32(len(self.texts))]
        for k, t in self.texts:
            out.append(_f64(k) + _str(t))
        out.append(_str(self.default))
        return b"".join(out)


class RangeToText:
    """Range decode: [(lo, hi, text), ...], and the text for anything else."""

    def __init__(self, ranges, default=""):
        self.ranges = list(ranges)
        self.default = default

    def encode(self):
        out = [_u8(6), _u32(len(self.ranges))]
        for lo, hi, t in self.ranges:
            out.append(_f64(lo) + _f64(hi) + _str(t))
        out.append(_str(self.default))
        return b"".join(out)


# --- schema -------------------------------------------------------------------

class Field:
    """One field of a record (§6.2).

    bits is the width in bits; 0 for a variable-length field, which lives in
    the record tail. offset is the first bit, counted in the field's own byte
    order (§6.2); leave it None to place the field right after the previous
    one. guard=(field, value) makes the field present only in records whose
    field (a name or index) holds the raw value.
    """

    def __init__(self, name, type, bits=None, *, offset=None, big_endian=False,
                 variable=False, unit="", desc="", conv=None, guard=None, meta=None):
        if type not in TYPES:
            raise ValueError("logb: field %r: unknown type %r" % (name, type))
        if bits is None:
            bits = {"bool": 1, "float": 64, "complex": 128}.get(type, 0 if variable else None)
            if bits is None:
                raise ValueError("logb: field %r: %s needs a width in bits" % (name, type))
        self.name = name
        self.type = type
        self.bits = bits
        self.offset = offset
        self.big_endian = big_endian
        self.variable = variable
        self.unit = unit
        self.desc = desc
        self.conv = conv
        self.guard = guard
        self.meta = meta or {}


class Run:
    """A run (§6.5): one dataset within a sweep, with its parameters."""

    def __init__(self, id, index=None, params=None):
        self.id = id
        self.index = id if index is None else index
        self.params = params or {}


class Stream:
    """A registered stream. Create it with Writer.add_stream."""

    def __init__(self, name, fields, *, uuid=None, axis_kind="time",
                 axis_mode="implicit", axis_exp=-9, axis_unit=None,
                 axis_step=None, axis_scale=None, axis_field=None, meta=None):
        if axis_kind not in AXIS_KINDS:
            raise ValueError("logb: stream %r: unknown axis kind %r" % (name, axis_kind))
        if axis_mode not in AXIS_MODES:
            raise ValueError("logb: stream %r: unknown axis mode %r" % (name, axis_mode))
        self.name = name
        self.fields = list(fields)
        self.uuid = _uuid.uuid4() if uuid is None else _as_uuid(uuid)
        self.axis_kind = axis_kind
        self.axis_mode = axis_mode
        self.axis_exp = axis_exp if axis_kind == "time" else 0
        self.axis_unit = ("s" if axis_kind == "time" else "") if axis_unit is None else axis_unit
        time = axis_kind == "time"
        if axis_step is None:
            axis_step = 0 if time else 0.0
        if axis_scale is None:
            axis_scale = 1 if time else 1.0
        # A change of axis_step needs a new segment (§5.4); the writer checks.
        self.axis_step = axis_step
        self.axis_scale = axis_scale
        self.meta = meta or {}
        self._index = {f.name: i for i, f in enumerate(self.fields)}
        self.axis_field = 0 if axis_field is None else self.field_index(axis_field)
        self.id = None

        bit = 0
        for f in self.fields:
            if f.variable:
                f.offset = 0
                continue
            if f.offset is None:
                f.offset = bit
            bit = max(bit, f.offset + f.bits)
        self.record_bits = bit
        self.record_bytes = (bit + 7) // 8
        for f in self.fields:
            if f.guard is not None:
                f.guard = (self.field_index(f.guard[0]), f.guard[1])
        self._validate()

    def field_index(self, f):
        if isinstance(f, int):
            if not 0 <= f < len(self.fields):
                raise ValueError("logb: stream %r has no field %d" % (self.name, f))
            return f
        if f not in self._index:
            raise ValueError("logb: stream %r has no field %r" % (self.name, f))
        return self._index[f]

    def _validate(self):
        # The checks Schema.Validate makes, so that a file this writes is one
        # the reference reader accepts.
        n = self.name
        if self.axis_mode == "log":
            if self.axis_kind == "time":
                raise ValueError("logb: stream %r: a log axis is undefined for time" % n)
            r = self.axis_step
            if not (r > 0) or r == float("inf") or r == 1:
                raise ValueError("logb: stream %r: axis ratio %r is not usable" % (n, r))
        if self.axis_mode == "explicit":
            f = self.fields[self.axis_field]
            if f.type not in ("uint", "sint", "float") or f.variable:
                raise ValueError("logb: stream %r: axis field %r must be numeric" % (n, f.name))
        if self.axis_kind == "time" and not (isinstance(self.axis_step, int)
                                             and isinstance(self.axis_scale, int)):
            raise ValueError("logb: stream %r: a time axis counts integer ticks" % n)
        for i, f in enumerate(self.fields):
            blob = f.type in ("bytes", "string")
            if f.variable:
                if not blob:
                    raise ValueError("logb: field %r: only bytes and string can be variable" % f.name)
                if f.bits != 0:
                    raise ValueError("logb: field %r: a variable field has width 0" % f.name)
            elif blob:
                if f.offset % 8 or f.bits % 8:
                    raise ValueError("logb: field %r: bytes and string must be whole bytes" % f.name)
            elif f.type == "bool":
                if f.bits != 1:
                    raise ValueError("logb: field %r: bool is 1 bit" % f.name)
            elif f.type == "float":
                if f.bits not in (32, 64):
                    raise ValueError("logb: field %r: float is 32 or 64 bits" % f.name)
            elif f.type == "complex":
                if f.bits not in (64, 128):
                    raise ValueError("logb: field %r: complex is 64 or 128 bits" % f.name)
            elif not 1 <= f.bits <= 64:
                raise ValueError("logb: field %r: %s is 1..64 bits" % (f.name, f.type))
            if blob and f.conv is not None:
                raise ValueError("logb: field %r: no conversion applies to %s" % (f.name, f.type))
            if f.type == "complex" and isinstance(f.conv, (Table, ValueToText, RangeToText)):
                raise ValueError("logb: field %r: complex takes linear or rational only" % f.name)
            if f.guard is not None:
                g = self.fields[f.guard[0]]
                if f.guard[0] == i:
                    raise ValueError("logb: field %r guards on itself" % f.name)
                if g.type not in ("uint", "sint", "bool") or g.variable or g.guard is not None:
                    raise ValueError("logb: field %r: guard field %r must be an unguarded "
                                     "uint, sint or bool" % (f.name, g.name))

    def _schema_payload(self):
        time = self.axis_kind == "time"
        out = [
            self.uuid.bytes,
            _str(self.name),
            _u32(self.record_bits),
            _u8(AXIS_KINDS[self.axis_kind]),
            _u8(AXIS_MODES[self.axis_mode]),
            _i8(self.axis_exp),
            _u8(0),
            _str(self.axis_unit),
            _i64(self.axis_step) if time else _f64(self.axis_step),
            _i64(self.axis_scale) if time else _f64(self.axis_scale),
            _u16(self.axis_field),
            _u16(len(self.fields)),
        ]
        for f in self.fields:
            flags = (1 if f.variable else 0) | (2 if f.guard is not None else 0)
            out += [_str(f.name), _u32(f.offset), _u32(f.bits), _u8(TYPES[f.type]),
                    _u8(1 if f.big_endian else 0), _u8(flags), _str(f.unit), _str(f.desc),
                    f.conv.encode() if f.conv is not None else _u8(0)]
            if f.guard is not None:
                out += [_u16(f.guard[0]), _u64(f.guard[1])]
            out.append(_kv(f.meta))
        out.append(_kv(self.meta))
        return b"".join(out)

    def pack(self, record):
        """Pack one record — a tuple in field order or a dict by name — into
        its fixed portion and its tail (§6.4)."""
        if isinstance(record, dict):
            unknown = set(record) - set(self._index)
            if unknown:
                raise ValueError("logb: stream %r has no field %r" % (self.name, unknown.pop()))
            values = [record.get(f.name) for f in self.fields]
        else:
            values = list(record)
            if len(values) != len(self.fields):
                raise ValueError("logb: stream %r: record has %d values, want %d"
                                 % (self.name, len(values), len(self.fields)))

        # Bit n of a little-endian field is bit n of the record read as a
        # little-endian integer, and bit n of a big-endian field is bit
        # (8*size-1-n) of the record read as a big-endian integer (§6.2). So
        # each order is one shift into its own integer, and the two integers
        # are merged at the end.
        le = be = 0
        size = self.record_bytes
        tail = []
        for f, v in zip(self.fields, values):
            if v is None:
                if f.guard is None and not f.variable:
                    raise ValueError("logb: stream %r: field %r has no value" % (self.name, f.name))
                if f.variable:
                    tail.append(_u32(0))
                continue
            if f.variable:
                b = v.encode("utf-8") if f.type == "string" else bytes(v)
                tail.append(_u32(len(b)) + b)
                continue
            raw = _raw_bits(f, v)
            if f.big_endian:
                be |= raw << (8 * size - f.offset - f.bits)
            else:
                le |= raw << f.offset
        fixed = le.to_bytes(size, "little")
        if be:
            fixed = bytes(a | b for a, b in zip(fixed, be.to_bytes(size, "big")))
        return fixed, b"".join(tail)


def _raw_bits(f, v):
    """The field's value as the unsigned integer its bits hold."""
    w = f.bits
    t = f.type
    if t == "uint":
        v = int(v)
        if not 0 <= v < 1 << w:
            raise ValueError("logb: field %r: %d does not fit uint%d" % (f.name, v, w))
        return v
    if t == "sint":
        v = int(v)
        if not -(1 << (w - 1)) <= v < 1 << (w - 1):
            raise ValueError("logb: field %r: %d does not fit sint%d" % (f.name, v, w))
        return v & ((1 << w) - 1)
    if t == "bool":
        return 1 if v else 0
    if t == "float":
        return _float_bits(v, w)
    if t == "complex":
        v = complex(v)
        h = w // 2
        re, im = _float_bits(v.real, h), _float_bits(v.imag, h)
        # The real part comes first (§6.3), which in each byte order's own
        # numbering is the low half for little and the high half for big.
        return (re << h | im) if f.big_endian else (im << h | re)
    # Fixed bytes and string: whole bytes at a byte offset, so they are
    # placed as a little-endian integer whatever byte_order says.
    b = v.encode("utf-8") if t == "string" else bytes(v)
    n = w // 8
    if len(b) > n or (t == "bytes" and len(b) != n):
        raise ValueError("logb: field %r: %d bytes for a %d-byte field" % (f.name, len(b), n))
    if b"\x00" in b and t == "string":
        raise ValueError("logb: field %r: a fixed string cannot contain NUL" % f.name)
    b = b.ljust(n, b"\x00")
    if f.big_endian:
        return int.from_bytes(b, "big")
    return int.from_bytes(b, "little")


def _float_bits(v, w):
    if w == 32:
        return struct.unpack("<I", struct.pack("<f", v))[0]
    return struct.unpack("<Q", struct.pack("<d", v))[0]


def _as_uuid(u):
    if isinstance(u, _uuid.UUID):
        return u
    if isinstance(u, (bytes, bytearray)):
        return _uuid.UUID(bytes=bytes(u))
    return _uuid.UUID(str(u))


def _compress(codec, data):
    if codec == "deflate":
        # Raw DEFLATE, no zlib header: what Go's compress/flate reads.
        c = zlib.compressobj(wbits=-15)
        return c.compress(data) + c.flush()
    if codec == "zstd":
        import zstandard
        return zstandard.ZstdCompressor().compress(data)
    raise ValueError("logb: unknown codec %r" % codec)


# --- writer -------------------------------------------------------------------

class Writer:
    """Appends Logb frames to a binary file object. It never seeks.

    codec is "none", "deflate" or "zstd"; transpose groups byte i of every
    record together before compression, which helps slowly varying data
    (§8). Both apply to every DATA frame this writer emits.
    """

    def __init__(self, f, codec="none", transpose=False):
        if codec not in CODECS:
            raise ValueError("logb: unknown codec %r" % codec)
        self.f = f
        self.codec = codec
        self.transpose = transpose
        self.off = 0
        self.streams = []
        self.runs = []
        self.seq = 0
        self.in_seg = False
        self._index = {}       # uuid -> [(offset, base, count, run_id)], in first-write order
        self._cur_run = {}     # uuid -> run_id being written in this segment
        self._past_run = {}    # uuid -> run ids finished in this segment
        self._seg_step = {}    # uuid -> axis_step this segment's schema declared
        self.closed = False
        hdr = MAGIC + _u16(VERSION_MAJOR) + _u16(VERSION_MINOR)
        self._write(hdr + _u32(crc32c(hdr)))

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        # On an exception the file is left without INDEX and END frames, which
        # is a valid file holding every frame written so far (rule 2).
        if exc_type is None:
            self.close()

    def _write(self, b):
        self.f.write(b)
        self.off += len(b)

    def _frame(self, ftype, stream_id, payload):
        hdr = _u32(len(payload)) + _u8(ftype) + _u8(0) + _u16(stream_id)
        self._write(hdr + payload + _u32(crc32c(payload, crc32c(hdr))))

    def add_stream(self, name, fields, **kw):
        """Declare a stream and return it. See Stream for the axis keywords:

        axis_kind   "time" (default), "frequency", "angle", "distance",
                    "index" or "other"
        axis_mode   "implicit" (default): base + i*axis_step, zero bytes
                    "explicit": base + field*axis_scale, axis_field names it
                    "log": base * axis_step**i, not for time
        axis_exp    time only: one tick is 10**axis_exp s, default -9 (ns)
        uuid        the stream's identity across segments and files; random
                    if not given. Persist it to make files concatenate into
                    one recording (§6.6).

        For a time axis, axis_step, axis_scale and every base are integer
        ticks; for any other kind they are floats in axis_unit.
        """
        s = Stream(name, fields, **kw)
        if any(o.uuid == s.uuid for o in self.streams):
            raise ValueError("logb: stream %r: uuid already in use" % name)
        s.id = len(self.streams)
        self.streams.append(s)
        if self.in_seg:
            self._write_schema(s)
        return s

    def add_run(self, id, index=None, params=None):
        """Declare a run (§6.5). It is restated in every segment until end_run."""
        r = Run(id, index, params)
        self.runs.append(r)
        if self.in_seg:
            self._write_run(r)
        return r

    def end_run(self, id):
        """Stop restating a run once its last record is written."""
        self.runs = [r for r in self.runs if r.id != id]

    def begin_segment(self, wall_time_ns=0):
        """Start a segment: a SYNC frame and every schema and run restated,
        which is what lets a reader start here (§4). How often is writer
        policy; write() starts the first one if you have not."""
        self._frame(FRAME_SYNC, 0, SYNC_PATTERN + _u64(self.seq) + _i64(wall_time_ns))
        self.seq += 1
        self.in_seg = True
        self._cur_run.clear()
        self._past_run.clear()
        self._seg_step.clear()
        for s in self.streams:
            self._write_schema(s)
        for r in self.runs:
            self._write_run(r)

    def _write_schema(self, s):
        self._seg_step[s.uuid] = s.axis_step
        self._frame(FRAME_SCHEMA, s.id, s._schema_payload())

    def _write_run(self, r):
        self._frame(FRAME_RUN, 0, _u32(r.id) + _u32(r.index) + _kv(r.params))

    def write_meta(self, key, value):
        """File-level key/value metadata, e.g. time.base = unix (§5.2)."""
        self._frame(FRAME_META, 0, _str(key) + _str(value))

    def write_attach(self, name, data):
        """Embed a file: a DBC, a netlist, a calibration."""
        if isinstance(data, str):
            data = data.encode("utf-8")
        self._frame(FRAME_ATTACH, 0, _str(name) + _u32(len(data)) + bytes(data))

    def write(self, stream, records, base=0, run_id=0):
        """Write records as one DATA frame.

        base is the axis value of the first record (implicit axes) or the
        value explicit axis fields count from: integer ticks for a time axis,
        a float otherwise.
        """
        fixed, tails = [], []
        for r in records:
            a, b = stream.pack(r)
            fixed.append(a)
            tails.append(b)
        self.write_raw(stream, b"".join(fixed) + b"".join(tails), len(fixed), base, run_id)

    def write_raw(self, stream, data, count, base=0, run_id=0):
        """Write count records already packed to the stream's layout: every
        fixed portion, then every tail (§6.4)."""
        if self.closed:
            raise ValueError("logb: writer is closed")
        if stream not in self.streams:
            raise ValueError("logb: stream %r was not added to this writer" % stream.name)
        if not self.in_seg:
            self.begin_segment(0)
        u = stream.uuid

        if self._seg_step.get(u, stream.axis_step) != stream.axis_step:
            raise ValueError("logb: stream %r: axis_step changed inside a segment; "
                             "call begin_segment first (§5.4)" % stream.name)

        cur = self._cur_run.get(u)
        if cur != run_id:
            past = self._past_run.setdefault(u, set())
            if run_id in past:
                raise ValueError("logb: stream %r: run %d resumed after another run in "
                                 "this segment; runs are contiguous (§6.5)" % (stream.name, run_id))
            if cur is not None:
                past.add(cur)
            self._cur_run[u] = run_id

        n = count * stream.record_bytes
        if len(data) < n:
            raise ValueError("logb: stream %r: %d records need %d bytes, got %d"
                             % (stream.name, count, n, len(data)))
        filt = 0
        payload = bytes(data)
        if self.transpose:
            filt = 1
            rb = stream.record_bytes
            if rb > 1:
                payload = b"".join(payload[c:n:rb] for c in range(rb)) + payload[n:]
        raw_size = len(payload)
        if self.codec != "none":
            payload = _compress(self.codec, payload)

        time = stream.axis_kind == "time"
        self._index.setdefault(u, []).append((self.off, base, count, run_id, time))
        self._frame(FRAME_DATA, stream.id,
                    (_i64(base) if time else _f64(base)) + _u32(count) + _u32(run_id)
                    + _u8(CODECS[self.codec]) + _u8(filt) + _u16(0) + _u64(raw_size) + payload)

    def close(self):
        """Write the INDEX and END frames. The file object stays open."""
        if self.closed:
            return
        start = self.off
        out = [_u32(len(self._index))]
        for u, entries in self._index.items():
            out += [u.bytes, _u32(len(entries))]
            for off, base, count, run_id, time in entries:
                out += [_u64(start - off), _i64(base) if time else _f64(base),
                        _u32(count), _u32(run_id)]
        self._frame(FRAME_INDEX, 0, b"".join(out))
        self._frame(FRAME_END, 0, b"")
        self.closed = True
        if hasattr(self.f, "flush"):
            self.f.flush()
