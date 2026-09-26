package lzo

import (
	"bytes"
	"io"
	"testing"
)

func BenchmarkNewReader(b *testing.B) {
	for _, c := range []struct{ lzo, src string }{
		{"text.lzo", "text.txt"},
		{"zeros.lzo", "zeros.bin"},
		{"frag.lzo", "frag.bin"},
		{"rand.lzo", "rand.bin"},
	} {
		stream := fixture(b, c.lzo)
		b.Run(c.lzo, func(b *testing.B) {
			b.SetBytes(int64(len(fixture(b, c.src))))
			b.ReportAllocs()
			for b.Loop() {
				z, err := NewReader(bytes.NewReader(stream))
				if err != nil {
					b.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, z); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecompress(b *testing.B) {
	for _, c := range craftedCases(b) {
		if c.Name != "state4-short-match" && c.Name != "m3-m4-trailing-literals" {
			continue
		}
		block, plain := hexBytes(b, c.Block), hexBytes(b, c.Plain)
		b.Run(c.Name, func(b *testing.B) {
			b.SetBytes(int64(len(plain)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Decompress(block, len(plain)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
