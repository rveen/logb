#!/usr/bin/env python3
"""Generate the Logb structure diagrams in doc/ from the BNF productions.

One diagram per production group in BNF.md, written beside this script:

    python3 doc/gen.py

The SVGs are self-contained — no external assets, no fonts to fetch — and carry
both light and dark palettes behind a prefers-color-scheme query, so they render
on GitHub either way. Colour is consistent across the set: blue for framing and
counts, green for payload and data, purple for strings and variable-length
regions, red for the CRC and for what a reader must reject, amber for fixed magic
bytes and UUIDs. A dashed border means variable-length or optional.

Edit the diagrams here rather than in the SVGs, and re-run.
"""

import os

OUT = os.path.dirname(os.path.abspath(__file__))
W = 880
X0 = 40
CW = W - 2 * X0

STYLE = """  <style>
    .bg   { fill: #ffffff; }
    .fg   { fill: #1f2328; }
    .mut  { fill: #656d76; }
    .cell { fill: #ffffff; stroke: #d0d7de; }
    .dash { stroke-dasharray: 4 3; }
    .axis { stroke: #8c959f; fill: none; }
    .k1   { fill: #0969da; fill-opacity: 0.12; stroke: #0969da; }
    .k2   { fill: #1a7f37; fill-opacity: 0.12; stroke: #1a7f37; }
    .k3   { fill: #8250df; fill-opacity: 0.12; stroke: #8250df; }
    .k4   { fill: #cf222e; fill-opacity: 0.12; stroke: #cf222e; }
    .k5   { fill: #9a6700; fill-opacity: 0.13; stroke: #9a6700; }
    .l1   { fill: #0969da; }
    .l2   { fill: #1a7f37; }
    .l3   { fill: #8250df; }
    .l4   { fill: #cf222e; }
    .l5   { fill: #9a6700; }
    .s1   { stroke: #0969da; fill: none; }
    .s4   { stroke: #cf222e; fill: none; }
    text  { font-family: ui-monospace, "SF Mono", Menlo, Consolas, monospace; }
    .t    { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    @media (prefers-color-scheme: dark) {
      .bg   { fill: #0d1117; }
      .fg   { fill: #e6edf3; }
      .mut  { fill: #8b949e; }
      .cell { fill: #0d1117; stroke: #30363d; }
      .axis { stroke: #6e7681; }
      .k1   { fill: #79c0ff; fill-opacity: 0.15; stroke: #79c0ff; }
      .k2   { fill: #3fb950; fill-opacity: 0.15; stroke: #3fb950; }
      .k3   { fill: #d2a8ff; fill-opacity: 0.15; stroke: #d2a8ff; }
      .k4   { fill: #ff7b72; fill-opacity: 0.15; stroke: #ff7b72; }
      .k5   { fill: #d29922; fill-opacity: 0.16; stroke: #d29922; }
      .l1   { fill: #79c0ff; }
      .l2   { fill: #3fb950; }
      .l3   { fill: #d2a8ff; }
      .l4   { fill: #ff7b72; }
      .l5   { fill: #d29922; }
      .s1   { stroke: #79c0ff; fill: none; }
      .s4   { stroke: #ff7b72; fill: none; }
    }
  </style>
"""


def wrap(s, n):
    out, line = [], ""
    for word in s.split(" "):
        if line and len(line) + 1 + len(word) > n:
            out.append(line)
            line = word
        else:
            line = word if not line else line + " " + word
    if line:
        out.append(line)
    return out


def esc(s):
    return (s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")
             .replace("->", "&#8594;"))


