package lzo

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

// The corpus is embedded rather than read from testdata, because the emulated
// CI lanes run a cross-compiled `go test -c` binary with no testdata directory
// beside it.
//
//go:embed testdata/crafted.json testdata/*.lzo testdata/*.bin testdata/text.txt
var corpus embed.FS

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := corpus.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("embedded fixture %s: %v", name, err)
	}
	return b
}

// lzopCorpus pairs each .lzo file written by the real lzop 1.04 with the exact
// bytes it was made from. testdata/gen.sh asserts, at generation time, that
// `lzop -dc` reads each one back to its source before the fixture is kept: a
// fixture whose premise has not been checked is not evidence.
var lzopCorpus = []struct {
	lzo, src string
	blocks   int // blocks lzop wrote, 0 for an empty file
	stored   int // how many of them are stored rather than compressed
	note     string
}{
	{"text.lzo", "text.txt", 1, 0, "method 1 (LZO1X_1), lzop's default"},
	{"text-1.lzo", "text.txt", 1, 0, "lzop -1: method 2 (LZO1X_1_15)"},
	{"text-9.lzo", "text.txt", 1, 0, "lzop -9: method 3 (LZO1X_999)"},
	{"text-best.lzo", "text.txt", 1, 0, "lzop --best: method 3 as well"},
	{"text-crc32.lzo", "text.txt", 1, 0, "CRC-32 header and data checksums"},
	{"text-nock.lzo", "text.txt", 1, 0, "no data checksum at all"},
	{"zeros.lzo", "zeros.bin", 2, 0, "300 KiB of zeros: maximal match lengths, and two blocks"},
	{"rand.lzo", "rand.bin", 1, 1, "32 KiB of incompressible bytes: one stored block"},
	{"frag.lzo", "frag.bin", 1, 0, "planted far repeats: reaches the M4 instruction"},
	{"mixedstore.lzo", "mixedstore.bin", 2, 1, "a stored block inside a multi-block stream"},
	{"empty.lzo", "empty.bin", 0, 0, "an empty file: no blocks, just the terminator"},
	{"one.lzo", "one.bin", 1, 1, "a single byte, which LZO1X cannot shrink"},
}

// TestLzopCorpus is the whole point of the package: bytes written by lzop must
// come back out of this decoder unchanged. A matching length would prove
// nothing, so the comparison is on the bytes.
func TestLzopCorpus(t *testing.T) {
	for _, c := range lzopCorpus {
		t.Run(c.lzo, func(t *testing.T) {
			want := fixture(t, c.src)
			z := mustReader(t, fixture(t, c.lzo))
			got, err := io.ReadAll(z)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("decoded bytes differ from %s (got %d bytes, want %d); %s",
					c.src, len(got), len(want), firstDifference(got, want))
			}
			r := z.(*reader)
			if r.blocks != c.blocks {
				t.Errorf("read %d blocks, want %d", r.blocks, c.blocks)
			}
			if r.stored != c.stored {
				t.Errorf("saw %d stored blocks, want %d", r.stored, c.stored)
			}
			t.Logf("%d bytes from %d, %d block(s), %d stored — %s",
				len(got), len(fixture(t, c.lzo)), r.blocks, r.stored, c.note)
		})
	}
}

// TestStoredBlocksExistInTheCorpus makes the stored-block premise explicit: if
// no fixture ever produced one, every test here would still pass with the
// stored-block shortcut deleted, and the package would fail on the first
// incompressible file it met.
func TestStoredBlocksExistInTheCorpus(t *testing.T) {
	total := 0
	for _, c := range lzopCorpus {
		z := mustReader(t, fixture(t, c.lzo))
		if _, err := io.ReadAll(z); err != nil {
			t.Fatalf("%s: %v", c.lzo, err)
		}
		total += z.(*reader).stored
	}
	if total == 0 {
		t.Fatal("no stored block anywhere in the corpus: the stored-block " +
			"shortcut is untested, and a random-bytes file would fail")
	}
	t.Logf("%d stored blocks across the lzop corpus", total)
}

