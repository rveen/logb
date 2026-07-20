# GNSS, time, and the two clocks

This is the long-form answer to a narrow question: *how does Logb store GNSS
data?* The short answer — the normative names and units — is in
[SPEC.md §6.8](SPEC.md). This document is the reasoning, because several of the
choices look arbitrary until you have seen what they are reacting to.

Nothing here needs a format change. That is the finding, not a disclaimer.

## Why this document exists

MDF 4.3 ships **GNSS Data Storage (v1.0.0)** as an *associated standard*: not a
block type, not a wire change, but an agreement about how positional and
navigational data is *identified*, so that any conforming tool can find latitude
without guessing which channel it is.

Logb's equivalent of an associated standard is a convention written down. This is
that, in the same shape as [§6.7](SPEC.md)'s event streams: three stream names, a
field vocabulary, some metadata keys, and no new machinery.

The interesting part is that GNSS is the payload where several pieces of Logb
that were designed independently turn out to interlock — the raw-plus-conversion
model (rule 5), the late-clock anchor (§5.2), and the explicit axis (§5) each
solve a specific GNSS problem, and none of them was put there for GNSS.

## GNSS is three things at once

A receiver emits three kinds of data with different shapes, and trying to put
them in one stream is the first mistake:

| | rate | shape |
|---|---|---|
| **Position solution** | 1–20 Hz, sporadic | one record per epoch |
| **Status and quality** | same epochs | one record per epoch |
| **Raw observables** | same epochs | *N satellites* per epoch, N varying |

So: `gnss.pvt`, `gnss.status`, `gnss.raw`. Three streams, each with its own
schema, which is what streams are for (§6). Splitting status from position is not
fastidiousness — DOP values and satellite counts are constant across long
stretches and compress to almost nothing when they are not interleaved with
changing coordinates.

## Latitude is an integer

The instinct is to store latitude as a float in degrees. Do not.

`f32` degrees is a real and recurring GNSS bug, and it is worth seeing the size
of it. An `f32` has a 24-bit mantissa, so its resolution scales with magnitude:

| value | `f32` resolution | on the ground |
|---|---|---|
| 0° | 1.2e-7° | 1 cm |
| 45° | 5.4e-6° | **60 cm** |
| 180° | 2.1e-5° | **2.4 m** |

A format that stores degrees as `f32` silently loses more precision the further
you drive from the equator and the prime meridian. It looks fine in testing near
0,0 and degrades on the other side of the world.

`f64` is precise enough and is still the wrong answer, because it is not what the
receiver produced. u-blox `UBX-NAV-PVT` carries latitude and longitude as `i32`
in units of 1e-7 degrees, heights as `i32` millimetres, velocities as `i32` mm/s,
heading as `i32` in 1e-5 degrees. Store those integers and declare the
conversion:

```
lat    i32   Linear{B: 1e-7}    "deg"
lon    i32   Linear{B: 1e-7}    "deg"
h_ell  i32   Linear{B: 0.001}   "m"
vel_n  i32   Linear{B: 0.001}   "m/s"
```

`i32` at 1e-7° resolves 1.11 cm and spans ±214.7°, which covers the globe with
room to spare; `i32` millimetres spans ±2147 km, which covers any altitude
anything will ever log.

This is **rule 5** — raw is preserved — doing exactly what it was written for. The
bytes in the file are the bytes the receiver put on the wire, a read-modify-write
round trip is byte-identical, and the physical value is derived rather than
stored. The float never enters the file, so the float's rounding never enters
the file either. It also makes the `f32` mistake *unavailable* rather than merely
discouraged, which is the stronger form of the fix.

## The two clocks

This is the part that is genuinely subtle, and the part every GNSS logger gets
wrong at least once.

**A GNSS receiver is simultaneously data and the clock that dates the file.**
Those are two different timestamps and they must not be merged:

- **When the logger recorded the message.** This is the stream's axis. The
  logger's own clock, `time.base = monotonic` (§5.2) if it booted without an RTC,
  which is the normal case for a logger that is waiting on GNSS for the time in
  the first place.
- **When the receiver says the fix was valid.** This is *data*: an ordinary
  field, `itow` (GPS time of week) or a UTC field, produced by the receiver and
  carried in the message.

