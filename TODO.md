# TODO

Changes under consideration. Nothing here is decided or scheduled.

## DBC: flatten nested multiplexing when the selectors are adjacent

**Status:** proposed, not started.

### Problem

A message multiplexed on more than one level (`mNM`, or `SG_MUL_VAL_` naming a
multiplexor that is itself multiplexed) is refused as a whole, because §6.2
guards do not chain. The common real case is diagnostics: a full OBD2 or UDS
database selects first on the service byte (`0x41`, `0x42`, `0x62`, …) and then
on the PID or DID. Logb's own `testdata/obd2.dbc` sidesteps this by fixing the
service to mode 01.

### Idea

If outer selector `O` and inner selector `I` (live when `O == v`) are contiguous
in one bit numbering, a single synthetic field spanning both reads `(v, w)` as
one integer. A signal live when `I == w` is then one ordinary guard on that
field. No format change: one guard, on an unguarded `uint` field, overlap
already legal, payload untouched.

Example, OBD2 response (byte 0 length, byte 1 service, byte 2 PID): a
big-endian field at payload bit 8, 16 bits wide, reads `0x410C` for service
`0x41`, PID `0x0C`; `EngineSpeed` is guarded on it `== 0x410C`.

The same works for any depth if the whole selector chain is contiguous.

### Conditions (otherwise keep today's refusal)

1. The selector chain covers one unbroken run of bits in one numbering — no gap,
   since a gap would make the guard compare unrelated bits.
2. One byte order across the chain. A byte-aligned 8-bit selector reads the same
   in either order and may join either.
3. All selectors unsigned, combined width ≤ 64 bits.
4. Single values only. `SG_MUL_VAL_` ranges (`1-5`) still need several guard
   values and stay refused.

Guard value: compose the selector values by position in the combined field —
in little-endian the lowest-offset selector is the low bits, in big-endian the
high bits. Inner multiplexors occupying the same bits under different outer
values (`PID_Service01`, `PID_Service02`) share one combined field.

### Prerequisite: parser must record the parent multiplexor

 `Signal` has `Muxed`/`MuxValue` but not *which* multiplexor, and
 `parseExtendedMux` ignores the multiplexor name in `SG_MUL_VAL_` (it uses
 `m[1]`, `m[2]`, `m[4]`, never `m[3]`). Harmless today because nested messages
are refused wholesale, but flattening needs it:

- add `MuxParent` to `Signal`, filled from `SG_MUL_VAL_`, or the sole `M` when
  there is only one;
- refuse a nested message lacking `SG_MUL_VAL_`, since the DBC then does not say
  which multiplexor an `mN` signal hangs off.

### Work

| Where | Change |
|---|---|
| `dbc/dbc.go` | `MuxParent`; keep the multiplexor name from `SG_MUL_VAL_` (~20 lines) |
| `dbc/schema.go` | walk each signal's selector chain, check the conditions, emit or reuse the combined field, compute the guard value; fall back to the refusal on failure (~80 lines) |
| `dbc/encode.go` | compute the chain too, or keep refusing nested messages for now |
| `mdf/bus.go` | none — it calls `dbc.Schema`, and the record layout is unchanged |
| tests | two-service OBD2 fixture: selected signal decodes, same PID under the other service reports absent; gap case and mixed-byte-order case still refused |
| `CAN.md`, `STATUS.md` | move contiguous nested selectors from "refused" to "supported (flattened)" |

### Open points

- The combined field's value means little alone (`0x410C`). Mark it in field
  metadata (e.g. `dbc.synthetic = selector`) so the viewer can hide or label it.
- Check a real multi-service OBD2 DBC (e.g. CSS Electronics') to confirm it uses
  `mNM` with adjacent service and PID bytes before relying on it as the example.
- Non-contiguous nesting still needs chained guards (SPEC.md §13.3, item 1).