var opNames = [numOpClasses]string{
	opFirstLiterals:    "first-byte literal run (opening byte 18..255)",
	opLiteralRun:       "literal run, length in the opcode (0..15, state 0)",
	opLiteralRunLong:   "literal run, length continuation (0..15, state 0)",
	opShort1k:          "2 bytes from <=1 KiB back (0..15, state 1..3)",
	opShort3k:          "3 bytes from 2..3 KiB back (0..15, state 4)",
	opM4:               "M4, length in the opcode (16..31)",
	opM4Long:           "M4, length continuation (16..31)",
	opM3:               "M3, length in the opcode (32..63)",
	opM3Long:           "M3, length continuation (32..63)",
	opM2:               "3..4 bytes from <=2 KiB back (64..127)",
	opM2Big:            "5..8 bytes from <=2 KiB back (128..255)",
	opTrailingLiterals: "the 1..3 trailing literals of a match",
	opCopyOverlap:      "an overlapping match (distance < length)",
	opEndOfStream:      "the distance-16384 end-of-stream instruction",
}

// TestCorpusReachesEveryInstructionClass asserts what the corpus is for. A
// corpus that merely round-trips proves the paths it happens to touch; this
// counts them and fails if any class of the grammar was never decoded.
func TestCorpusReachesEveryInstructionClass(t *testing.T) {
	var total [numOpClasses]uint32
	add := func(h [numOpClasses]uint32) {
		for i := range h {
			total[i] += h[i]
		}
	}

	for _, c := range lzopCorpus {
		z := mustReader(t, fixture(t, c.lzo))
		if _, err := io.ReadAll(z); err != nil {
			t.Fatalf("%s: %v", c.lzo, err)
		}
		add(z.(*reader).dec.hits)
	}
	fromLzop := total

	for _, c := range craftedCases(t) {
		if c.Stored {
			continue
		}
		var d decoder
		d.reset(hexBytes(t, c.Block), make([]byte, len(hexBytes(t, c.Plain))))
		if err := d.run(); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		add(d.hits)
	}

	for i := range total {
		t.Logf("%7d (%6d from lzop's own output)  %s", total[i], fromLzop[i], opNames[i])
		if total[i] == 0 {
			t.Errorf("instruction class never reached: %s", opNames[i])
		}
	}
	// The state-4 short match is the one class lzop's compressors do not emit,
	// which is why testdata/gen_crafted.py exists at all. If lzop starts
	// emitting it this assertion is what will say so.
	if fromLzop[opShort3k] != 0 {
		t.Logf("note: lzop's own output now reaches the state-4 short match "+
			"(%d times); the crafted case for it is no longer the only witness",
			fromLzop[opShort3k])
	}
}

// --------------------------------------------------------------------------
// Crafted cases, each confirmed by lzop -dc at generation time.
// --------------------------------------------------------------------------

type craftedCase struct {
	Name   string `json:"name"`
	Note   string `json:"note"`
	Block  string `json:"block"`
	Plain  string `json:"plain"`
	Stream string `json:"stream"`
	Stored bool   `json:"stored"`
}

func craftedCases(t testing.TB) []craftedCase {
	t.Helper()
	var cases []craftedCase
	if err := json.Unmarshal(fixture(t, "crafted.json"), &cases); err != nil {
		t.Fatalf("crafted.json: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("crafted.json is empty")
	}
	return cases
}

// TestCraftedBlocks decodes the hand-built LZO1X blocks that lzop's compressor
// never emits. Their expected output is not this package's opinion: lzop
// decompressed every one of them to exactly these bytes before the fixture was
// written, with lzop checking the Adler-32 in the container as it went.
func TestCraftedBlocks(t *testing.T) {
	for _, c := range craftedCases(t) {
		if c.Stored {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			block, want := hexBytes(t, c.Block), hexBytes(t, c.Plain)
			got, err := Decompress(block, len(want))
			if err != nil {
				t.Fatalf("Decompress: %v (%s)", err, c.Note)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: %s", c.Note, firstDifference(got, want))
			}
			t.Logf("%d bytes from %d — %s", len(want), len(block), c.Note)
		})
	}
}