class Doc:
    def __init__(self, aria, title, subs=()):
        self.aria = aria
        self.p = []
        self.y = 30
        self.p.append('<text class="t fg" x="%d" y="%d" font-size="15" '
                      'font-weight="600">%s</text>' % (X0, self.y, esc(title)))
        self.y += 19
        for s in subs:
            # BNF productions are pre-broken by hand; prose gets wrapped.
            for line in ([s] if s.startswith("<") or s.startswith(" ")
                         else wrap(s, int(CW / (12 * 0.55)))):
                self.p.append('<text class="t mut" x="%d" y="%d" font-size="12">%s</text>'
                              % (X0, self.y, esc(line)))
                self.y += 17
        self.y += 12

    def gap(self, n=10):
        self.y += n

    def note(self, s, cls="t mut", size=11.5, x=X0):
        # Wrap to the content width. Sans is ~0.53em per char, mono ~0.60em;
        # stay conservative so nothing ever runs off the right edge.
        per = size * (0.55 if cls.startswith("t ") else 0.60)
        maxc = int((X0 + CW - x) / per)
        self.y += 4
        for line in wrap(s, maxc):
            self.p.append('<text class="%s" x="%d" y="%d" font-size="%s">%s</text>'
                          % (cls, x, self.y, size, esc(line)))
            self.y += 17

    def label(self, s, cls="t fg"):
        self.p.append('<text class="%s" x="%d" y="%d" font-size="12.5" '
                      'font-weight="600">%s</text>' % (cls, X0, self.y + 11, esc(s)))
        self.y += 22

    def row(self, cells, x=X0, w=CW, h=48, offsets=True, rowlabel=None):
        """cells: dicts with name, typ, u (width weight), cls, dash, off."""
        if rowlabel is not None:
            self.p.append('<text class="t mut" x="%d" y="%d" font-size="11.5">%s</text>'
                          % (X0, self.y + 11, esc(rowlabel)))
            self.y += 20
        if offsets and any(c.get("off") for c in cells):
            self.y += 12
        top = self.y
        tot = sum(c.get("u", 1) for c in cells)
        cx = x
        boxes = []
        for c in cells:
            cw = w * c.get("u", 1) / tot
            klass = "cell " + c.get("cls", "")
            if c.get("dash"):
                klass += " dash"
            if c.get("cls") != "none":   # "none" is an invisible spacer
                self.p.append('<rect class="%s" x="%.1f" y="%d" width="%.1f" '
                              'height="%d" rx="3"/>'
                              % (klass.strip(), cx, top, cw - 2, h))
            if c.get("off"):
                self.p.append('<text class="mut" x="%.1f" y="%d" font-size="9.5">%s</text>'
                              % (cx, top - 5, esc(c["off"])))
            mid = cx + (cw - 2) / 2
            ty = top + h / 2 + (-3 if c.get("typ") else 4)
            self.p.append('<text class="fg" x="%.1f" y="%.1f" font-size="%s" '
                          'text-anchor="middle">%s</text>'
                          % (mid, ty, c.get("fs", 11.5), esc(c["name"])))
            if c.get("typ"):
                self.p.append('<text class="mut" x="%.1f" y="%.1f" font-size="10" '
                              'text-anchor="middle">%s</text>'
                              % (mid, ty + 14, esc(c["typ"])))
            boxes.append((cx, top, cw - 2, h))
            cx += cw
        self.y = top + h + 14
        return boxes

    def expand(self, box, cls="k1"):
        """Draw a widening callout from box down to the next full-width row."""
        bx, by, bw, bh = box
        y1 = by + bh
        y2 = self.y + 8
        self.p.append('<path class="%s" fill-opacity="0.06" stroke-opacity="0.5" '
                      'd="M%.1f %d L%.1f %d L%d %.1f L%d %.1f Z"/>'
                      % (cls, bx, y1, bx + bw, y1, X0 + CW, y2, X0, y2))
        self.y = y2

    def brace(self, x1, x2, y, text, cls="s1", tcls="l1"):
        """An underbrace: legs point up at the cells above, caption below."""
        self.p.append('<path class="%s" d="M%.1f %.1f L%.1f %.1f L%.1f %.1f '
                      'L%.1f %.1f"/>' % (cls, x1, y - 7, x1, y, x2, y, x2, y - 7))
        self.p.append('<text class="%s" x="%.1f" y="%.1f" font-size="10.5" '
                      'text-anchor="middle">%s</text>'
                      % (tcls, (x1 + x2) / 2, y + 15, esc(text)))
        self.y = max(self.y, y + 30)

    def raw(self, s):
        self.p.append(s)

    def save(self, name):
        h = int(self.y + 16)
        body = "\n  ".join(self.p)
        svg = ('<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" '
               'width="%d" height="%d" role="img" aria-label="%s">\n%s'
               '  <rect class="bg" width="%d" height="%d"/>\n  %s\n</svg>\n'
               % (W, h, W, h, esc(self.aria), STYLE, W, h, body))
        with open(os.path.join(OUT, name), "w") as f:
            f.write(svg)
        print(name, h)


