//go:build liblzo2 && cgo

// Package liblzo2 binds the reference C library for benchmarks and the
// differential test only; it is never part of the shipped module surface.
package liblzo2

/*
#cgo CFLAGS: -I/opt/homebrew/include
#cgo LDFLAGS: -L/opt/homebrew/lib -llzo2
#include <lzo/lzo1x.h>
#include <stdlib.h>

static int init_lzo(void) { return lzo_init(); }

static size_t comp(const unsigned char *in, size_t n, unsigned char *out) {
	lzo_uint olen = 0;
	void *wrk = malloc(LZO1X_1_MEM_COMPRESS);
	lzo1x_1_compress(in, n, out, &olen, wrk);
	free(wrk);
	return olen;
}

// decompAll decodes count blocks in one call so a benchmark measures the
// decoder, not one cgo crossing per 4 KiB block.
static long decompAll(const unsigned char *in, const size_t *off, const size_t *clen,
		unsigned char *out, size_t blk, size_t count) {
	long total = 0;
	for (size_t i = 0; i < count; i++) {
		lzo_uint olen = blk;
		if (lzo1x_decompress_safe(in + off[i], clen[i], out + i*blk, &olen, NULL) != LZO_E_OK) return -1;
		total += olen;
	}
	return total;
}
*/
import "C"

import "unsafe"

func init() {
	if C.init_lzo() != 0 {
		panic("lzo_init failed")
	}
}

// Compress is lzo1x_1_compress, the encoder the Linux kernel uses.
func Compress(in []byte) []byte {
	out := make([]byte, len(in)+len(in)/16+64+3)
	n := C.comp((*C.uchar)(unsafe.Pointer(&in[0])), C.size_t(len(in)), (*C.uchar)(unsafe.Pointer(&out[0])))
	return out[:n]
}

// DecompressAll decodes blocks (concatenated in packed, at off/clen) of blk
// bytes each into out, in a single cgo call; it returns -1 on error.
func DecompressAll(packed []byte, off, clen []uintptr, out []byte, blk int) int {
	return int(C.decompAll((*C.uchar)(unsafe.Pointer(&packed[0])), (*C.size_t)(unsafe.Pointer(&off[0])),
		(*C.size_t)(unsafe.Pointer(&clen[0])), (*C.uchar)(unsafe.Pointer(&out[0])), C.size_t(blk), C.size_t(len(off))))
}