Merging them — using receiver time as the axis — throws away something real and
measurable: the difference between the two is receiver-to-logger latency, tens of
milliseconds, and *variable*. On a serial link it moves with message length and
buffer state. A vehicle at 100 km/h covers 28 cm per 10 ms, so this is not a
rounding concern for anyone doing lane-level work. Store both and the latency is
recoverable; store one and it is gone.

The binding between the two is §5.2's anchor:

```
time.anchor = <monotonic_ns>:<unix_ns>
```

emitted as a META frame **after** the records it retroactively dates. This is the
mechanism [SPEC.md §5.2](SPEC.md) already describes, and GNSS is its intended
source — the spec mentions "a GPS or NTP fix" as the triggering event. What is
new here is only that the same receiver is also filling a data stream.

Multiple anchors are allowed and a GNSS logger should emit them periodically: a
reader fits them to recover the logger's clock drift, which on a cheap crystal is
tens of ppm and worth correcting.

### Leap seconds, and why `tai` is there

GPS time does not have leap seconds. UTC does. They differ by an integer number
of seconds — **18 since 2017-01-01** — and that number changes.

`time.base = unix` (§5.2) smears leap seconds, which is fine for a drive
recording and wrong for anything being correlated against scientific data.
`time.base = tai` is the honest option and exists for this. A file that stores
GPS time-of-week as a field and declares `tai` as its base has thrown nothing
away; one that converts to UTC and stores the result has baked in a leap-second
table that was current on the day the file was written.

If a receiver supplies the current GPS–UTC offset — most do — store it, as a
field or as META `gnss.leap_s`. It is the one number that makes the conversion
reversible later.

## The axis is explicit, and this one is dangerous

A GNSS solution is nominally 1 Hz and is not. You lose fix in a tunnel, under a
bridge, in an urban canyon. Epochs go missing.

`axis_mode = implicit uniform` (§5) would space the records evenly and claim they
were periodic. After a single dropped fix, **every subsequent sample carries a
wrong timestamp**, and the file looks perfectly well-formed while doing it. There
is no CRC failure, no truncation, no signal that anything happened — just a
recording where the vehicle's position and its speed stop agreeing.

Same rule as §6.7 events, sharper consequence. Events are sporadic and obviously
so; GNSS is *nominally periodic*, which is exactly what makes the implicit axis
tempting and wrong.

## Raw observables: the variable-count case

`gnss.raw` is where MDF 4.3's dynamic-data machinery would be used, and where
Logb's refusal of it gets its real test.

Raw observables are **N satellites per epoch, N varying** — typically 8 to 30
across GPS, GLONASS, Galileo, BeiDou, QZSS and SBAS, changing constantly as
satellites rise and set. This is precisely the CLBLOCK case
([SPEC.md §10](SPEC.md)) that rule 6 — fixed cost per record — forbids.

The modelling answer is one record per **satellite-observation**:

```
gnss.raw:  sv_id, constellation, pseudorange, carrier_phase, doppler, cn0
```

An epoch is the set of records sharing an axis value. If you want grouping that
does not depend on timestamp equality — and you should, because a receiver may
tag observables with slightly different times — add an explicit `epoch` field, a
`u32` that increments per solution. That is the same device §6.5 uses for runs,
one level down.

What this buys, and what a variable-length blob per epoch would have cost:

- **Fixed cost per record**, so decoding one satellite's C/N0 across a whole
  recording does not require walking every other satellite's data.
- **Seekability.** §6.4 is explicit that a variable-length field costs the batch
  its seekability, because the tail is parsed sequentially. Observables are the
  bulkiest thing a GNSS logger writes; making them the one part of the file you
  cannot seek into would be a poor trade.
- **Transpose** (§8) works. Column-ish locality across many records of identical
  layout is exactly the case `filter=transpose` was built for, and C/N0 values
  across satellites are slowly varying — the ideal input.

The cost is repeating the axis value per satellite rather than per epoch. At an
explicit `u32` that is four bytes times ~20 satellites times 1 Hz, or 80 bytes a
second before compression, and transpose makes short work of a column of
near-identical timestamps.

## Where the position actually is

Two ambiguities in GNSS data cause more downstream confusion than any precision
question, and both are metadata rather than fields.

**Which height?** NMEA `GGA` reports height above mean sea level *and* the geoid
separation, in adjacent fields, because the two differ by up to about 100 m
depending where you are. `UBX-NAV-PVT` reports ellipsoidal height and MSL height
as separate members. A file that stores "altitude" without saying which has
stored a number that is wrong by up to 100 m for anyone who assumes the other
one. Store `h_ell`, or `h_msl`, or both — named, never "alt" — and declare
`gnss.height_ref`.

