package lzo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/adler32"
	"io"
	"testing"
)

func isErr(err, target error) bool { return err != nil && errors.Is(err, target) }

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// A literal run of six bytes, and the end-of-stream instruction. Between them
// they are enough of a valid block to hang every malformed-input case off.
var (
	lit6 = []byte{0x03, 'A', 'B', 'C', 'D', 'E', 'F'}
	eos  = []byte{0x11, 0x00, 0x00}
)

func TestDecompressRejects(t *testing.T) {
	for _, c := range []struct {
		name   string
		src    []byte
		n      int
		target error
	}{
		{"an empty block", nil, 0, ErrCorrupt},
		{"a negative output length", eos, -1, ErrCorrupt},
		{"no end-of-stream marker", lit6, 6, ErrCorrupt},
		{"bytes after the end-of-stream marker", cat(lit6, eos, []byte{0xFF}), 6, ErrCorrupt},
		{"an end-of-stream marker before the output is full", cat(lit6, eos), 10, ErrCorrupt},
		{"a truncated literal run", []byte{0x03, 'A'}, 6, ErrCorrupt},
		{"a literal run past the end of the output", lit6, 4, ErrCorrupt},
		{"a truncated literal length continuation", []byte{0x00}, 100, ErrCorrupt},
		{"a literal length continuation past the end of the output", []byte{0x00, 0x00, 0x01}, 10, ErrCorrupt},
		{"a truncated short-match distance byte", []byte{18, 'A', 0x00}, 3, ErrCorrupt},
		{"a truncated M2 distance byte", cat(lit6, []byte{0x40}), 9, ErrCorrupt},
		{"a match reaching before the start of the block", cat(lit6, []byte{0x40, 0xFF}, eos), 9, ErrCorrupt},
		{"a match past the end of the output", cat(lit6, []byte{0x40, 0x00}, eos), 8, ErrCorrupt},
		{"a truncated M3 distance operand", cat(lit6, []byte{0x21, 0x00}), 9, ErrCorrupt},
		{"a truncated M3 length continuation", cat(lit6, []byte{0x20}), 40, ErrCorrupt},
		{"an M3 length continuation past the end of the output", cat(lit6, []byte{0x20, 0x00, 0x01}), 10, ErrCorrupt},
		{"a truncated M4 distance operand", cat(lit6, []byte{0x11, 0x00}), 9, ErrCorrupt},
		{"a truncated M4 length continuation", cat(lit6, []byte{0x10}), 40, ErrCorrupt},
		{"a truncated M4 distance operand after a length continuation", cat(lit6, []byte{0x10, 0x01, 0x00}), 40, ErrCorrupt},
		{"truncated trailing literals", cat(lit6, []byte{0x41, 0x00}), 10, ErrCorrupt},
		{"a truncated first-byte literal run", []byte{0xFF, 'A'}, 238, ErrCorrupt},
		{"LZO-RLE, bitstream version 1", cat([]byte{17, 1}, lit6, eos), 6, ErrBitstreamVersion},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Decompress(c.src, c.n)
			if !isErr(err, c.target) {
				t.Fatalf("got (%d bytes, %v), want %v", len(got), err, c.target)
			}
			if got != nil {
				t.Errorf("returned %d bytes alongside the error", len(got))
			}
			t.Logf("%v", err)
		})
	}
}

// TestDecompressEmptyOutput covers the shortest block there is: nothing but the
// end-of-stream marker, which is what a zero-length output looks like.
func TestDecompressEmptyOutput(t *testing.T) {
	got, err := Decompress(eos, 0)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d bytes, want 0", len(got))
	}
}

// --------------------------------------------------------------------------
// Container-level malformed input.
//
// Every case below is a single-field mutation of one lzop-validated crafted
// stream, so the control for all of them is that the unmutated stream decodes.
// The offsets are fixed by that stream's header: a 7-byte filename, no filter
// field, and the Adler-32 of the uncompressed data as the only block checksum.
// --------------------------------------------------------------------------

