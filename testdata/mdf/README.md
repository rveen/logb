# MDF4 test fixtures

Input files for `mdf/mdf_test.go` and `mdf/convert_test.go`. They are here so the
test suite is self-contained: an importer whose tests need a file that is not in
the repository is an importer nobody else can check.

| File | What it exercises |
|---|---|
| `ex3.mf4` | The simple case: sorted, one DT block, f64 master, three int64 channels |
| `ex5.mf4` | A linear conversion on the master (so the stored bytes are not seconds), a `tabx` value-to-text conversion, and one embedded AT attachment |
| `ex1.mf4` | A DZ (deflate) data block and an 8-bit channel at a byte offset |
| `ex6-compressed.mf4` | An HL → DL → DZ chain, 100 000 records, and a **uint64** master that only becomes seconds after its linear conversion |
| `ex2-obd.mf4` | An unfinalized, unsorted CAN recording: record ids, composed channels at bit offsets (`CAN_DataFrame.ID` is 29 bits starting at bit 2), and a VLSD payload channel |

## Provenance

The first four come from the test suite of
[LincolnG4/GoMDF](https://github.com/LincolnG4/GoMDF), which is MIT licensed
(Copyright (c) 2023 Gabriel Lincoln Santos). The OBD2 recording is from the same
suite, where it appears as `test/obd2/a.mf4`; it originates with a CANedge
logger's public sample data.

`obd2-trunc.mf4` is the **first 64 KiB** of that 1 MB file. Truncating it is
sound rather than lucky: the recording is *unfinalized* (`UnFinMF `,
std_flags=37), meaning its cycle counts were never written and the final DT
block's length field was never patched, so a reader must already derive the
record count by walking to the end of the data. Cutting the file short just moves
where that end is — which is also, usefully, a test that the walk stops cleanly on
a partial trailing record.