def C(name, typ=None, u=1, cls="", dash=False, off=None, fs=None):
    d = {"name": name, "u": u, "cls": cls}
    if typ:
        d["typ"] = typ
    if dash:
        d["dash"] = True
    if off:
        d["off"] = off
    if fs:
        d["fs"] = fs
    return d


# --------------------------------------------------------------- 1. file
d = Doc("The overall structure of a Logb file",
        "A Logb file",
        ["<file> ::= <file-header> <segment>* [ <index-frame> ] [ <end-frame> ]",
         "Nothing points forward, so the whole tail is effectively optional: a file cut at any byte still holds every complete frame before the cut."])
b = d.row([C("file-header", "16 bytes", 1.1, "k5"),
           C("segment", "one or more", 3.4, "k1"),
           C("index-frame", "optional", 1.1, "k3", dash=True),
           C("end-frame", "optional", 1.0, "k3", dash=True)])
seg = b[1]
d.gap(6)
d.expand(seg)
d.row([C("sync-frame", "0x01, exactly one", 1.3, "k1"),
       C("schema-frame", "0x10, one or more", 1.4, "k1"),
       C("run-frame", "0x13, zero or more", 1.3, "k1", dash=True),
       C("segment-body", "zero or more", 2.0, "k2")],
      rowlabel="<segment>  ::=  a resynchronisation point, its schemas, then bodies")
body = None
d.gap(2)
d.row([C("meta-frame", "0x11", 1, "k2"),
       C("attach-frame", "0x12", 1, "k2"),
       C("data-frame", "0x20", 1, "k2"),
       C("other-frame", "unknown type, skipped via payload_len", 1.8, "cell", dash=True)],
      h=42,
      rowlabel="<segment-body>  ::=  in any order, any number of times")
d.note("A segment is the unit of recovery: schemas are rebound at every SYNC frame, so a reader that lands anywhere can scan forward to the next one and decode from there. Concatenation appends a second file minus its header — which is why an index or end frame may also turn up mid-file.")
d.save("file-structure.svg")

# --------------------------------------------------------------- 2. file header
d = Doc("The 16-byte Logb file header",
        "File header — 16 bytes, written once",
        ["<file-header> ::= <magic> <version-major> <version-minor> <header-crc>"])
b = d.row([C("magic", "8 bytes", 2.6, "k5", off="+0"),
           C("version_major", "u16 = 0", 1.3, "k1", off="+8"),
           C("version_minor", "u16 = 1", 1.3, "k1", off="+10"),
           C("header_crc", "u32", 1.3, "k4", off="+12")])
d.gap(2)
d.row([C(x, None, 1, "k5", fs=11) for x in
       ["89", "4C", "4F", "47", "42", "0D", "0A", "1A"]],
      x=b[0][0], w=b[0][2] + 2, h=30, offsets=False)
d.raw('<text class="mut" x="%d" y="%.1f" font-size="10.5">'
      '\\x89  L  O  G  B  \\r  \\n  \\x1a  &#8212; the PNG trick: byte 0 is non-ASCII, '
      'and CR LF EOF catch a text-mode transfer that mangled the file</text>'
      % (X0, d.y + 2))
d.y += 20
d.note("header_crc covers bytes 0..11. An unknown version_major is rejected; an unknown higher version_minor is accepted and its unknown frames skipped.")
d.save("file-header.svg")