const (
	offVersion = 9  // the first checksummed header byte
	offNeeded  = 13 // versionNeeded
	offMethod  = 15
	offFlags   = 17
	offNameLen = 33
	offHdrSum  = 41 // end of the checksummed region, start of its checksum
	offBlock   = 45 // first block: uncompressed length, compressed length, ...
	offCompLen = 49
	offPlainCk = 53 // the block's Adler-32 of the uncompressed data
	offPayload = 57
)

// base returns a copy of the named crafted stream, which lzop -dc confirmed.
func base(t testing.TB, name string) []byte {
	t.Helper()
	for _, c := range craftedCases(t) {
		if c.Name == name {
			return hexBytes(t, c.Stream)
		}
	}
	t.Fatalf("crafted case %q not found", name)
	return nil
}

// reseal recomputes the Adler-32 header checksum after a header field has been
// changed, so that a test of one field is not silently a test of the checksum.
func reseal(s []byte) []byte {
	binary.BigEndian.PutUint32(s[offHdrSum:], adler32.Checksum(s[offVersion:offHdrSum]))
	return s
}

// TestHeaderOffsetsAreWhatTheTestsAssume is the control for every mutation
// below: if the offsets or the checksummed region were wrong, resealing an
// untouched stream would change it.
func TestHeaderOffsetsAreWhatTheTestsAssume(t *testing.T) {
	s := base(t, "method-1")
	if got := binary.BigEndian.Uint16(s[offVersion:]); got != 0x1040 {
		t.Errorf("version at %d is %#04x, want 0x1040", offVersion, got)
	}
	if got := binary.BigEndian.Uint16(s[offNeeded:]); got != 0x0940 {
		t.Errorf("versionNeeded at %d is %#04x, want 0x0940", offNeeded, got)
	}
	if s[offMethod] != 1 {
		t.Errorf("method at %d is %d, want 1", offMethod, s[offMethod])
	}
	if got := binary.BigEndian.Uint32(s[offFlags:]); got != 0x03000001 {
		t.Errorf("flags at %d are %#08x, want 0x03000001", offFlags, got)
	}
	if s[offNameLen] != 7 {
		t.Errorf("name length at %d is %d, want 7", offNameLen, s[offNameLen])
	}
	sealed := reseal(bytes.Clone(s))
	if !bytes.Equal(sealed, s) {
		t.Fatalf("resealing an untouched stream changed it: the checksummed " +
			"region or the checksum offset is wrong, and every mutation test " +
			"below would be testing the checksum instead of its field")
	}
}

func TestNewReaderRejects(t *testing.T) {
	for _, c := range []struct {
		name   string
		stream func(t testing.TB) []byte
		target error
	}{
		{"an empty input", func(testing.TB) []byte { return nil }, ErrHeader},
		{"a truncated magic", func(testing.TB) []byte { return magic[:4] }, ErrHeader},
		{"a stream that is not lzop", func(testing.TB) []byte {
			return append([]byte("not lzop!"), 0, 0, 0, 0)
		}, ErrMagic},
		{"a header truncated after the version", func(t testing.TB) []byte {
			return base(t, "method-1")[:offVersion+2]
		}, ErrHeader},
		{"a header truncated inside the filename", func(t testing.TB) []byte {
			return base(t, "method-1")[:offNameLen+3]
		}, ErrHeader},
		{"a header with no checksum", func(t testing.TB) []byte {
			return base(t, "method-1")[:offHdrSum]
		}, ErrHeader},
		{"a wrong Adler-32 header checksum", func(t testing.TB) []byte {
			s := base(t, "method-1")
			s[offHdrSum] ^= 0x80
			return s
		}, ErrChecksum},
		{"a wrong CRC-32 header checksum", func(t testing.TB) []byte {
			s := base(t, "crc32-header-and-data")
			s[offHdrSum] ^= 0x80
			return s
		}, ErrChecksum},
		{"a header checksum of the wrong kind", func(t testing.TB) []byte {
			// Claim a CRC-32 header checksum while leaving the Adler-32 there.
			s := base(t, "method-1")
			s[offFlags+2] |= fHCRC32 >> 8
			return reseal(s)
		}, ErrChecksum},
		{"a stream demanding a newer lzop", func(t testing.TB) []byte {
			s := base(t, "method-1")
			binary.BigEndian.PutUint16(s[offNeeded:], maxLzopVersion+1)
			return reseal(s)
		}, ErrUnsupported},
		{"a compression method that is not LZO1X", func(t testing.TB) []byte {
			s := base(t, "method-1")
			s[offMethod] = 4
			return reseal(s)
		}, ErrUnsupported},
		{"a delta filter", func(t testing.TB) []byte {
			return fixture(t, "text-filter1.lzo")
		}, ErrUnsupported},
		{"a header extra field", func(t testing.TB) []byte {
			s := base(t, "method-1")
			s[offFlags+3] |= fHExtraField
			return reseal(s)
		}, ErrUnsupported},
	} {
		t.Run(c.name, func(t *testing.T) {
			z, err := NewReader(bytes.NewReader(c.stream(t)))
			if !isErr(err, c.target) {
				t.Fatalf("got (%v, %v), want %v", z, err, c.target)
			}
			if z != nil {
				t.Error("returned a reader alongside the error")
			}
			t.Logf("%v", err)
		})
	}
}

