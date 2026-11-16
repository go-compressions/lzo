//go:build unix

package lzo

import (
	"bytes"
	"syscall"
	"testing"
)

// DecodeBlock moves 8 and 16 bytes at a time near the ends of its buffers; it
// must not read past src or write past dst. Both end flush against a PROT_NONE
// page.
func TestDecodeBlockStaysInsideBuffers(t *testing.T) {
	page := syscall.Getpagesize()
	size := (1<<18 + page - 1) / page * page
	mk := func() []byte {
		m, err := syscall.Mmap(-1, 0, size+page, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
		if err != nil {
			t.Skip("mmap:", err)
		}
		if err := syscall.Mprotect(m[size:], syscall.PROT_NONE); err != nil {
			t.Skip("mprotect:", err)
		}
		t.Cleanup(func() { syscall.Munmap(m) })
		return m
	}
	srcMem, dstMem := mk(), mk()
	for name, blocks := range liblzo2Blocks(t) {
		for i, b := range blocks {
			src := srcMem[size-len(b.comp) : size : size]
			copy(src, b.comp)
			dst := dstMem[size-len(b.want) : size : size]
			n, err := DecodeBlock(dst, src)
			if err != nil || n != len(b.want) || !bytes.Equal(dst[:n], b.want) {
				t.Fatalf("%s block %d: n=%d err=%v", name, i, n, err)
			}
		}
	}
}
