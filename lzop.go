package lzo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/adler32"
	"hash/crc32"
	"io"
)

// Container-level errors. Each is distinguishable with [errors.Is] so a caller
// can tell "this is not an lzop file" from "this is one, and I cannot read it".
var (
	// ErrMagic reports a stream that does not begin with the lzop magic.
	ErrMagic = errors.New("lzo: not an lzop stream")

	// ErrHeader reports a malformed or unreadable lzop header or block header.
	ErrHeader = errors.New("lzo: malformed lzop stream")

	// ErrChecksum reports a header or block checksum that does not match.
	ErrChecksum = errors.New("lzo: lzop checksum mismatch")

	// ErrUnsupported reports a well-formed lzop stream this package declines to
	// decode: an unknown compression method, a delta filter, an extra-field
	// header block, or a stream that demands a newer lzop than 1.04.
	ErrUnsupported = errors.New("lzo: unsupported lzop stream")
)

// The nine-byte lzop magic.
var magic = [9]byte{0x89, 0x4c, 0x5a, 0x4f, 0x00, 0x0d, 0x0a, 0x1a, 0x0a}

// Header flag bits. Only the ones that change how the stream is parsed are
// named; the rest (F_STDIN, F_STDOUT, the OS and character-set bytes) are
// provenance metadata that a decoder does not act on.
//
// Note in particular that the filter flag is 0x0800 and 0x0040 is the
// extra-field flag. Both were confirmed against real lzop output: `lzop
// --filter=1` sets 0x0800 and writes one u32 after the flags.
const (
	fAdler32D      = 0x00000001 // Adler-32 of each block's uncompressed data
	fAdler32C      = 0x00000002 // Adler-32 of each block's compressed data
	fCRC32D        = 0x00000100 // CRC-32 of each block's uncompressed data
	fCRC32C        = 0x00000200 // CRC-32 of each block's compressed data
	fHExtraField   = 0x00000040 // a header extra field follows the header
	fHFilter       = 0x00000800 // a u32 filter id sits between flags and mode
	fHCRC32        = 0x00001000 // the header checksum is CRC-32, not Adler-32
	fVersionFields = 0x0940     // versions below this lack three header fields
)

// maxLzopVersion is lzop 1.04, the newest release of the tool. A stream whose
// versionNeeded exceeds it was written against a format revision that did not
// exist when this package was written, so its layout cannot be assumed.
const maxLzopVersion = 0x1040

// maxBlockSize caps a block's claimed uncompressed length. lzop's own maximum
// is 64 MiB; the cap keeps a hostile four-byte length from becoming a 4 GiB
// allocation.
const maxBlockSize = 64 << 20

// NewReader returns an [io.Reader] yielding the uncompressed contents of the
// lzop (.lzo) stream in r.
//
// The file header is read and verified before NewReader returns, so a stream
// that is not lzop, or is lzop this package declines to decode, is reported
// here rather than on the first Read. Block and header checksums are verified
// whenever the stream carries them; a mismatch is reported as [ErrChecksum].
//
// lzop stores its own integers big-endian, and its blocks each carry both
// lengths, so a block whose compressed length equals its uncompressed length is
// stored verbatim rather than compressed — which is what lzop does with
// incompressible input. Such blocks are passed through, not handed to the
// LZO1X decoder.
func NewReader(r io.Reader) (io.Reader, error) {
	z := &reader{r: r}
	if err := z.readHeader(); err != nil {
		return nil, err
	}
	return z, nil
}

type reader struct {
	r     io.Reader
	flags uint32

	dec   decoder
	plain []byte // scratch holding the decompressed bytes of the current block
	comp  []byte // scratch holding the compressed bytes of the current block
	block []byte // the current block's bytes: a view of plain or of comp
	off   int    // how much of block has been handed out
	err   error

	// Observed while reading, for tests that have to prove a corpus reaches
	// the paths it claims to.
	blocks int
	stored int
}