func TestReadRejects(t *testing.T) {
	for _, c := range []struct {
		name   string
		stream func(t testing.TB) []byte
		target error
	}{
		{"no end-of-stream block", func(t testing.TB) []byte {
			return base(t, "method-1")[:offHdrSum+4]
		}, ErrHeader},
		{"a missing terminator after the last block", func(t testing.TB) []byte {
			s := base(t, "method-1")
			return s[:len(s)-4]
		}, ErrHeader},
		{"a block length over the size limit", func(t testing.TB) []byte {
			s := base(t, "method-1")
			binary.BigEndian.PutUint32(s[offBlock:], maxBlockSize+1)
			return s
		}, ErrHeader},
		{"a block truncated after its uncompressed length", func(t testing.TB) []byte {
			return base(t, "method-1")[:offCompLen]
		}, ErrHeader},
		{"a zero compressed length", func(t testing.TB) []byte {
			s := base(t, "method-1")
			binary.BigEndian.PutUint32(s[offCompLen:], 0)
			return s
		}, ErrHeader},
		{"a compressed length over the uncompressed length", func(t testing.TB) []byte {
			// The uncompressed length is lowered rather than the compressed one
			// raised, so that the payload is still all there: raising it would
			// make this a truncation test instead, and the guard would go
			// untested. The ablation sweep found exactly that.
			s := base(t, "method-1")
			binary.BigEndian.PutUint32(s[offBlock:],
				binary.BigEndian.Uint32(s[offCompLen:])-1)
			return s
		}, ErrHeader},
		{"a block truncated before its data Adler-32", func(t testing.TB) []byte {
			return base(t, "method-1")[:offPlainCk]
		}, ErrHeader},
		{"a block truncated before its data CRC-32", func(t testing.TB) []byte {
			return base(t, "all-checksums")[:offPlainCk+4]
		}, ErrHeader},
		{"a block truncated before its compressed Adler-32", func(t testing.TB) []byte {
			return base(t, "all-checksums")[:offPlainCk+8]
		}, ErrHeader},
		{"a block truncated before its compressed CRC-32", func(t testing.TB) []byte {
			return base(t, "all-checksums")[:offPlainCk+12]
		}, ErrHeader},
		{"a truncated payload", func(t testing.TB) []byte {
			return base(t, "method-1")[:offPayload+4]
		}, ErrHeader},
		{"a wrong data Adler-32", func(t testing.TB) []byte {
			s := base(t, "method-1")
			s[offPlainCk] ^= 0x80
			return s
		}, ErrChecksum},
		{"a wrong data CRC-32", func(t testing.TB) []byte {
			s := base(t, "all-checksums")
			s[offPlainCk+4] ^= 0x80
			return s
		}, ErrChecksum},
		{"a wrong compressed Adler-32", func(t testing.TB) []byte {
			s := base(t, "all-checksums")
			s[offPlainCk+8] ^= 0x80
			return s
		}, ErrChecksum},
		{"a wrong compressed CRC-32", func(t testing.TB) []byte {
			s := base(t, "all-checksums")
			s[offPlainCk+12] ^= 0x80
			return s
		}, ErrChecksum},
		{"a payload that decodes to the wrong bytes", func(t testing.TB) []byte {
			// Flip a literal, which decodes cleanly and fails the data checksum.
			s := base(t, "method-1")
			s[offPayload+2] ^= 0x20
			return s
		}, ErrChecksum},
		{"a block that decodes to fewer bytes than it promised", func(t testing.TB) []byte {
			// The block header, not the bytestream, is what says how long the
			// output is; a disagreement between them is corruption.
			s := base(t, "method-1")
			binary.BigEndian.PutUint32(s[offBlock:],
				binary.BigEndian.Uint32(s[offBlock:])+1)
			return s
		}, ErrCorrupt},
		{"a corrupt LZO1X payload", func(t testing.TB) []byte {
			// A match instruction as the first byte of a block: there is no
			// output behind it to copy from.
			s := base(t, "method-1")
			s[offPayload] = 0xFF
			return s
		}, ErrCorrupt},
	} {
		t.Run(c.name, func(t *testing.T) {
			z, err := NewReader(bytes.NewReader(c.stream(t)))
			if err != nil {
				t.Fatalf("NewReader: %v (this case is meant to fail on Read)", err)
			}
			_, err = io.ReadAll(z)
			if !isErr(err, c.target) {
				t.Fatalf("ReadAll: got %v, want %v", err, c.target)
			}
			// The error must stick: a second Read may not quietly resume.
			if _, again := z.Read(make([]byte, 1)); again == nil || again.Error() != err.Error() {
				t.Errorf("second Read returned %v, want the same error", again)
			}
			t.Logf("%v", err)
		})
	}
}

