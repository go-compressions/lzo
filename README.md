<p align="center"><img src="https://raw.githubusercontent.com/go-compressions/brand/main/social/go-compressions-lzo.png" alt="go-compressions/lzo" width="720"></p>

# lzo

[![ci](https://github.com/go-compressions/lzo/actions/workflows/ci.yml/badge.svg)](https://github.com/go-compressions/lzo/actions/workflows/ci.yml)
![coverage](https://img.shields.io/badge/coverage-100%25-brightgreen)
[![Go Reference](https://pkg.go.dev/badge/github.com/go-compressions/lzo.svg)](https://pkg.go.dev/github.com/go-compressions/lzo)
[![License](https://img.shields.io/badge/License-BSD--3--Clause-blue.svg)](LICENSE)

A pure-Go (`CGO_ENABLED=0`, no third-party dependency) **decoder** for the
**LZO1X** compressed bytestream and the **lzop** (`.lzo`) container.

```go
func Decompress(src []byte, uncompressedLen int) ([]byte, error) // one LZO1X block
func NewReader(r io.Reader) (io.Reader, error)                   // an lzop stream
```

## Licence, and why this was written from scratch

liblzo2 — the reference LZO implementation — is **GPL-2.0**. This package is
**BSD-3-Clause**, so no liblzo2 code, and no port or translation of liblzo2
code, was read, consulted or adapted.

It was written instead from a published prose description of the bytestream:

- **["LZO stream format as understood by Linux's LZO decompressor"](https://docs.kernel.org/staging/lzo.html)**
  by Willy Tarreau and Dave Rodgman — `Documentation/staging/lzo.rst` in the
  Linux tree. It gives the instruction table, the variable-length encoding, the
  distance formulas, the meaning of the *state* variable and the end-of-stream
  marker. Every rule in `lzo1x.go` comes from that table.
- the **`lzop(1)` command-line tool, used as a black box**: `lzop -c` writes the
  fixtures, `lzop -dc` reads them back, and every hand-built corner case in
  `testdata/crafted.json` had to be decompressed by lzop to exactly the expected
  bytes before it was allowed to become a fixture.

Reading a format description and checking the result against a black-box
reference is the whole method. The container layout was pinned down the same
way — field by field, then confirmed against real `.lzo` files, which is how
two mistakes in the working notes were caught (see *Two things worth
knowing*, below).

## Install

```sh
go get github.com/go-compressions/lzo
```

## Usage

```go
f, _ := os.Open("firmware.lzo")
r, err := lzo.NewReader(f)
if err != nil {
	log.Fatal(err) // not lzop, a filtered stream, a bad header checksum...
}
data, err := io.ReadAll(r)
```

```go
// A bare LZO1X block, whose output length the caller already knows.
plain, err := lzo.Decompress(block, 4096)
```

Errors are sentinel-wrapped, so a caller can tell the cases apart with
`errors.Is`: `ErrMagic` (not an lzop stream), `ErrHeader` (malformed or
truncated), `ErrChecksum`, `ErrUnsupported` (a filter, an extra field, a
non-LZO1X method, a stream needing a newer lzop), `ErrCorrupt` (a bad LZO1X
block) and `ErrBitstreamVersion` (LZO-RLE).

## Decoder only

There is no compressor, on purpose. Writing `.lzo` serves nobody: every
consumer of the format already reads gzip, zstd or xz, all of which compress
better, and a second encoder would double the surface that has to be proved
byte-exact while adding no capability a reader does not already give. Decoding
is what lets Go programs open the `.lzo` files that already exist — Hadoop
splits, firmware images, initramfs payloads, old archives.

## How it works

**LZO1X.** The bytestream is a sequence of one-byte opcodes, each carrying a
match length, part of a distance and the number of literals that follow it. Five
instruction shapes cover the space:

| opcode | shape | meaning |
| --- | --- | --- |
| `0..15` | `0000LLLL` / `0000DDSS` | a literal run of 4 or more, **or** a 2-byte match within 1 KiB, **or** a 3-byte match 2..3 KiB back — which one depends on how many literals the previous instruction copied |
| `16..31` | `0001HLLL` + LE16 | a match 16..48 KiB back; distance 16384 means **end of stream** |
| `32..63` | `001LLLLL` + LE16 | a match within 16 KiB |
| `64..127` | `01LDDDSS` + byte | 3..4 bytes within 2 KiB |
| `128..255` | `1LLDDDSS` + byte | 5..8 bytes within 2 KiB |

A length field of zero switches to a continuation encoding: any number of `0x00`
bytes each adding 255, then one non-zero byte adding its value. The opening byte
of a block follows its own rule, because there is no output behind it to copy
from: above 17 it means a literal run of *byte*−17, and it is the only way a run
of 1 to 3 literals can be spelled.

**The container.** Nine magic bytes, then a big-endian header whose shape
depends on its own version field, then an Adler-32 or CRC-32 over that header,
then blocks: uncompressed length, compressed length, up to four optional
checksums, payload. A zero uncompressed length ends the stream.

**Methods 1, 2 and 3 are one format.** LZO1X_1, LZO1X_1_15 and LZO1X_999 differ
only in how hard the *compressor* searches; all three emit the same instruction
grammar and all three decode with the same decoder. It looks like three formats
and it is one.

## Two things worth knowing

**A block whose compressed length equals its uncompressed length is stored, not
compressed.** lzop falls back to storing whenever LZO1X would make a block
bigger, which incompressible input always does. A decoder that hands every block
to the LZO1X decompressor fails on the first random-bytes file it meets — and a
corpus without such a file will never say so. `rand.lzo` and `mixedstore.lzo`
exist for that, and the tests count the stored blocks they produce and fail if
the count is zero.

**lzop refuses a block whose compressed length is not *strictly* less than its
uncompressed length.** That is the same rule seen from the writer's side, and it
is why every hand-crafted block in `testdata/crafted.json` ends with cheap
padding: without it, lzop declines to read the fixture at all and cannot act as
a judge.

## Not supported

- **LZO-RLE** (bitstream version 1, the zram zero-run extension). No witness for
  it is available here, and a decoder that silently mis-decodes is worse than one
  that says no. `ErrBitstreamVersion`.
- **lzop's delta filters** (`lzop --filter=N`) and its **header extra field**.
  `ErrUnsupported`, with `testdata/text-filter1.lzo` as the real-world fixture.

## Tests

- **Every fixture's premise is asserted before it is judged.** `testdata/gen.sh`
  makes each `.lzo` with real lzop and requires `lzop -dc` to reproduce the
  source byte-for-byte before the fixture is kept. A generator that reports
  success without doing the work is worse than no generator.
- **The corpus is proved to reach the grammar, not assumed to.** The decoder
  counts each instruction class it decodes, and a test fails if any class was
  never reached. That is how the one class lzop's compressors never emit — the
  state-4 short match, 3 bytes from 2..3 KiB back — was found, and why
  `testdata/gen_crafted.py` exists.
- **The crafted cases are judged by lzop, not by this package.** Each is built by
  hand from the published description, wrapped in a container carrying the
  Adler-32 of the expected output, and handed to `lzop -dc`. A case lzop refuses,
  or decodes differently, fails the generator and never becomes a fixture.
- **Ablation.** Each rule of the encoding, the stored-block shortcut, the
  end-of-stream zero, the checksum choice and the big-endian reads were broken
  one at a time to confirm the suite notices. That sweep found a test that only
  looked like it tested the compressed-length guard.
- **100 % statement coverage**, gated in CI on four native and four emulated
  architectures (riscv64, loong64, ppc64le, s390x). Fixtures are embedded with
  `//go:embed`, because the emulated lanes run a cross-compiled test binary with
  no `testdata` beside it.

## Regenerating the corpus

```sh
sh testdata/gen.sh      # needs lzop(1) and python3
```
