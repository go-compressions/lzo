package lzo

// Margins for fastRun. One instruction reads at most 17 bytes of src (a
// 16-byte literal move, or 3 header bytes and a 4-byte literal move) and writes
// at most 33+16 bytes of dst (the longest match it takes, 33, rounded up to
// whole 16-byte moves); keeping src and dst this far from their ends means no
// per-instruction bound check is needed.
const (
	fastSrcMargin = 32
	fastDstMargin = 64
)