# --------------------------------------------------------------- 3. frame
d = Doc("The common shape of every Logb frame",
        "Frame — every frame after the file header has this shape",
        ["<frame> ::= <frame-header> <payload> <frame-crc>",
         "12 bytes of overhead. Frames are batches, not records, so this is noise."])
b = d.row([C("payload_len", "u32", 1.15, "k1", off="+0"),
           C("frame_type", "u8", 0.75, "k1", off="+4"),
           C("flags", "u8", 0.75, "k1", off="+5"),
           C("stream_id", "u16", 0.95, "k1", off="+6"),
           C("payload", "payload_len bytes", 3.4, "k2", off="+8"),
           C("crc32c", "u32", 1.0, "k4", off="+8+n")])
d.gap(14)
d.brace(b[0][0], b[4][0] + b[4][2], d.y, "crc32c is computed over all of this — header and payload both", cls="s4", tcls="l4")
d.note("A reader validates the CRC before trusting any payload byte. A frame whose CRC fails, or whose payload_len runs past end of input, terminates the read there.")
d.gap(8)
d.label("frame_type selects the payload production")
types = [("0x01", "SYNC", "k1"), ("0x10", "SCHEMA", "k1"), ("0x11", "META", "k2"),
         ("0x12", "ATTACH", "k2"), ("0x13", "RUN", "k1"), ("0x20", "DATA", "k2"),
         ("0x30", "INDEX", "k3"), ("0x40", "END", "k3"), ("0x50", "SIGN", "cell")]
d.row([C(n, i, 1, c, dash=(n == "SIGN")) for i, n, c in types], h=42, offsets=False)
d.note("0x50 is reserved and undefined in v0.1. Any other frame_type is legal and skipped using payload_len — that is the only extension mechanism, and it is enough.")
d.save("frame.svg")

# --------------------------------------------------------------- 4. string / kv
d = Doc("The two composite terminals: string and kv",
        "Composite terminals — these two recur in every payload",
        ["Every count in the format is a u32 that immediately precedes what it counts. There are no NUL terminators anywhere."])
d.label("<string> ::= <u32> <byte>{n}")
d.row([C("n", "u32", 1, "k1"),
       C("UTF-8 bytes", "n bytes, not NUL-terminated", 5, "k3", dash=True)], h=44)
d.gap(6)
d.label("<kv> ::= <u32> <kv-pair>{n}")
b = d.row([C("n", "u32", 1, "k1"),
           C("kv-pair", None, 2, "k3"),
           C("kv-pair", None, 2, "k3"),
           C("...", None, 1, "cell", dash=True),
           C("kv-pair", None, 2, "k3")], h=44)
d.gap(4)
d.expand(b[1], "k3")
d.row([C("key", "<string>", 1, "k3"), C("value", "<string>", 1, "k3")], h=42,
      rowlabel="<kv-pair> ::= <string> <string>   — keys sorted, so the encoding of a map is deterministic")
d.save("terminals.svg")

# --------------------------------------------------------------- 5. sync
d = Doc("The SYNC frame payload",
        "SYNC — 0x01 — 32-byte payload",
        ["<sync-payload> ::= <sync-pattern> <segment-seq> <wall-time-ns>"])
b = d.row([C("sync_pattern", "16 fixed bytes", 2.4, "k5", off="+0"),
           C("segment_seq", "u64, monotonic from 0", 1.5, "k1", off="+16"),
           C("wall_time_ns", "i64, 0 if unknown", 1.5, "k1", off="+24")])
d.gap(2)
d.row([C(x, None, 1, "k5", fs=9.5) for x in
       ["4C", "4F", "47", "42", "53", "59", "4E", "43",
        "A7", "3E", "91", "D2", "5C", "68", "0B", "F4"]],
      x=b[0][0], w=b[0][2] + 2, h=28, offsets=False)
d.raw('<text class="mut" x="%d" y="%.1f" font-size="10.5">'
      'L O G B S Y N C, then 8 random bytes &#8212; long enough that it cannot plausibly '
      'occur inside record data</text>' % (X0, d.y + 2))
