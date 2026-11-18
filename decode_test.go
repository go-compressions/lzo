package lzo

import (
	"bytes"
	"embed"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"
)

// The fixtures are embedded: the emulated CI jobs run the test binary far from
// the source tree.
//
//go:embed testdata/liblzo2/*.lzo32768
var liblzo2FS embed.FS

// testdata/liblzo2/{text,runs,mixed}.lzo32768 hold the first three 32 KiB blocks
// of three deterministic corpora, compressed by liblzo2 2.10 (lzo1x_1_compress,
// the encoder the Linux kernel's squashfs and btrfs use), as records [u32
// rawLen][u32 compLen][bytes]. The corpora are regenerated here (one PCG stream
// each, seeds (42, 43+i)); the decoders must return exactly those bytes, so the
// reference encoder is the oracle for the format, not this package's own blocks.
const corpusTotal = 8 << 20

func genText(r *rand.Rand) []byte {
	vocab := make([][]byte, 6000)
	for i := range vocab {
		w := make([]byte, 3+r.IntN(8))
		for j := range w {
			w[j] = byte('a' + r.IntN(26))
		}
		vocab[i] = w
	}
	var b []byte
	for len(b) < corpusTotal {
		k := int(float64(len(vocab)) * r.Float64() * r.Float64() * r.Float64())
		b = append(b, vocab[k]...)
		if r.IntN(12) == 0 {
			b = append(b, '\n')
		} else {
			b = append(b, ' ')
		}
	}
	return b[:corpusTotal]
}

func genRuns(r *rand.Rand) []byte {
	b := make([]byte, corpusTotal)
	for i := range b {
		b[i] = byte('A' + (i/4096)%26)
	}
	for i := 0; i < corpusTotal/512; i++ {
		b[r.IntN(corpusTotal)] = byte(r.Uint32())
	}
	return b
}

func genMixed(r *rand.Rand) []byte {
	b := make([]byte, 0, corpusTotal)
	for len(b) < corpusTotal {
		if len(b) > 64 && r.IntN(3) == 0 {
			n := 4 + r.IntN(28)
			off := 1 + r.IntN(min(len(b), 60000))
			for k := 0; k < n; k++ {
				b = append(b, b[len(b)-off])
			}
		} else {
			n := 1 + r.IntN(24)
			for k := 0; k < n; k++ {
				b = append(b, byte(r.Uint32()))
			}
		}
	}
	return b[:corpusTotal]
}

type liblzo2Block struct{ comp, want []byte }

func liblzo2Blocks(t testing.TB) map[string][]liblzo2Block {
	out := map[string][]liblzo2Block{}
	for i, c := range []struct {
		name string
		gen  func(*rand.Rand) []byte
	}{{"text", genText}, {"runs", genRuns}, {"mixed", genMixed}} {
		raw := c.gen(rand.New(rand.NewPCG(42, uint64(43+i))))
		f, err := liblzo2FS.ReadFile("testdata/liblzo2/" + c.name + ".lzo32768")
		if err != nil {
			t.Fatal(err)
		}
		for n := 0; len(f) > 0; n++ {
			rawLen := int(binary.LittleEndian.Uint32(f))
			compLen := int(binary.LittleEndian.Uint32(f[4:]))
			out[c.name] = append(out[c.name], liblzo2Block{f[8 : 8+compLen], raw[n*rawLen : (n+1)*rawLen]})
			f = f[8+compLen:]
		}
	}
	return out
}

func TestDecodeBlockAgainstLiblzo2(t *testing.T) {
	for name, blocks := range liblzo2Blocks(t) {
		if len(blocks) != 3 {
			t.Fatalf("%s: %d blocks, want 3", name, len(blocks))
		}
		for i, b := range blocks {
			dst := make([]byte, len(b.want))
			n, err := DecodeBlock(dst, b.comp)
			if err != nil || !bytes.Equal(dst[:n], b.want) {
				t.Fatalf("%s block %d: n=%d err=%v", name, i, n, err)
			}
			got, err := Decompress(b.comp, len(b.want))
			if err != nil || !bytes.Equal(got, b.want) {
				t.Fatalf("%s block %d: Decompress err=%v", name, i, err)
			}
		}
	}
}

// The unchecked fast path stops a fixed margin short of the buffers' ends and
// the exact loop finishes: whatever slack dst has (0..80 bytes, either side of
// the 64-byte margin), the output is the same and nothing is written past dst
// or, beyond the returned count, further than the documented 15 bytes.
func TestDecodeBlockEverySlack(t *testing.T) {
	for name, blocks := range liblzo2Blocks(t) {
		for bi, b := range blocks {
			for slack := 0; slack <= 80; slack++ {
				buf := make([]byte, len(b.want)+slack+16)
				for i := range buf {
					buf[i] = 0xA5
				}
				dst := buf[: len(b.want)+slack : len(b.want)+slack]
				n, err := DecodeBlock(dst, b.comp)
				if err != nil || !bytes.Equal(dst[:n], b.want) {
					t.Fatalf("%s block %d slack %d: n=%d err=%v", name, bi, slack, n, err)
				}
				for i := len(dst); i < len(buf); i++ {
					if buf[i] != 0xA5 {
						t.Fatalf("%s block %d slack %d: wrote past dst at %d", name, bi, slack, i)
					}
				}
				for i := n + 15; i < len(dst); i++ {
					if buf[i] != 0xA5 {
						t.Fatalf("%s block %d slack %d: scratch beyond 15 bytes at %d (n=%d)", name, bi, slack, i, n)
					}
				}
			}
		}
	}
}

