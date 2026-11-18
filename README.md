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
func DecodeBlock(dst, src []byte) (int, error)                   // into your own buffer
func NewReader(r io.Reader) (io.Reader, error)                   // an lzop stream
```

`DecodeBlock` allocates nothing and never reads or writes outside `src` and `dst`:
for a filesystem block whose exact size is known (SquashFS, btrfs). A block that
would write past `len(dst)` is `ErrShortDst`; a malformed one, or bytes left after
the end-of-stream marker, `ErrCorrupt`. `Decompress` is built on it. Common
instructions are decoded with 8- and 16-byte moves while both buffers are far
from their ends, exact paths otherwise.

Checked against blocks written by liblzo2 2.10 (`lzo1x_1_compress`, the encoder
the Linux kernel's squashfs and btrfs use; `testdata/liblzo2`, the raw corpora
regenerated in the test), fuzzed against the instrumented reference decoder in
this package (25M and 105M executions, no difference), and run against
`PROT_NONE` guard pages on both buffers. liblzo2 is used only as a black box: to
write those blocks and as the speed bar of a separate benchmark program, never
read or copied.

Single core, 128 KiB blocks, output MB/s (liblzo2 2.10 -O2, anchore/go-lzo
v0.1.1): Apple M4 Max text 2000 (liblzo2 1110, anchore 640), runs 16100 (4200,
1790), mixed 58000 (19600, 2210); 4 KiB blocks of text 10900 (5300, 1610). AMD
Ryzen 9 7950X: text 2000 (1150, 690), runs 12000 (4400, 1570), mixed 58000
(22500, 1670), 4 KiB 9400 (6000, 1410).

The btrfs read workload, 1024 pages of 4 KiB of a text-like corpus (200-word
vocabulary, ratio 1.88) compressed by liblzo2's `lzo1x_1_compress`, decoded by
`DecodeBlock` and by `lzo1x_decompress_safe` (through cgo, one call per
whole batch; `go test -tags liblzo2 -bench BtrfsPages`): Apple M4 Max, one core,
12 interleaved rounds, 1-minute load 23 (above the fleet's 8, so indicative),
liblzo2 1.06 GiB/s, `DecodeBlock` 1.85 GiB/s (1.7x liblzo2; 1.48 GiB/s before
the branch-reduced fast loop, 1.4x). On amd64 and arm64 the common
instructions run in an unchecked loop (`fast_unsafe.go`) that stays 32 bytes
of input and 64 of output away from the buffers' ends, so the proof that no
access leaves the buffers is the loop condition; other architectures, and the
`purego` build tag, use the same loop with ordinary indexing
(`fast_generic.go`). Opcode dispatch is the cost: the short match forms take
their length from `1 + t>>5` and the trailing literals are moved without
branching on their count.

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
block), `ErrBitstreamVersion` (LZO-RLE) and `ErrTooLarge` (a requested output
above `DefaultMaxOutput`).

## Untrusted input

Lengths come from a container header somebody else wrote, and LZO1X expands up
to 255:1 (each extra length byte adds 255 — the kernel document says "the
compression ratio cannot exceed around 255:1"). Before v0.3.0:

- `Decompress(src, n)` allocated `n` as given: three bytes asking for 1 GiB
  allocated 1 GiB (measured with `TotalAlloc`).
- `NewReader` sized its buffers from the block header before reading the
  payload: a block claiming 64 MiB followed by 10 bytes allocated 64 MiB, and a
  100-byte block claiming 64 MiB allocated 64 MiB for output that could never be
  produced.

Now:

| bound | where | result |
| --- | --- | --- |
| `n > DefaultMaxOutput` (1 GiB) | `Decompress` | `ErrTooLarge`, before allocating |
| `n > MaxExpansion (255) × len(src)` | `Decompress` | `ErrCorrupt`, before allocating — such a block cannot exist |
| uncompressed length `> 255 ×` compressed length | `NewReader`, per block | `ErrHeader`, before any buffer is sized |
| compressed length claim | `NewReader`, per block | the payload buffer grows with the bytes that actually arrive (64 KiB, then doubling) |
| block size `> 64 MiB` | `NewReader` | `ErrHeader` (lzop's own maximum, unchanged) |

`FuzzExpansion` decodes into an output far above the bound and asserts that no
accepted block exceeds it, so the 255 is checked rather than trusted, and
`FuzzDecompress` no longer skips large lengths.

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
