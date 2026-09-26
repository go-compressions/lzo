package lzo

import (
	"errors"
	"fmt"
)

// ErrCorrupt reports an LZO1X block that cannot be decoded: a truncated
// instruction or operand, a match reaching back before the start of the block,
// an output overrun, or a missing end-of-stream marker.
var ErrCorrupt = errors.New("lzo: corrupt LZO1X block")

// ErrBitstreamVersion reports an LZO1X block carrying a bitstream version
// marker for a version this package does not decode. Version 1 is LZO-RLE, the
// zero-run extension used by zram; see the package documentation.
var ErrBitstreamVersion = errors.New("lzo: unsupported LZO1X bitstream version")

// Decompress decodes one LZO1X block and returns its uncompressedLen bytes.
//
// LZO1X does not record its output length, so uncompressedLen must be supplied
// by the caller from wherever the container kept it. It is exact, not a hint:
// a block that ends after a different number of bytes, or that has bytes left
// over after its end-of-stream marker, is reported as [ErrCorrupt] rather than
// truncated or padded to fit.
//
// The same decoder reads blocks from every LZO1X compressor variant — LZO1X_1,
// LZO1X_1_15 and LZO1X_999 (lzop methods 1, 2 and 3) — because they share one
// bytestream grammar.
func Decompress(src []byte, uncompressedLen int) ([]byte, error) {
	if uncompressedLen < 0 {
		return nil, fmt.Errorf("%w: negative uncompressed length %d", ErrCorrupt, uncompressedLen)
	}
	var d decoder
	d.reset(src, make([]byte, uncompressedLen))
	if err := d.run(); err != nil {
		return nil, err
	}
	if err := d.finish(); err != nil {
		return nil, err
	}
	return d.dst, nil
}

// Instruction classes, counted in decoder.hits so that tests can assert which
// paths a corpus actually reaches. Reaching a path is not the same as having a
// test for it, and a corpus that claims to cover the grammar has to prove it.
const (
	opFirstLiterals    = iota // first-byte literal shortcut (byte 18..255)
	opLiteralRun              // 0..15, state 0: literal run, length in the opcode
	opLiteralRunLong          // 0..15, state 0: literal run, length continuation
	opShort1k                 // 0..15, state 1..3: 2 bytes from <= 1 KiB back
	opShort3k                 // 0..15, state 4: 3 bytes from 2..3 KiB back
	opM4                      // 16..31: 16..48 KiB back, length in the opcode
	opM4Long                  // 16..31: 16..48 KiB back, length continuation
	opM3                      // 32..63: <= 16 KiB back, length in the opcode
	opM3Long                  // 32..63: <= 16 KiB back, length continuation
	opM2                      // 64..127: 3..4 bytes from <= 2 KiB back
	opM2Big                   // 128..255: 5..8 bytes from <= 2 KiB back
	opTrailingLiterals        // the 1..3 literals a match instruction carries
	opCopyOverlap             // a match whose distance is shorter than its length
	opEndOfStream             // the distance-0 M4 instruction
	numOpClasses
)

// decoder holds one in-progress LZO1X block. It is reused across the blocks of
// an lzop stream so that hits accumulate over the whole stream.
type decoder struct {
	src  []byte
	dst  []byte
	ip   int
	op   int
	hits [numOpClasses]uint32
}

func (d *decoder) reset(src, dst []byte) {
	d.src, d.dst, d.ip, d.op = src, dst, 0, 0
}

// finish checks the two things an end-of-stream marker alone does not: that
// the block produced exactly the promised number of bytes, and that nothing
// follows the marker.
func (d *decoder) finish() error {
	if d.op != len(d.dst) {
		return fmt.Errorf("%w: end-of-stream after %d bytes, want %d", ErrCorrupt, d.op, len(d.dst))
	}
	if d.ip != len(d.src) {
		return fmt.Errorf("%w: %d bytes left after the end-of-stream marker", ErrCorrupt, len(d.src)-d.ip)
	}
	return nil
}

// next returns the next input byte.
func (d *decoder) next() (int, error) {
	if d.ip >= len(d.src) {
		return 0, fmt.Errorf("%w: input exhausted at output offset %d", ErrCorrupt, d.op)
	}
	b := d.src[d.ip]
	d.ip++
	return int(b), nil
}

// le16 returns the next little-endian 16-bit operand. Instruction opcodes and
// the container's integers disagree about byte order on purpose: lzop's header
// and block lengths are big-endian, while LZO1X's two-byte distance operands
// are little-endian.
func (d *decoder) le16() (int, error) {
	if len(d.src)-d.ip < 2 {
		return 0, fmt.Errorf("%w: truncated 16-bit operand at input offset %d", ErrCorrupt, d.ip)
	}
	v := int(d.src[d.ip]) | int(d.src[d.ip+1])<<8
	d.ip += 2
	return v, nil
}

// longLength decodes the variable-length continuation used whenever an
// instruction's length field is zero: any number of 0x00 bytes each adding
// 255, then one non-zero byte adding its own value, on top of base.
func (d *decoder) longLength(base int) (int, error) {
	n := base
	room := len(d.dst) - d.op
	for {
		b, err := d.next()
		if err != nil {
			return 0, err
		}
		if b != 0 {
			return n + b, nil
		}
		n += 255
		if n > room {
			return 0, fmt.Errorf("%w: length continuation at input offset %d reaches %d, past the %d bytes left in the output",
				ErrCorrupt, d.ip, n, room)
		}
	}
}