d.y += 22
d.note("A reader that has lost its place scans for this pattern, steps back 8 bytes to the frame header, validates the CRC, and is synchronised. Every schema binding expires here, so nothing before a SYNC frame is needed to decode anything after it.")
d.save("sync-payload.svg")

# --------------------------------------------------------------- 6. schema
d = Doc("The SCHEMA frame payload",
        "SCHEMA — 0x10 — binds stream_id for the rest of the segment",
        ["<schema-payload> ::= <stream-uuid> <stream-name> <record-bits> <axis-kind> <axis-mode> <axis-exp> <reserved8>",
         "                     <axis-unit> <axis-step> <axis-scale> <axis-field> <field-count> <field>{field_count} <kv>"])
d.row([C("stream_uuid", "16 bytes, opaque", 2.2, "k5", off="+0"),
       C("stream_name", "<string>", 1.7, "k3", off="+16", dash=True),
       C("record_bits", "u32, bit-exact", 1.6, "k1"),
       C("axis_kind", "u8", 0.8, "k2"),
       C("axis_mode", "u8", 0.8, "k2"),
       C("axis_exp", "i8", 0.8, "k2"),
       C("rsv", "u8", 0.5, "cell")])
d.gap(2)
d.row([C("axis_unit", "<string>", 1.5, "k3", dash=True),
       C("axis_step", "i64 ticks or f64", 1.45, "k2"),
       C("axis_scale", "i64 or f64", 1.35, "k2"),
       C("axis_field", "u16", 1.05, "k2"),
       C("field_count", "u16", 1.1, "k1"),
       C("field × field_count", "see below", 2.2, "k4", dash=True),
       C("kv", "stream metadata", 1.3, "k3", dash=True)])
d.gap(6)
d.note("axis_kind: 0 time · 1 frequency · 2 angle · 3 distance · 4 index · 5 other", cls="mut", size=11)
d.note("axis_mode: 0 implicit uniform · 1 explicit field · 2 implicit log. axis_step and axis_scale are i64 ticks when axis_kind is time, f64 otherwise — except that a log axis carries its ratio as f64 regardless. axis_scale and axis_field are meaningful in explicit mode only.", cls="mut", size=11)
d.note("axis_exp is the time tick exponent: one tick = 10^axis_exp seconds.", cls="mut", size=11)
d.gap(4)
d.note("stream_id (in the frame header) is scoped to the segment and expires at the next SYNC. stream_uuid is the identity that survives segments, files, and concatenation — which is why the index groups by uuid and never by id. Equal uuid implies a byte-identical schema.")
d.save("schema-payload.svg")

# --------------------------------------------------------------- 7. field
d = Doc("The layout of one field inside a SCHEMA payload",
        "Field — repeated field_count times inside a SCHEMA payload",
        ["<field> ::= <field-name> <bit-offset> <bit-width> <data-type> <byte-order> <field-flags>",
         "            <unit> <desc> <conversion> [ <guard-field> <guard-value> ] <kv>"])
d.row([C("field_name", "<string>", 1.9, "k3", dash=True),
       C("bit_offset", "u32", 1.2, "k1"),
       C("bit_width", "u32", 1.2, "k1"),
       C("data_type", "u8", 1.0, "k2"),
       C("byte_order", "u8", 1.05, "k2"),
       C("field_flags", "u8", 1.05, "k2")])
d.gap(2)
b = d.row([C("unit", "<string>", 1.3, "k3", dash=True),
           C("desc", "<string>", 1.3, "k3", dash=True),
           C("conversion", "tagged, see below", 1.9, "k2", dash=True),
           C("guard_field", "u16", 1.1, "k4", dash=True),
           C("guard_value", "u64", 1.1, "k4", dash=True),
           C("kv", "field metadata", 1.2, "k3", dash=True)])
