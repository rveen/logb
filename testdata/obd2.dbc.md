# `obd2.dbc`

A small OBD2 database, written for this repository, covering the PIDs that
actually occur in `testdata/mdf/ex2-obd.mf4`. It exists so the DBC decoder can
be demonstrated and tested end to end on a recording that is already here:

```sh
go run ./cmd/mdf2logb -dbc testdata/obd2.dbc testdata/mdf/ex2-obd.mf4
```

## What it says

OBD2 mode 01 is request/response. The tester asks on `0x7DF` and the ECU answers
on `0x7E8`, and the payload is `[length][service][PID][data…]` — so what the data
bytes *mean* depends on the PID byte inside the frame, not on the CAN identifier.
That is multiplexing, and it is why this file is the natural fixture for
[SPEC.md §6.2](../SPEC.md)'s guarded fields: `PID` is the multiplexor, and every
signal below it is present only in frames whose PID selects it.

`EngineSpeed` is deliberately Motorola (`31|16@0+`), because PID 0x0C is
big-endian across bytes 3 and 4: `((256*A)+B)/4`. It is the case
[CAN.md](../CAN.md) is about, and it goes through the importer as an offset
transform — DBC start bit 31 becomes Logb bit offset 24 — with no data movement.

The scaling formulas are the public OBD2 standard ones (SAE J1979 / ISO 15031-5),
which are widely published; the file itself was written here rather than copied.

## What it deliberately does not do

A production OBD2 DBC — the one CSS Electronics publishes, for instance — uses
**extended multiplexing** (`SG_MUL_VAL_`) to multiplex on the service byte and
then again on the PID within it. Logb's guards do not chain, by design: §6.2
allows one level, on the grounds that more buys conditional logic a reader has to
evaluate as a graph. This file multiplexes on the PID alone, which is exactly one
level and is sufficient here because the recording is all mode 01.

The importer does not paper over the difference. A DBC that needs two levels is
reported through `Options.Warn` and the affected signals are left out rather than
decoded in frames that do not carry them — a wrong number being worse than a
missing one.
