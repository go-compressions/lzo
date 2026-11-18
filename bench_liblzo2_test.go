//go:build liblzo2 && cgo

package lzo

import (
	"bytes"
	"math/rand/v2"
	"testing"

	"github.com/go-compressions/lzo/internal/liblzo2"
)

// btrfsBlocks builds the btrfs read workload: 4 KiB pages of a text-like corpus
// (200-word vocabulary, ratio about 2) compressed by liblzo2's lzo1x_1_compress.
func btrfsBlocks(tb testing.TB) (packed []byte, off, clen []uintptr, plain []byte) {
	const blk, count = 4096, 1024
	r := rand.New(rand.NewPCG(7, 9))
	vocab := make([][]byte, 200)
	for i := range vocab {
		w := make([]byte, 3+r.IntN(8))
		for j := range w {
			w[j] = byte('a' + r.IntN(26))
		}
		vocab[i] = w
	}
	for len(plain) < blk*count {
		w := vocab[int(float64(len(vocab))*r.Float64()*r.Float64())]
		plain = append(plain, w...)
		plain = append(plain, ' ')
	}
	plain = plain[:blk*count]
	for i := 0; i < count; i++ {
		c := liblzo2.Compress(plain[i*blk : (i+1)*blk])
		off = append(off, uintptr(len(packed)))
		clen = append(clen, uintptr(len(c)))
		packed = append(packed, c...)
	}
	tb.Logf("ratio %.2f", float64(len(plain))/float64(len(packed)))
	return
}

// TestDecodeBlockVsLiblzo2Compressor decodes liblzo2's own 4 KiB blocks.
func TestDecodeBlockVsLiblzo2Compressor(t *testing.T) {
	packed, off, clen, plain := btrfsBlocks(t)
	dst := make([]byte, 4096)
	for i := range off {
		n, err := DecodeBlock(dst, packed[off[i]:off[i]+clen[i]])
		if err != nil || !bytes.Equal(dst[:n], plain[i*4096:(i+1)*4096]) {
			t.Fatalf("block %d: n=%d err=%v", i, n, err)
		}
	}
}

func BenchmarkBtrfsPages(b *testing.B) {
	packed, off, clen, plain := btrfsBlocks(b)
	out := make([]byte, len(plain)+16)
	b.Run("go", func(b *testing.B) {
		b.SetBytes(int64(len(plain)))
		b.ReportAllocs()
		for b.Loop() {
			for i := range off {
				if _, err := DecodeBlock(out[i*4096:(i+1)*4096], packed[off[i]:off[i]+clen[i]]); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("liblzo2", func(b *testing.B) {
		b.SetBytes(int64(len(plain)))
		for b.Loop() {
			if liblzo2.DecompressAll(packed, off, clen, out, 4096) != len(plain) {
				b.Fatal("decode")
			}
		}
	})
}
