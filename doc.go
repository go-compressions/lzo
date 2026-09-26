// Package lzo decodes the LZO1X compressed bytestream and the lzop (.lzo)
// container, in pure Go, with no cgo and no third-party dependency.
//
// # Provenance and licence
//
// This is an independent implementation written from a published prose
// description of the LZO1X bytestream — principally "LZO stream format as
// understood by Linux's LZO decompressor" by Willy Tarreau and Dave Rodgman,
// distributed as Documentation/staging/lzo.rst in the Linux source tree and
// published at https://docs.kernel.org/staging/lzo.html — cross-checked
// end-to-end against streams produced by the lzop(1) command-line tool,
// treated purely as a black box.
//
// No liblzo2 code, and no port or translation of liblzo2 code, was read,
// consulted or adapted. liblzo2 is GPL-2.0; this package is BSD-3-Clause, and
// the two must not be mixed. The lzop container layout was likewise derived
// from a field-by-field description plus empirical inspection of real .lzo
// files, not from lzop's sources.
//
// # Decoder only
//
// There is deliberately no compressor here. Writing .lzo serves nobody: every
// consumer of the format already reads gzip, zstd or xz, all of which compress
// better, and a second encoder would double the surface area that has to be
// proved byte-exact while adding no capability a reader does not already give.
// Decoding is what lets Go programs open the .lzo files that already exist —
// Hadoop splits, embedded firmware images, kernel initramfs payloads, old
// archives — and that is the whole scope.
//
// # The two layers
//
// [Decompress] decodes one LZO1X block. LZO1X blocks are not self-delimiting
// in their output length, so the caller must supply it; containers such as
// lzop store it alongside each block.
//
// [NewReader] decodes a whole lzop stream: it parses and checksum-verifies the
// file header, then walks the block list, decompressing each block and
// verifying its checksums.
//
// Methods 1 (LZO1X_1), 2 (LZO1X_1_15) and 3 (LZO1X_999) all decode with the
// same LZO1X decoder. They look like three formats and they are one: only the
// compressor differs — how hard it searches for matches — and every one of
// them emits the same instruction grammar. A decoder that special-cased them
// would be three copies of one thing.
//
// # What is not supported
//
// LZO-RLE (bitstream version 1, the zram zero-run extension) is rejected
// rather than guessed at: no witness for it is available to verify against
// here, and a decoder that silently mis-decodes is worse than one that says
// no. lzop's delta filters and its extra-field header block are rejected for
// the same reason. Each returns a distinguishable error.
package lzo