gx = b[3][0]
gw = b[4][0] + b[4][2] - gx
d.gap(10)
d.brace(gx, gx + gw, d.y, "present if and only if field_flags bit 1 is set", cls="s4", tcls="l4")
d.note("This is the one place the grammar is not context-free: the parse of a field depends on a byte inside that field.", cls="t l4", size=11.5)
d.gap(10)
d.label("field_flags")
d.row([C("bit 0", "variable-length: the value lives in the tail, bit_width is 0", 3, "k2"),
       C("bit 1", "guarded", 1.1, "k4"),
       C("bits 2..7", "write 0", 1.6, "cell", dash=True)], h=44, offsets=False)
d.gap(4)
d.label("data_type")
d.row([C(n, i, 1, "k2") for i, n in
       [("0", "uint"), ("1", "sint"), ("2", "float"), ("3", "bool"),
        ("4", "bytes"), ("5", "string"), ("6", "complex")]] , h=40, offsets=False)
d.note("byte_order is 0 little, 1 big, and it is per field because a single CAN frame routinely mixes Intel and Motorola signals. Bit numbering inside the record follows the field’s own byte_order. A variable-length field has bit_width 0, contributes nothing to record_bits, and its bit_offset is ignored.")
d.save("schema-field.svg")

# --------------------------------------------------------------- 8. conversion
d = Doc("The seven conversion forms",
        "Conversion — a tagged struct: one type byte, then type-specific parameters",
        ["Raw bits stay raw on disk; the conversion says how to read them. A type byte outside 0..6 is a corrupt schema, not a skippable unknown — there is no length to skip by."])
rows = [
    ([C("00", "type", 0.6, "k5"), C("identity — no parameters", None, 5.4, "cell", dash=True)], None),
    ([C("01", "type", 0.6, "k5"), C("a", "f64", 1.2, "k2"), C("b", "f64", 1.2, "k2"),
      C("linear:  a + b·x", None, 3.0, "cell", dash=True)], None),
    ([C("02", "type", 0.6, "k5")] + [C("p%d" % i, "f64", 0.7, "k2") for i in range(1, 7)] +
     [C("rational", None, 1.2, "cell", dash=True)], None),
    ([C("03", "type", 0.6, "k5"), C("n", "u32", 0.7, "k1"),
      C("table-entry × n", "<f64> key, <f64> val", 3.0, "k2", dash=True),
      C("lookup, no interpolation", None, 1.7, "cell", dash=True)], None),
    ([C("04", "type", 0.6, "k5"), C("n", "u32", 0.7, "k1"),
      C("table-entry × n", "<f64> key, <f64> val", 3.0, "k2", dash=True),
      C("lookup, interpolated", None, 1.7, "cell", dash=True)], None),
    ([C("05", "type", 0.6, "k5"), C("n", "u32", 0.7, "k1"),
      C("v2t-entry × n", "<f64> key, <string> text", 2.6, "k3", dash=True),
      C("default", "<string>", 1.0, "k3", dash=True),
      C("value to text", None, 1.1, "cell", dash=True)], None),
    ([C("06", "type", 0.6, "k5"), C("n", "u32", 0.7, "k1"),
      C("r2t-entry × n", "<f64> lo, <f64> hi, <string> text", 2.6, "k3", dash=True),
      C("default", "<string>", 1.0, "k3", dash=True),
      C("range to text", None, 1.1, "cell", dash=True)], None),
]
for cells, _ in rows:
    d.row(cells, h=40, offsets=False)
    d.y -= 4
d.y += 12
d.note("Every {n} takes its count from the u32 immediately preceding it. A non-identity conversion on a bytes or string field is rejected — the schema, not the value. For complex, only identity, linear, and rational are valid.")
d.save("conversion.svg")

# --------------------------------------------------------------- 9. small payloads
d = Doc("The META, ATTACH, RUN and END payloads",
        "META, ATTACH, RUN, END — the small payloads",
        [])
d.label("META — 0x11 — one key/value pair per frame")
d.row([C("key", "<string>", 1, "k3", dash=True),
       C("value", "<string>", 1, "k3", dash=True)], h=42)
