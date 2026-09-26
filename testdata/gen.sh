#!/bin/sh
# Regenerates the .lzo corpus in this directory with the real lzop(1).
#
# Every fixture's premise is asserted before it is kept: lzop -dc must read
# back byte-for-byte what lzop -c just wrote. A generator that reports success
# without doing the work is worse than no generator at all.
#
# Usage: sh testdata/gen.sh      (needs lzop and python3 on PATH)
set -e
cd "$(dirname "$0")"

command -v lzop >/dev/null || { echo "lzop not on PATH" >&2; exit 1; }
lzop --version | head -1

# --- sources ---------------------------------------------------------------

# text.txt: ~132 KiB of prose-like input with heavy repetition. Short
# distances, many M2/M3 matches, literal runs between them.
: > text.txt
i=0
while [ $i -lt 1200 ]; do
	printf 'the quick brown fox jumps over the lazy dog %04d\n' "$i"
	printf 'pack my box with five dozen liquor jugs, and then pack it again\n'
	i=$((i + 1))
done >> text.txt

# zeros.bin: 300 KiB of 0x00. Maximal match lengths, so the variable-length
# continuation bytes are exercised, and 300 KiB > 256 KiB (lzop's default block
# size) so lzop must emit more than one block.
dd if=/dev/zero of=zeros.bin bs=1024 count=300 2>/dev/null

# rand.bin: 32 KiB of incompressible bytes. LZO1X cannot shrink this, so lzop
# writes the block STORED (compressedSize == uncompressedSize).
dd if=/dev/urandom of=rand.bin bs=1024 count=32 2>/dev/null

# frag.bin: 40 KiB of semi-random words that LZO1X cannot shrink, followed by
# verbatim copies of slices whose only earlier occurrence is tens of KiB back.
# That forces the encoder onto the M4 instruction (distances 16..48 KiB), with
# both long lengths (4 KiB copies) and short ones (7-byte copies).
python3 - <<'PYGEN'
import random
random.seed(20260926)
words = ["".join(random.choice("abcdefghijklmnopqrstuvwxyz")
                 for _ in range(random.randint(3, 11)))
         for _ in range(4000)]
out = bytearray()
while len(out) < 40 * 1024:
    out += (" ".join(random.choice(words) for _ in range(12)) + "\n").encode()
base = bytes(out)
for off in (0, 6000, 13000, 21000):
    out += base[off:off + 4096]
for off in (1000, 9000, 17000, 25000, 33000):
    out += base[off:off + 6] + b"!"
open("frag.bin", "wb").write(bytes(out))
PYGEN

# mixedstore.bin: a compressible block followed by an incompressible one, so a
# STORED block appears inside a multi-block stream rather than as the whole of
# a single-block one.
{ dd if=/dev/zero bs=1024 count=250 2>/dev/null
  dd if=/dev/urandom bs=1024 count=40 2>/dev/null; } > mixedstore.bin

: > empty.bin
printf 'Z' > one.bin

# --- compress, asserting the premise each time -----------------------------

emit() { # emit <name> <source> [lzop flags...]
	name=$1
	src=$2
	shift 2
	lzop -f "$@" -c "$src" > "$name"
	lzop -dc "$name" > roundtrip.tmp
	if ! cmp -s roundtrip.tmp "$src"; then
		echo "PREMISE FAILED: lzop -dc did not reproduce $src for $name" >&2
		exit 1
	fi
	rm -f roundtrip.tmp
	printf '%-20s %8d -> %8d  flags:%s\n' "$name" \
		"$(wc -c < "$src")" "$(wc -c < "$name")" "${*:-none}"
}

emit text.lzo        text.txt
emit text-1.lzo      text.txt -1
emit text-9.lzo      text.txt -9
emit text-best.lzo   text.txt --best
emit text-crc32.lzo  text.txt --crc32
emit text-nock.lzo   text.txt --no-checksum
emit zeros.lzo       zeros.bin
emit rand.lzo        rand.bin
emit frag.lzo        frag.bin
emit mixedstore.lzo  mixedstore.bin
emit empty.lzo       empty.bin
emit one.lzo         one.bin

# A filtered stream. lzop's delta filters are a transform this package does not
# implement; the reader must say so rather than hand back wrong bytes. The
# premise asserted here is only that lzop itself produced and accepts it.
lzop -f --filter=1 -c text.txt > text-filter1.lzo
lzop -dc text-filter1.lzo > roundtrip.tmp
cmp -s roundtrip.tmp text.txt || { echo "PREMISE FAILED: text-filter1.lzo" >&2; exit 1; }
rm -f roundtrip.tmp
echo "text-filter1.lzo     written (expected to be REJECTED by this package)"

echo "all fixtures round-tripped through lzop -dc"

# The crafted corners lzop's compressor never emits, each one likewise put to
# lzop -dc before it is kept.
python3 "$(dirname "$0")/gen_crafted.py"