// TestCraftedStreams runs the same cases through the container reader, which
// also covers the header shapes lzop does not write by default: no filename, a
// pre-0x0940 version, and each combination of the four optional block
// checksums.
func TestCraftedStreams(t *testing.T) {
	for _, c := range craftedCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			want := hexBytes(t, c.Plain)
			got, err := io.ReadAll(mustReader(t, hexBytes(t, c.Stream)))
			if err != nil {
				t.Fatalf("ReadAll: %v (%s)", err, c.Note)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s: %s", c.Note, firstDifference(got, want))
			}
		})
	}
}

// TestBitstreamVersionMarker covers the one rule in the published description
// that lzop cannot judge: an opening byte of 17 introduces a bitstream version.
// liblzo2 2.10 predates the LZO-RLE extension that added it, so lzop reads such
// a block as an immediate end-of-stream instead and cannot serve as a witness.
// The behaviour here follows the description: version 0 is decoded, and version
// 1 (LZO-RLE) is refused rather than guessed at.
func TestBitstreamVersionMarker(t *testing.T) {
	const payload = "version zero"
	v0 := append([]byte{17, 0, byte(len(payload) - 3)}, payload...)
	v0 = append(v0, 0x11, 0x00, 0x00)
	got, err := Decompress(v0, len(payload))
	if err != nil {
		t.Fatalf("version 0 marker: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("version 0 marker: got %q, want %q", got, payload)
	}

	v1 := append([]byte{17, 1, byte(len(payload) - 3)}, payload...)
	v1 = append(v1, 0x11, 0x00, 0x00)
	if _, err := Decompress(v1, len(payload)); !isErr(err, ErrBitstreamVersion) {
		t.Fatalf("version 1 marker: got %v, want ErrBitstreamVersion", err)
	}
}

// TestReadInSmallPieces checks that the reader hands its blocks out correctly
// across Read boundaries rather than only under io.ReadAll's large buffer.
func TestReadInSmallPieces(t *testing.T) {
	want := fixture(t, "zeros.bin")
	z := mustReader(t, fixture(t, "zeros.lzo"))
	got := make([]byte, 0, len(want))
	buf := make([]byte, 7)
	for {
		n, err := z.Read(buf)
		got = append(got, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatal(firstDifference(got, want))
	}
	// A zero-length Read must not consume anything or report the end.
	if n, err := z.Read(nil); n != 0 || err != io.EOF {
		t.Fatalf("Read(nil) past the end: %d, %v; want 0, EOF", n, err)
	}
}

// --------------------------------------------------------------------------
// Helpers.
// --------------------------------------------------------------------------

func mustReader(t testing.TB, stream []byte) io.Reader {
	t.Helper()
	z, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return z
}

func hexBytes(t testing.TB, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := range b {
		var v byte
		for j := 0; j < 2; j++ {
			c := s[2*i+j]
			switch {
			case c >= '0' && c <= '9':
				v = v<<4 | (c - '0')
			case c >= 'a' && c <= 'f':
				v = v<<4 | (c - 'a' + 10)
			default:
				t.Fatalf("bad hex digit %q at %d", c, 2*i+j)
			}
		}
		b[i] = v
	}
	return b
}

// firstDifference names where two byte slices part company, because "lengths
// differ" is not a diagnosis.
func firstDifference(got, want []byte) string {
	n := min(len(got), len(want))
	for i := 0; i < n; i++ {
		if got[i] != want[i] {
			lo := max(0, i-8)
			return fmt.Sprintf("first difference at byte %d of %d/%d: got %#02x, want %#02x (got %q, want %q)",
				i, len(got), len(want), got[i], want[i], got[lo:min(len(got), i+8)], want[lo:min(len(want), i+8)])
		}
	}
	return fmt.Sprintf("common prefix of %d bytes agrees; got %d bytes, want %d", n, len(got), len(want))
}