**Which datum?** WGS84, ETRS89 and ITRF are not the same frame, and the
difference is not static. ETRS89 is fixed to the Eurasian plate; ITRF is not. They
diverge at roughly the rate the plate moves — about **2.5 cm per year**, which
since 1989 has accumulated to the better part of a metre. Anyone doing RTK work at
centimetre accuracy is doing it inside a datum, and a file that does not name its
datum has thrown away the frame that made the centimetres meaningful.

Both go in stream META (§6.1), because they are constant for the stream and
belong with the schema that is restated in every segment:

```
gnss.frame          = WGS84
gnss.height_ref     = ellipsoid
gnss.antenna_offset_m = 0,0,1.42
```

The antenna offset is there for the same reason: the receiver reports the
antenna's position, and every consumer wants the vehicle's.

## Fix quality is an enum

`fix_type` is a small integer with names, which is `value_to_text` (§7):

```
0 none, 1 gnss, 2 dgnss, 4 rtk_fixed, 5 rtk_float, 6 dead_reckoning
```

The numbering follows NMEA `GGA`'s quality indicator, which is the one encoding
every receiver on the market can produce. Note that 4 is *fixed* and 5 is
*float*, which is the reverse of the intuitive ordering and a classic
off-by-one — RTK fixed is the better solution and carries the lower number.

A reader with no GNSS knowledge renders `rtk_float` from the file alone, which is
the whole point of carrying conversions rather than a sidecar.

## Raw NMEA, UBX, and RTCM

Rule 5 says the bits the sensor produced are preserved. For a logger with no
parser — or one that wants an audit trail — the whole receiver message goes in a
fixed `bytes` field, with the encoding declared in **field metadata** (§6.2):

```
payload   bytes   meta: payload.encoding = ubx
```

Signals may overlay those same bytes as a separate stream, exactly as
[CAN.md](CAN.md) describes for a CAN payload: `gnss.pvt` holds the decoded
solution, a raw stream holds the wire bytes, fields may overlap, and a
read-modify-write round trip is byte-identical.

## What this does not do well

One honest gap, and it is the same one MDF answers with a block type Logb does
not have.

**Position covariance is a matrix.** A 3×3 covariance, or six elements exploiting
symmetry, becomes six enumerated fields — `cov_nn`, `cov_ne`, `cov_nd`, `cov_ee`,
`cov_ed`, `cov_dd`. That works, costs nothing at runtime, and stays seekable.
It is also plainly verbose, and it is where the absence of an array type stops
being theoretical. Most loggers store `h_acc` and `v_acc` scalars instead and
never meet this; anyone doing tightly-coupled INS work will.

**Interoperability is not free.** ASAM's standard means a Vector tool opens any
conforming file and finds latitude. A Logb convention means that only once it is
written down and adopted, which is what this document and §6.8 are for. The
associated-standard model has exactly this cost, and pretending otherwise would
be dishonest: the format guarantees the file is decodable, and the convention is
what makes it *comparable*.

## Summary

| | MDF 4.3 | Logb |
|---|---|---|
| How GNSS is standardised | associated standard (v1.0.0) | convention: §6.8 + this document |
| Format changes required | none | none |
| Coordinates | channel, any type | `i32` at 1e-7°, conversion declared (rule 5) |
| Receiver time vs logger time | one master channel | axis is logger time; receiver time is a field |
| Dating a file logged before fix | header must be written up front | `time.anchor`, emitted after the fact (§5.2) |
| Dropped epochs | master channel per group | explicit axis, mandatory (§5) |
| Raw observables, N per epoch | CLBLOCK dynamic data | one record per satellite-observation |
| Covariance matrix | array block | six enumerated fields — the weak spot |
| Datum, height reference | XML metadata | stream META key/value (§6.1) |
| Raw receiver messages | attachment or blob channel | `bytes` field, `payload.encoding` in field META |

## See also

- [SPEC.md §6.8](SPEC.md) — the normative stream names, fields, units, and META keys
- [SPEC.md §5.2](SPEC.md) — time base, anchors, and the late clock
- [SPEC.md §6.7](SPEC.md) — event streams, the other written-down convention
- [CAN.md](CAN.md) — the same argument for bus data: raw preserved, signals overlaid