func (z *reader) Read(p []byte) (int, error) {
	// The error is checked before the buffer, not after: a block that failed
	// its checksum must not still be readable.
	if z.err != nil {
		return 0, z.err
	}
	if z.off >= len(z.block) {
		if err := z.nextBlock(); err != nil {
			z.err = err
			z.block, z.off = nil, 0
			return 0, err
		}
	}
	n := copy(p, z.block[z.off:])
	z.off += n
	return n, nil
}

// readFull fills buf, turning a short read into ErrHeader. An lzop stream ends
// with an explicit zero-length block, so end of file anywhere else is
// truncation, not termination.
func (z *reader) readFull(buf []byte, what string) error {
	if _, err := io.ReadFull(z.r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return fmt.Errorf("%w: reading %s: %w", ErrHeader, what, err)
	}
	return nil
}

// hdr accumulates the header bytes that the header checksum covers.
type hdr struct {
	z   *reader
	buf []byte
	err error
}

// take appends the next n header bytes to the checksummed region and returns
// them. After the first failure it is a no-op returning zeros, so the header
// can be parsed straight through and checked once at the end.
func (h *hdr) take(n int, what string) []byte {
	if h.err != nil {
		return make([]byte, n)
	}
	off := len(h.buf)
	h.buf = append(h.buf, make([]byte, n)...)
	if err := h.z.readFull(h.buf[off:], what); err != nil {
		h.err = err
		h.buf = h.buf[:off]
		return make([]byte, n)
	}
	return h.buf[off:]
}

func (h *hdr) u8(what string) uint8   { return h.take(1, what)[0] }
func (h *hdr) u16(what string) uint16 { return binary.BigEndian.Uint16(h.take(2, what)) }
func (h *hdr) u32(what string) uint32 { return binary.BigEndian.Uint32(h.take(4, what)) }

func (z *reader) readHeader() error {
	var m [len(magic)]byte
	if err := z.readFull(m[:], "magic"); err != nil {
		return err
	}
	if m != magic {
		return fmt.Errorf("%w: magic is % x, want % x", ErrMagic, m[:], magic[:])
	}

	// Everything from here to the header checksum is big-endian, and every byte
	// of it feeds the checksum.
	h := &hdr{z: z}
	version := h.u16("version")
	h.u16("library version")
	versionNeeded := uint16(0)
	if version >= fVersionFields {
		versionNeeded = h.u16("version needed")
	}
	method := h.u8("method")
	if version >= fVersionFields {
		h.u8("level")
	}
	flags := h.u32("flags")
	if flags&fHFilter != 0 {
		h.u32("filter")
	}
	h.u32("mode")
	h.u32("mtime low")
	if version >= fVersionFields {
		h.u32("mtime high")
	}
	nameLen := h.u8("name length")
	h.take(int(nameLen), "name")
	if h.err != nil {
		return h.err
	}

	var stored [4]byte
	if err := z.readFull(stored[:], "header checksum"); err != nil {
		return err
	}
	want := binary.BigEndian.Uint32(stored[:])
	got := adler32.Checksum(h.buf)
	kind := "Adler-32"
	if flags&fHCRC32 != 0 {
		got = crc32.ChecksumIEEE(h.buf)
		kind = "CRC-32"
	}
	if got != want {
		return fmt.Errorf("%w: header %s is %#08x, want %#08x", ErrChecksum, kind, got, want)
	}

	// Only now that the header is known intact is it worth judging it.
	if versionNeeded > maxLzopVersion {
		return fmt.Errorf("%w: needs lzop %#04x, this decoder knows up to %#04x",
			ErrUnsupported, versionNeeded, maxLzopVersion)
	}
	switch method {
	case 1, 2, 3: // LZO1X_1, LZO1X_1_15, LZO1X_999 — one bytestream, one decoder.
	default:
		return fmt.Errorf("%w: compression method %d is not LZO1X", ErrUnsupported, method)
	}
	if flags&fHFilter != 0 {
		return fmt.Errorf("%w: stream uses a delta filter", ErrUnsupported)
	}
	if flags&fHExtraField != 0 {
		return fmt.Errorf("%w: stream carries a header extra field", ErrUnsupported)
	}

	z.flags = flags
	return nil
}

