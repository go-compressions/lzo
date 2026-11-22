package lzo

import (
	stdadler "hash/adler32"
	"strconv"
	"testing"

	simdadler "github.com/go-simd/adler32"
)

// benchSums compares the standard library's Adler-32 with the go-simd kernel
// the container reader now uses, over a typical lzop block (256 KiB) and a
// small one.
func BenchmarkAdler32(b *testing.B) {
	for _, n := range []int{4 << 10, 256 << 10} {
		buf := make([]byte, n)
		for i := range buf {
			buf[i] = byte(i*131 + i>>8)
		}
		b.Run("std/"+sizeName(n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				_ = stdadler.Checksum(buf)
			}
		})
		b.Run("simd/"+sizeName(n), func(b *testing.B) {
			b.SetBytes(int64(n))
			for b.Loop() {
				_ = simdadler.Checksum(buf)
			}
		})
	}
}

func sizeName(n int) string { return strconv.Itoa(n>>10) + "KiB" }

// TestAdler32MatchesStdlib pins the kernel to the standard library over many
// lengths, so a wrong checksum cannot hide behind the container tests.
func TestAdler32MatchesStdlib(t *testing.T) {
	buf := make([]byte, 70000)
	for i := range buf {
		buf[i] = byte(i*7 + i>>9)
	}
	for _, n := range []int{0, 1, 15, 16, 17, 31, 32, 33, 255, 5551, 5552, 5553, 65535, 65536, 70000} {
		if got, want := simdadler.Checksum(buf[:n]), stdadler.Checksum(buf[:n]); got != want {
			t.Fatalf("n=%d: simd %#x, stdlib %#x", n, got, want)
		}
	}
}
