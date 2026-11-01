package lzo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"
)

// allocated reports how many bytes f allocated, by runtime.MemStats.
func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// lzopHeader returns a valid lzop file header with no block checksums, so that
// a block is just uncompressed length, compressed length and payload.
func lzopHeader(t testing.TB) []byte {
	t.Helper()
	s := base(t, "method-1")[:offBlock]
	binary.BigEndian.PutUint32(s[offFlags:], 0)
	return reseal(s)
}

func block(plain, comp uint32, payload []byte) []byte {
	var b [8]byte
	binary.BigEndian.PutUint32(b[:], plain)
	binary.BigEndian.PutUint32(b[4:], comp)
	return append(b[:], payload...)
}

var endBlock = []byte{0, 0, 0, 0}

// readAllCost decodes stream and reports the error and the bytes allocated.
func readAllCost(t *testing.T, stream []byte) (err error, cost uint64) {
	t.Helper()
	cost = allocated(func() {
		var z io.Reader
		if z, err = NewReader(bytes.NewReader(stream)); err != nil {
			return
		}
		_, err = io.Copy(io.Discard, z)
	})
	return err, cost
}

// A block header that claims 64 MiB and is followed by ten bytes used to cost
// two 64 MiB buffers before the missing payload was noticed.
func TestClaimedBlockSizeDoesNotAllocate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream []byte
		want   error
	}{
		{"stored block claiming 64 MiB, ten bytes follow",
			append(lzopHeader(t), block(maxBlockSize, maxBlockSize, make([]byte, 10))...), ErrHeader},
		{"compressed block claiming 64 MiB from 100 bytes",
			append(lzopHeader(t), block(maxBlockSize, 100, make([]byte, 100))...), ErrHeader},
		{"compressed block claiming 64 MiB from 1 byte",
			append(lzopHeader(t), block(maxBlockSize, 1, []byte{0})...), ErrHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err, cost := readAllCost(t, tc.stream)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if cost > 1<<20 {
				t.Errorf("allocated %d bytes, want < 1 MiB", cost)
			}
		})
	}
}

// The expansion bound is exact: plain = 255*comp is let through to the
// decoder (which then rejects this payload), 255*comp+1 never gets that far.
func TestBlockExpansionBoundIsExact(t *testing.T) {
	const comp = 100
	at := append(lzopHeader(t), block(255*comp, comp, make([]byte, comp))...)
	err, _ := readAllCost(t, at)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("at the bound: err = %v, want the decoder's ErrCorrupt", err)
	}
	over := append(lzopHeader(t), block(255*comp+1, comp, make([]byte, comp))...)
	err, _ = readAllCost(t, over)
	if !errors.Is(err, ErrHeader) {
		t.Fatalf("one over the bound: err = %v, want ErrHeader", err)
	}
}

// Large stored blocks cross the payload chunk size several times; a smaller
// block after them reuses the buffer. The bytes must come back intact.
func TestLargeStoredBlocksRoundTrip(t *testing.T) {
	big := make([]byte, 300<<10)
	for i := range big {
		big[i] = byte(i * 7)
	}
	small := big[:1000]
	exact := big[:payloadChunk] // exactly one chunk
	stream := lzopHeader(t)
	for _, p := range [][]byte{big, small, exact, big[:payloadChunk+1]} {
		stream = append(stream, block(uint32(len(p)), uint32(len(p)), p)...)
	}
	stream = append(stream, endBlock...)
	got, err := io.ReadAll(mustReader(t, stream))
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Join([][]byte{big, small, exact, big[:payloadChunk+1]}, nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d, first difference at %s", len(got), len(want), firstDifference(got, want))
	}
}

// A payload cut short after more than one chunk is reported, not hung or
// padded.
func TestTruncatedLargePayload(t *testing.T) {
	stream := append(lzopHeader(t), block(1<<20, 1<<20, make([]byte, 200<<10))...)
	err, cost := readAllCost(t, stream)
	if !errors.Is(err, ErrHeader) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want ErrHeader wrapping io.ErrUnexpectedEOF", err)
	}
	if cost > 2<<20 {
		t.Errorf("allocated %d bytes for 200 KiB delivered of 1 MiB claimed", cost)
	}
}

// Decompress takes its length from a container header; a hostile one used to
// be allocated as given.
func TestDecompressLengthBombs(t *testing.T) {
	src := cat(lit6, eos)
	for _, tc := range []struct {
		name string
		n    int
		want error
	}{
		{"one over the ceiling", DefaultMaxOutput + 1, ErrTooLarge},
		{"at the ceiling but unreachable from 9 bytes", DefaultMaxOutput, ErrCorrupt},
		{"one over 255x the input", MaxExpansion*len(src) + 1, ErrCorrupt},
		{"255x the input passes the bound and fails in the decoder", MaxExpansion * len(src), ErrCorrupt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			cost := allocated(func() { _, err = Decompress(src, tc.n) })
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.n > MaxExpansion*len(src) && cost > 64<<10 {
				t.Errorf("refusal allocated %d bytes", cost)
			}
		})
	}
}

// longRun builds a block that spells the biggest expansion the grammar has:
// six literals, then one M3 match whose length is extended by k zero bytes.
func longRun(k int) (block []byte, plainLen int) {
	block = append(block, 0x03, 'A', 'B', 'C', 'D', 'E', 'F') // 6 literals
	block = append(block, 0x20)                               // M3, length extended
	block = append(block, make([]byte, k)...)                 // k * 255
	block = append(block, 1)                                  // + 1
	block = append(block, 0x00, 0x00)                         // distance 1
	block = append(block, eos...)
	plainLen = 6 + (31 + 255*k + 1 + 2)
	return block, plainLen
}

// TestMaxExpansionIsReal decodes the densest block the grammar allows, to show
// both that it decodes and that its ratio stays under the bound.
func TestMaxExpansionIsReal(t *testing.T) {
	blk, n := longRun(4000)
	got, err := Decompress(blk, n)
	if err != nil {
		t.Fatalf("Decompress: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d bytes, want %d", len(got), n)
	}
	ratio := float64(len(got)) / float64(len(blk))
	t.Logf("%d input bytes -> %d output bytes, %.1fx", len(blk), len(got), ratio)
	if len(got) > MaxExpansion*len(blk) {
		t.Fatalf("ratio %.1f exceeds MaxExpansion %d", ratio, MaxExpansion)
	}
}

// FuzzExpansion checks the claim MaxExpansion rests on without going through
// the bound that relies on it: decode into an output far larger than the bound
// allows and assert that no accepted block comes out above it.
func FuzzExpansion(f *testing.F) {
	f.Add(cat(lit6, eos))
	blk, _ := longRun(10)
	f.Add(blk)
	f.Add([]byte{0x11, 0x00, 0x00})
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<12 {
			t.Skip()
		}
		var d decoder
		d.reset(src, make([]byte, (MaxExpansion+5)*len(src)+4096))
		if err := d.run(); err != nil {
			return
		}
		if d.op > MaxExpansion*len(src) {
			t.Fatalf("%d input bytes produced %d output bytes, over %dx", len(src), d.op, MaxExpansion)
		}
	})
}