// TestUnexpectedEOFIsDistinguished checks the two ways the input can end short:
// at a field boundary (a plain EOF, which must be reported as truncation) and
// part-way through a field.
func TestUnexpectedEOFIsDistinguished(t *testing.T) {
	for _, n := range []int{offHdrSum, offHdrSum + 2} {
		s := base(t, "method-1")[:n]
		_, err := NewReader(bytes.NewReader(s))
		if !isErr(err, ErrHeader) || !isErr(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%d bytes: got %v, want ErrHeader wrapping io.ErrUnexpectedEOF", n, err)
		}
	}
}

// FuzzDecompress and FuzzNewReader run their seed corpus on every ordinary test
// run. Neither asserts an output: the property is that no input, however
// malformed, panics or reads outside its buffers.
func FuzzDecompress(f *testing.F) {
	f.Add(cat(lit6, eos), 6)
	f.Add(eos, 0)
	f.Add([]byte{0x00}, 100)
	for _, c := range craftedCases(f) {
		if !c.Stored {
			f.Add(hexBytes(f, c.Block), len(hexBytes(f, c.Plain)))
		}
	}
	f.Fuzz(func(t *testing.T, src []byte, n int) {
		if n < 0 || n > 1<<20 {
			t.Skip("output length outside the range this target allocates for")
		}
		got, err := Decompress(src, n)
		if err == nil && len(got) != n {
			t.Fatalf("no error but %d bytes, want %d", len(got), n)
		}
	})
}

func FuzzNewReader(f *testing.F) {
	f.Add(fixture(f, "one.lzo"))
	f.Add(fixture(f, "empty.lzo"))
	f.Add(base(f, "method-1"))
	f.Fuzz(func(t *testing.T, stream []byte) {
		z, err := NewReader(bytes.NewReader(stream))
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, z)
	})
}