// literals copies n bytes straight from the input to the output.
func (d *decoder) literals(n int) error {
	if n > len(d.src)-d.ip {
		return fmt.Errorf("%w: %d literal bytes wanted at input offset %d, %d available",
			ErrCorrupt, n, d.ip, len(d.src)-d.ip)
	}
	if n > len(d.dst)-d.op {
		return fmt.Errorf("%w: %d literal bytes overrun the %d-byte output at offset %d",
			ErrCorrupt, n, len(d.dst), d.op)
	}
	copy(d.dst[d.op:], d.src[d.ip:d.ip+n])
	d.ip += n
	d.op += n
	return nil
}

// match copies length bytes from distance bytes back in the output.
func (d *decoder) match(distance, length int) error {
	if distance > d.op {
		return fmt.Errorf("%w: match distance %d at output offset %d reaches before the start of the block",
			ErrCorrupt, distance, d.op)
	}
	if length > len(d.dst)-d.op {
		return fmt.Errorf("%w: match of %d bytes overruns the %d-byte output at offset %d",
			ErrCorrupt, length, len(d.dst), d.op)
	}
	if distance >= length {
		copy(d.dst[d.op:d.op+length], d.dst[d.op-distance:])
		d.op += length
		return nil
	}
	// Overlapping match: the source advances into bytes this copy is itself
	// writing, which is how LZO1X spells a run. copy would not reproduce it.
	d.hits[opCopyOverlap]++
	for i := 0; i < length; i++ {
		d.dst[d.op] = d.dst[d.op-distance]
		d.op++
	}
	return nil
}

func (d *decoder) run() error {
	if len(d.src) == 0 {
		return fmt.Errorf("%w: empty block", ErrCorrupt)
	}

	// An opening byte of 17, in a block long enough for it to be meaningful,
	// is a bitstream version marker rather than an instruction.
	if len(d.src) >= 5 && d.src[0] == 17 {
		if v := d.src[1]; v != 0 {
			return fmt.Errorf("%w: %d", ErrBitstreamVersion, v)
		}
		d.ip = 2
	}

	// The first instruction byte follows its own encoding: with no output
	// behind it there is no dictionary to copy from, so values above 17 mean a
	// literal run of byte-17 bytes. This is the only way a run of 1 to 3
	// literals can be spelled.
	state := 0
	if int(d.src[d.ip]) > 17 {
		d.hits[opFirstLiterals]++
		n := int(d.src[d.ip]) - 17
		d.ip++
		if err := d.literals(n); err != nil {
			return err
		}
		// Only three cases are ever distinguished downstream -- no literals, one
		// to three, and four or more -- so the clamp is for readers of this
		// code, not for the decoder. The ablation sweep confirms as much: it is
		// the one mutation the tests cannot see, because there is nothing to
		// see.
		state = min(n, 4)
	}

	for {
		t, err := d.next()
		if err != nil {
			return err
		}

		var length, distance int
		switch {
		case t < 16:
			if state == 0 {
				// A literal run of 4 or more bytes.
				n := t
				if n == 0 {
					d.hits[opLiteralRunLong]++
					if n, err = d.longLength(15); err != nil {
						return err
					}
				} else {
					d.hits[opLiteralRun]++
				}
				if err = d.literals(n + 3); err != nil {
					return err
				}
				state = 4
				continue
			}
			// With literals behind it the same opcode is a short match, whose
			// distance range depends on how many there were.
			h, err := d.next()
			if err != nil {
				return err
			}
			if state < 4 {
				d.hits[opShort1k]++
				length, distance = 2, (h<<2)+(t>>2)+1
			} else {
				d.hits[opShort3k]++
				length, distance = 3, (h<<2)+(t>>2)+2049
			}
			state = t & 3

		case t < 32: // 0 0 0 1 H L L L
			n := t & 7
			if n == 0 {
				d.hits[opM4Long]++
				if n, err = d.longLength(7); err != nil {
					return err
				}
			} else {
				d.hits[opM4]++
			}
			length = n + 2
			v, err := d.le16()
			if err != nil {
				return err
			}
			distance = 16384 + ((t & 8) << 11) + (v >> 2)
			if distance == 16384 {
				d.hits[opEndOfStream]++
				return nil
			}
			state = v & 3

		case t < 64: // 0 0 1 L L L L L
			n := t & 31
			if n == 0 {
				d.hits[opM3Long]++
				if n, err = d.longLength(31); err != nil {
					return err
				}
			} else {
				d.hits[opM3]++
			}
			length = n + 2
			v, err := d.le16()
			if err != nil {
				return err
			}
			distance = (v >> 2) + 1
			state = v & 3

		default: // 0 1 L D D D S S and 1 L L D D D S S
			if t < 128 {
				d.hits[opM2]++
				length = 3 + ((t >> 5) & 1)
			} else {
				d.hits[opM2Big]++
				length = 5 + ((t >> 5) & 3)
			}
			h, err := d.next()
			if err != nil {
				return err
			}
			distance = (h << 3) + ((t >> 2) & 7) + 1
			state = t & 3
		}

		if err = d.match(distance, length); err != nil {
			return err
		}
		if state > 0 {
			d.hits[opTrailingLiterals]++
			if err = d.literals(state); err != nil {
				return err
			}
		}
	}
}