// optionalSum reads a four-byte checksum if the given flag is set.
func (z *reader) optionalSum(flag uint32, what string) (uint32, bool, error) {
	if z.flags&flag == 0 {
		return 0, false, nil
	}
	var b [4]byte
	if err := z.readFull(b[:], what); err != nil {
		return 0, false, err
	}
	return binary.BigEndian.Uint32(b[:]), true, nil
}

func verify(data []byte, sum uint32, present bool, f func([]byte) uint32, what string) error {
	if !present {
		return nil
	}
	if got := f(data); got != sum {
		return fmt.Errorf("%w: %s is %#08x, want %#08x", ErrChecksum, what, got, sum)
	}
	return nil
}

func (z *reader) nextBlock() error {
	var b [4]byte
	if err := z.readFull(b[:], "block uncompressed length"); err != nil {
		return err
	}
	plainLen := binary.BigEndian.Uint32(b[:])
	if plainLen == 0 {
		// The zero length is the end of the stream, not a zero-length block.
		return io.EOF
	}
	if plainLen > maxBlockSize {
		return fmt.Errorf("%w: block claims %d uncompressed bytes, over the %d-byte limit",
			ErrHeader, plainLen, maxBlockSize)
	}
	if err := z.readFull(b[:], "block compressed length"); err != nil {
		return err
	}
	compLen := binary.BigEndian.Uint32(b[:])
	if compLen == 0 || compLen > plainLen {
		return fmt.Errorf("%w: block of %d uncompressed bytes claims %d compressed bytes",
			ErrHeader, plainLen, compLen)
	}

	plainAdler, hasPlainAdler, err := z.optionalSum(fAdler32D, "block data Adler-32")
	if err != nil {
		return err
	}
	plainCRC, hasPlainCRC, err := z.optionalSum(fCRC32D, "block data CRC-32")
	if err != nil {
		return err
	}
	compAdler, hasCompAdler, err := z.optionalSum(fAdler32C, "compressed data Adler-32")
	if err != nil {
		return err
	}
	compCRC, hasCompCRC, err := z.optionalSum(fCRC32C, "compressed data CRC-32")
	if err != nil {
		return err
	}

	z.comp = grow(z.comp, int(compLen))
	if err := z.readFull(z.comp, "block payload"); err != nil {
		return err
	}
	if err := verify(z.comp, compAdler, hasCompAdler, adler32.Checksum, "compressed data Adler-32"); err != nil {
		return err
	}
	if err := verify(z.comp, compCRC, hasCompCRC, crc32.ChecksumIEEE, "compressed data CRC-32"); err != nil {
		return err
	}

	var plain []byte
	if compLen == plainLen {
		// Stored, not compressed. lzop falls back to this whenever LZO1X would
		// make the block bigger, which incompressible input always does. A
		// decoder that hands these to the LZO1X decoder fails on random data.
		z.stored++
		plain = z.comp
	} else {
		z.plain = grow(z.plain, int(plainLen))
		z.dec.reset(z.comp, z.plain)
		if err := z.dec.run(); err != nil {
			return err
		}
		if err := z.dec.finish(); err != nil {
			return err
		}
		plain = z.plain
	}

	if err := verify(plain, plainAdler, hasPlainAdler, adler32.Checksum, "block data Adler-32"); err != nil {
		return err
	}
	if err := verify(plain, plainCRC, hasPlainCRC, crc32.ChecksumIEEE, "block data CRC-32"); err != nil {
		return err
	}

	// Only a block that has passed everything becomes readable.
	z.block = plain
	z.off = 0
	z.blocks++
	return nil
}

func grow(b []byte, n int) []byte {
	if cap(b) >= n {
		return b[:n]
	}
	return make([]byte, n)
}