// A destination one byte too small is refused and nothing past it is touched.
func TestDecodeBlockShortDst(t *testing.T) {
	for name, blocks := range liblzo2Blocks(t) {
		b := blocks[0]
		buf := make([]byte, len(b.want)+32)
		for i := range buf {
			buf[i] = 0xEE
		}
		if _, err := DecodeBlock(buf[:len(b.want)-1], b.comp); !errors.Is(err, ErrShortDst) {
			t.Fatalf("%s: err = %v, want ErrShortDst", name, err)
		}
		for i := len(b.want) - 1; i < len(buf); i++ {
			if buf[i] != 0xEE {
				t.Fatalf("%s: wrote past dst at %d", name, i)
			}
		}
	}
}

// DecodeBlock and the instrumented reference decoder (decoder.run, the one the
// corpus tests count instruction classes with) must agree on every input: same
// output when the block is valid, both refusing when it is not.
func FuzzDecodeBlockVsReference(f *testing.F) {
	for _, blocks := range liblzo2Blocks(f) {
		f.Add(blocks[0].comp[:min(len(blocks[0].comp), 300)], 400)
		// Whole blocks, so the fuzzer mutates inside the unchecked fast path
		// (fastRun), which needs 32 bytes of input and 64 of output headroom.
		for _, b := range blocks {
			f.Add(b.comp, len(b.want))
			f.Add(b.comp, len(b.want)+64)
		}
	}
	f.Add([]byte{17, 0, 0, 0, 0, 0}, 0)
	f.Add([]byte{0x15, 'h', 'e', 'l', 'l', 'o', 0x11, 0, 0}, 4)
	f.Fuzz(func(t *testing.T, src []byte, k int) {
		if k < 0 || k > 1<<20 {
			return
		}
		dst := make([]byte, k+9)
		for i := range dst {
			dst[i] = 0xA5
		}
		n, err := DecodeBlock(dst[:k:k], src) // capacity k: an overrun panics instead of hiding in the slack
		for i := k; i < len(dst); i++ {
			if dst[i] != 0xA5 {
				t.Fatalf("wrote past dst at %d", i)
			}
		}
		var d decoder
		d.reset(src, make([]byte, k))
		rerr := d.run()
		if rerr == nil {
			rerr = d.finishLax()
		}
		switch {
		case err == nil && rerr == nil:
			if !bytes.Equal(dst[:n], d.dst[:d.op]) {
				t.Fatalf("outputs differ: %d vs %d bytes", n, d.op)
			}
		case err == nil && rerr != nil:
			t.Fatalf("DecodeBlock accepted what the reference refused: %v", rerr)
		case err != nil && rerr == nil:
			t.Fatalf("DecodeBlock refused what the reference decoded: %v", err)
		}
	})
}

// finishLax is finish without the exact-length requirement: DecodeBlock returns
// how many bytes it wrote, the caller decides whether that is the size it wanted.
func (d *decoder) finishLax() error {
	if d.ip != len(d.src) {
		return errors.New("bytes left after the end-of-stream marker")
	}
	return nil
}

// The refusals and exact-copy paths DecodeBlock has that Decompress's own
// tables do not reach (ErrShortDst, and trailing literals at the end of dst).
func TestDecodeBlockShortDstPaths(t *testing.T) {
	lit6 := []byte{0x03, 'A', 'B', 'C', 'D', 'E', 'F'}
	eos := []byte{0x11, 0x00, 0x00}
	join := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	for _, c := range []struct {
		name string
		src  []byte
		dst  int
	}{
		{"first-byte literals past dst", []byte{20, 'a', 'b', 'c'}, 2},
		{"M4 length continuation past dst", join(lit6, []byte{0x10, 0x00}), 10},
		{"trailing literals past dst", join(lit6, []byte{0x41, 0x00}, []byte{'Z'}), 9},
	} {
		if _, err := DecodeBlock(make([]byte, c.dst), c.src); !errors.Is(err, ErrShortDst) {
			t.Errorf("%s: err = %v, want ErrShortDst", c.name, err)
		}
	}
	// Trailing literals with less than a 4-byte move of room: the exact copy.
	src := join(lit6, []byte{0x41, 0x00}, []byte{'Z'}, eos)
	dst := make([]byte, 10)
	n, err := DecodeBlock(dst, src)
	if err != nil || n != 10 || dst[9] != 'Z' || string(dst[:6]) != "ABCDEF" {
		t.Fatalf("n=%d err=%v dst=%q", n, err, dst)
	}
}