d.note("File-scoped when stream_id is 0, stream-scoped otherwise. Stream and field metadata live inline in the SCHEMA frame as <kv> instead.", size=11)
d.gap(10)
d.label("ATTACH — 0x12 — an embedded file: a DBC, a calibration, a netlist")
d.row([C("attach_name", "<string>, e.g. “engine.dbc”", 1.6, "k3", dash=True),
       C("attach_len", "u32", 0.9, "k1"),
       C("bytes × attach_len", "the file, verbatim", 2.6, "k2", dash=True)], h=42)
d.gap(10)
d.label("RUN — 0x13 — declares what a run_id means")
d.row([C("run_id", "u32, the value DATA frames carry", 1.4, "k1"),
       C("run_index", "u32, ordinal within the sweep", 1.4, "k1"),
       C("kv", "the parameter set: “R1”=“1.0e3”, “temp”=“27”", 2.2, "k3", dash=True)], h=42)
d.note("A logger writes run_id = 0 forever and never emits a RUN frame; the concept costs it four bytes per batch and no complexity.", size=11)
d.gap(10)
d.label("END — 0x40 — empty payload")
d.row([C("payload_len = 0 — nothing at all", None, 1, "cell", dash=True)], h=36)
d.note("An END frame states that a writer closed cleanly at that point: a statement about the past, not a command about the future. A reader that finds bytes after one MUST continue scanning — which is exactly what a concatenated file looks like.")
d.save("small-payloads.svg")

# --------------------------------------------------------------- 10. data
d = Doc("The DATA frame payload",
        "DATA — 0x20 — a batch of records",
        ["<data-payload> ::= <axis-base> <record-count> <run-id> <codec> <filter> <reserved16> <raw-size> <records>"])
d.row([C("axis_base", "i64 ticks or f64", 1.7, "k2", off="+0"),
       C("record_count", "u32", 1.3, "k1", off="+8"),
       C("run_id", "u32", 1.0, "k1", off="+12"),
       C("codec", "u8", 0.8, "k5", off="+16"),
       C("filter", "u8", 0.8, "k5", off="+17"),
       C("rsv", "u16", 0.6, "cell", off="+18"),
       C("raw_size", "u64", 1.1, "k1", off="+20"),
       C("records", "to end of payload", 2.4, "k4", off="+28", dash=True)])
d.gap(6)
d.note("codec: 0 none · 1 zstd (the default) · 2 lz4 · 3 deflate.   filter: 0 none · 1 transpose.   raw_size is the decoded size, for a one-shot allocation.", cls="mut", size=11)
d.gap(8)
d.note("A reader that meets a codec or filter it does not know MUST reject that frame and MUST NOT return its records — unlike an unknown frame type, which is skippable because its meaning is unknown. Because each frame carries its own axis_base and run_id, a DATA frame is independently decodable given its schema, and that is what makes resynchronisation work.")
d.gap(8)
d.note("payload_len sits in the frame header and crc32c in the tail, so the batch must be complete before the first byte is written. A writer therefore accumulates records in memory and emits a whole frame at once: batching is not an optimisation here, it is the unit in which the format is written.", cls="t l1", size=11.5)
d.save("data-payload.svg")

# --------------------------------------------------------------- 11. record region
d = Doc("The decoded record region of a DATA frame",
        "Record region — what the records bytes hold after decompression and de-transposition",
        ["<record-region> ::= <fixed>{record_count} <tail>{record_count}"])
b = d.row([C("fixed 0", None, 1, "k2"), C("fixed 1", None, 1, "k2"),
           C("fixed 2", None, 1, "k2"),
           C("…", None, 0.6, "cell", dash=True),
           C("fixed n-1", None, 1, "k2"),
           C("tail 0", None, 1.1, "k3", dash=True),
           C("tail 1", None, 1.1, "k3", dash=True),
           C("…", None, 0.6, "cell", dash=True),
           C("tail n-1", None, 1.1, "k3", dash=True)], h=44)
