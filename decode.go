package lzo

import (
	"errors"
	"fmt"
)

// ErrShortDst reports an output buffer too small for the block.
var ErrShortDst = errors.New("lzo: destination too small")

// DecodeBlock decodes one LZO1X block into dst and returns the number of bytes
// written. It allocates nothing and never reads or writes outside src and dst:
// the caller owns dst, sized for the largest output it accepts (SquashFS and
// btrfs know the exact size). A block that would write past len(dst) is refused
// with [ErrShortDst], a malformed one with [ErrCorrupt] (or
// [ErrBitstreamVersion]); bytes left over after the end-of-stream marker are
// [ErrCorrupt], as liblzo2's LZO_E_INPUT_NOT_CONSUMED.
//
// It reads the same grammar as [Decompress] (LZO1X_1, _1_15 and _999 share it).
// The common instructions are decoded with fixed-size 8- and 16-byte moves while
// both buffers are far enough from their ends; the last few bytes and every
// unusual case take exact paths, so the result never depends on the margin.
func DecodeBlock(dst, src []byte) (int, error) {
	if len(src) == 0 {
		return 0, fmt.Errorf("%w: empty block", ErrCorrupt)
	}
	ip, op := 0, 0
	if len(src) >= 5 && src[0] == 17 {
		if v := src[1]; v != 0 {
			return 0, fmt.Errorf("%w: %d", ErrBitstreamVersion, v)
		}
		ip = 2
	}
	// state: literals the previous instruction left: 0, 1..3, or 4 (four or more).
	state := 0
	if t := int(src[ip]); t > 17 {
		n := t - 17
		ip++
		if n > len(src)-ip {
			return 0, errTruncated
		}
		if n > len(dst) {
			return 0, ErrShortDst
		}
		copy(dst[:n], src[ip:ip+n])
		ip += n
		op = n
		state = min(n, 4)
	}

	for {
		if ip >= len(src) {
			return 0, errTruncated
		}
		t := int(src[ip])
		ip++

		var length, distance int
		switch {
		case t < 16:
			if state == 0 {
				// A literal run of 4 or more bytes.
				n := t
				if n == 0 {
					n = 15
					for {
						if ip >= len(src) {
							return 0, errTruncated
						}
						b := int(src[ip])
						ip++
						if b != 0 {
							n += b
							break
						}
						n += 255
						if n > len(dst)-op+3 {
							return 0, ErrShortDst
						}
					}
				}
				n += 3
				if n > len(src)-ip {
					return 0, errTruncated
				}
				if n > len(dst)-op {
					return 0, ErrShortDst
				}
				if n <= 16 && ip+16 <= len(src) && op+16 <= len(dst) {
					copy(dst[op:op+16], src[ip:ip+16])
				} else {
					copy(dst[op:op+n], src[ip:ip+n])
				}
				ip += n
				op += n
				state = 4
				continue
			}
			// With literals behind it the same opcode is a short match.
			if ip >= len(src) {
				return 0, errTruncated
			}
			h := int(src[ip])
			ip++
			if state < 4 {
				length, distance = 2, (h<<2)+(t>>2)+1
			} else {
				length, distance = 3, (h<<2)+(t>>2)+2049
			}
			state = t & 3

		case t < 32: // 0 0 0 1 H L L L
			n := t & 7
			if n == 0 {
				n = 7
				for {
					if ip >= len(src) {
						return 0, errTruncated
					}
					b := int(src[ip])
					ip++
					if b != 0 {
						n += b
						break
					}
					n += 255
					if n > len(dst)-op {
						return 0, ErrShortDst
					}
				}
			}
			length = n + 2
			if ip+2 > len(src) {
				return 0, errTruncated
			}
			v := int(src[ip]) | int(src[ip+1])<<8
			ip += 2
			distance = 16384 + ((t & 8) << 11) + (v >> 2)
			if distance == 16384 { // the end-of-stream instruction
				if ip != len(src) {
					return 0, fmt.Errorf("%w: %d bytes left after the end-of-stream marker", ErrCorrupt, len(src)-ip)
				}
				return op, nil
			}
			state = v & 3

		case t < 64: // 0 0 1 L L L L L
			n := t & 31
			if n == 0 {
				n = 31
				for {
					if ip >= len(src) {
						return 0, errTruncated
					}
					b := int(src[ip])
					ip++
					if b != 0 {
						n += b
						break
					}
					n += 255
					if n > len(dst)-op {
						return 0, ErrShortDst
					}
				}
			}
			length = n + 2
			if ip+2 > len(src) {
				return 0, errTruncated
			}
			v := int(src[ip]) | int(src[ip+1])<<8
			ip += 2
			distance = (v >> 2) + 1
			state = v & 3

		default: // 0 1 L D D D S S and 1 L L D D D S S
			if t < 128 {
				length = 3 + ((t >> 5) & 1)
			} else {
				length = 5 + ((t >> 5) & 3)
			}
			if ip >= len(src) {
				return 0, errTruncated
			}
			distance = (int(src[ip]) << 3) + ((t >> 2) & 7) + 1
			ip++
			state = t & 3
		}

		// The match: length bytes from distance bytes back in the output.
		if distance > op {
			return 0, fmt.Errorf("%w: match distance %d at output offset %d reaches before the start of the block", ErrCorrupt, distance, op)
		}
		if length > len(dst)-op {
			return 0, ErrShortDst
		}
		mpos := op - distance
		switch {
		case distance >= 16 && op+length+16 <= len(dst):
			// 16-byte moves, the last may run up to 15 bytes past the match
			// (inside dst); each reads bytes already written because the
			// distance is at least a move.
			for i := 0; i < length; i += 16 {
				copy(dst[op+i:op+i+16], dst[mpos+i:mpos+i+16])
			}
		case distance >= length:
			copy(dst[op:op+length], dst[mpos:mpos+length])
		default:
			// Overlapping: the source advances into bytes this copy is itself
			// writing (how LZO1X spells a run): lay the pattern down and double it.
			expanded := dst[mpos : op+length]
			for n := distance; n < len(expanded); n *= 2 {
				copy(expanded[n:], expanded[:n])
			}
		}
		op += length

		if state > 0 { // the 1..3 literals a match instruction carries
			if state > len(src)-ip {
				return 0, errTruncated
			}
			if state > len(dst)-op {
				return 0, ErrShortDst
			}
			if ip+4 <= len(src) && op+4 <= len(dst) {
				copy(dst[op:op+4], src[ip:ip+4])
			} else {
				copy(dst[op:op+state], src[ip:ip+state])
			}
			ip += state
			op += state
		}
	}
}

var errTruncated = fmt.Errorf("%w: input exhausted", ErrCorrupt)
