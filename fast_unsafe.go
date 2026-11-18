//go:build (amd64 || arm64) && !purego

package lzo

import "unsafe"

// fastRun decodes instructions while src and dst are far from their ends and
// every instruction is one of the common short forms (a literal run of 4..16,
// a match of at most 33 bytes with at most 3 trailing literals). It stops
// before the first instruction that is anything else, or whose match would reach
// before the start of the block, leaving it for the exact loop in
// [DecodeBlock], so malformed input is refused there with the usual error.
//
// The wild moves may write up to 15 bytes past the logical end of a match or
// literal run; the margin keeps them inside dst, and the next instruction (or
// the exact loop) overwrites them.
//
// This is the unchecked version for the architectures whose unaligned loads
// and stores are native: the loop condition proves every access is inside src
// and dst (see fastSrcMargin, fastDstMargin), so the per-access bound checks
// are dropped. fast_generic.go is the same loop with ordinary indexing.
func fastRun(dst, src []byte, ip, op, state int) (int, int, int) {
	sp := unsafe.Pointer(unsafe.SliceData(src))
	dp := unsafe.Pointer(unsafe.SliceData(dst))
	srcEnd, dstEnd := len(src)-fastSrcMargin, len(dst)-fastDstMargin
	for ip <= srcEnd && op <= dstEnd {
		t := int(*(*byte)(unsafe.Add(sp, ip)))
		ip0, state0 := ip, state
		var length, distance int
		switch {
		case t >= 64: // 0 1 L D D D S S and 1 L L D D D S S: M2, two bytes
			length = 1 + t>>5 // 3..4 for 0 1 L, 5..8 for 1 L L: one formula, no branch
			distance = int(*(*byte)(unsafe.Add(sp, ip+1)))<<3 + (t>>2)&7 + 1
			ip += 2
			state = t & 3
		case t >= 32: // 0 0 1 L L L L L: M3, three bytes
			n := t & 31
			if n == 0 {
				return ip, op, state
			}
			v := int(*(*uint16)(unsafe.Add(sp, ip+1)))
			length = n + 2
			distance = v>>2 + 1
			ip += 3
			state = v & 3
		case t >= 16: // 0 0 0 1 H L L L: M4, three bytes
			n := t & 7
			if n == 0 {
				return ip, op, state
			}
			v := int(*(*uint16)(unsafe.Add(sp, ip+1)))
			distance = 16384 + (t&8)<<11 + v>>2
			if distance == 16384 { // end of stream: the exact loop checks it
				return ip, op, state
			}
			length = n + 2
			ip += 3
			state = v & 3
		case state == 0: // a literal run of 4 or more bytes
			if t == 0 || t > 13 {
				return ip, op, state
			}
			*(*[16]byte)(unsafe.Add(dp, op)) = *(*[16]byte)(unsafe.Add(sp, ip+1))
			ip += t + 4
			op += t + 3
			state = 4
			continue
		default: // a two-byte match after literals
			h := int(*(*byte)(unsafe.Add(sp, ip+1)))
			if state < 4 {
				length, distance = 2, h<<2+t>>2+1
			} else {
				length, distance = 3, h<<2+t>>2+2049
			}
			ip += 2
			state = t & 3
		}

		mpos := op - distance
		if mpos < 0 {
			// Reaches before the block: undo the header and let the exact
			// loop refuse it. Only reachable on corrupt input.
			return ip0, op, state0
		}
		d, m := unsafe.Add(dp, op), unsafe.Add(dp, mpos)
		switch {
		case distance >= 16:
			// Most matches are short: one unconditional move, the loop only
			// for the rest. Each move reads bytes already written, as the
			// distance is at least a move.
			*(*[16]byte)(d) = *(*[16]byte)(m)
			for i := 16; i < length; i += 16 {
				*(*[16]byte)(unsafe.Add(d, i)) = *(*[16]byte)(unsafe.Add(m, i))
			}
		case distance >= 8:
			*(*uint64)(d) = *(*uint64)(m)
			for i := 8; i < length; i += 8 {
				*(*uint64)(unsafe.Add(d, i)) = *(*uint64)(unsafe.Add(m, i))
			}
		default:
			for i := 0; i < length; i++ {
				*(*byte)(unsafe.Add(d, i)) = *(*byte)(unsafe.Add(m, i))
			}
		}
		op += length

		// The 0..3 trailing literals, moved as 4 bytes whatever their number
		// (no branch on state); only state of them count.
		*(*[4]byte)(unsafe.Add(dp, op)) = *(*[4]byte)(unsafe.Add(sp, ip))
		ip += state
		op += state
	}
	return ip, op, state
}