fx1, fx2 = b[0][0], b[4][0] + b[4][2]
tx1, tx2 = b[5][0], b[8][0] + b[8][2]
d.gap(8)
by = d.y
d.brace(fx1, fx2, by, "each ceil(record_bits / 8) bytes", cls="s1", tcls="l1")
d.raw('<path class="s4" d="M%.1f %.1f L%.1f %.1f L%.1f %.1f L%.1f %.1f"/>'
      % (tx1, by - 7, tx1, by, tx2, by, tx2, by - 7))
d.raw('<text class="l4" x="%.1f" y="%.1f" font-size="10.5" text-anchor="middle">'
      'appended untransposed</text>' % ((tx1 + tx2) / 2, by + 15))
d.y += 22
d.note("filter=transpose covers the fixed region only. All fixed portions precede all tails; they are never interleaved. This is what lets a reader seek to record i in a batch by multiplication — and it is why one variable-length field costs the batch its seekability, since the tails must then be walked linearly.")
d.gap(10)
d.label("<tail> — one per record, only if the schema declares variable-length fields")
d.row([C("len", "u32", 0.8, "k1"), C("bytes × len", "first variable field", 2.2, "k3", dash=True),
       C("len", "u32", 0.8, "k1"), C("bytes × len", "second variable field", 2.2, "k3", dash=True),
       C("…", None, 0.7, "cell", dash=True)], h=42)
d.note("In field-declaration order. A field with no bytes writes a zero length; it is not omitted. An absent field — including one whose guard is unsatisfied — still occupies its slot, because the tail must stay walkable without evaluating any guard.", size=11)
d.gap(10)
d.label("<fixed> — a bit field, not a byte structure")
d.row([C("field A", "bits 0..11", 1.3, "k2"), C("field B", "bits 12..12", 0.9, "k2"),
       C("field C", "bits 13..28, may overlap", 2.0, "k2"),
       C("padding", "unused bits are legal", 1.4, "cell", dash=True)], h=42)
d.note("Fields are placed by bit_offset and bit_width. They need not be byte-aligned and they may overlap. The grammar cannot express this layer; SPEC.md §6.2’s conformance vectors are the specification for it.", size=11)
d.save("record-region.svg")

# --------------------------------------------------------------- 12. index
d = Doc("The INDEX frame payload",
        "INDEX — 0x30 — purely an accelerator",
        ["<index-payload> ::= <stream-count> <index-group>{stream_count}"])
b = d.row([C("stream_count", "u32", 1.1, "k1"),
           C("index-group", None, 1.8, "k2"),
           C("index-group", None, 1.8, "k2"),
           C("…", None, 0.8, "cell", dash=True),
           C("index-group", None, 1.8, "k2")], h=44)
d.gap(4)
d.expand(b[1], "k2")
b2 = d.row([C("stream_uuid", "16 bytes — grouped by uuid, never by stream_id", 2.4, "k5"),
            C("entry_count", "u32", 1.0, "k1"),
            C("index-entry", None, 1.2, "k3"),
            C("…", None, 0.6, "cell", dash=True),
            C("index-entry", None, 1.2, "k3")], h=44,
           rowlabel="<index-group> ::= <stream-uuid> <entry-count> <index-entry>{entry_count}")
d.gap(4)
d.expand(b2[2], "k3")
d.row([C("back_offset", "u64, bytes backwards from this frame’s start", 2.4, "k4"),
       C("first_axis", "i64 or f64, per axis_kind", 1.6, "k2"),
       C("record_count", "u32", 1.0, "k1"),
       C("run_id", "u32", 1.0, "k1")], h=46,
      rowlabel="<index-entry> ::= <back-offset> <first-axis> <record-count> <run-id>")
d.note("The offset is backwards, and that is the whole design: nothing points forward, so an index written at the end of a file references only bytes that already exist. A reader rebuilds the index by scanning and MUST NOT trust it over the frames.", cls="t l4", size=11.5)
d.save("index-payload.svg")
