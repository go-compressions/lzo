#!/usr/bin/env python3
"""Builds crafted.json: LZO1X blocks and lzop streams that lzop(1) will not
produce on its own, each one validated by handing it to lzop -dc.

Some corners of the grammar and of the container are out of reach of lzop's
compressor: the state-4 short match (3 bytes from 2..3 KiB back), a header with
no filename, a pre-0x0940 header, and the compressed-data checksum fields. Those
still have to be decoded correctly, so they are written by hand here and then put
to an independent judge -- lzop itself must decompress each stream to exactly the
expected bytes, with lzop verifying the Adler-32 this script stores in the block
header. A case lzop refuses, or decodes to something else, is a case where this
script's reading of the format is wrong, and the whole run fails rather than
writing a fixture nobody checked.

Two constraints learned from lzop the hard way, both now respected here:

  * lzop refuses a block whose compressed length is not strictly less than its
    uncompressed length, because that is the stored case. Every crafted block
    therefore ends with cheap padding that inflates its output.
  * lzop asserts internally that F_ADLER32_D is set whenever F_ADLER32_C is, so
    the field-order question is settled with all four checksums present at once.

The small reference decoder below exists only to work out what each crafted block
should produce, so that the length and checksum in the container are right. It is
never shipped; lzop's agreement is what makes the expectation trustworthy.
"""

import json
import os
import struct
import subprocess
import sys
import zlib

MAGIC = bytes([0x89, 0x4C, 0x5A, 0x4F, 0x00, 0x0D, 0x0A, 0x1A, 0x0A])

F_ADLER32_D = 0x0001
F_ADLER32_C = 0x0002
F_CRC32_D = 0x0100
F_CRC32_C = 0x0200
F_H_FILTER = 0x0800
F_H_CRC32 = 0x1000
F_OS_UNIX = 0x03000000


# --------------------------------------------------------------------------
# Reference decoder, from the published bytestream description only.
# --------------------------------------------------------------------------
def decode(src):
    out = bytearray()
    ip = 0
    if len(src) >= 5 and src[0] == 17:
        assert src[1] == 0, "this script only crafts bitstream version 0"
        ip = 2
    state = 0
    if src[ip] > 17:
        n = src[ip] - 17
        ip += 1
        out += src[ip:ip + n]
        ip += n
        state = min(n, 4)

    def long_length(base):
        nonlocal ip
        n = base
        while src[ip] == 0:
            n += 255
            ip += 1
        n += src[ip]
        ip += 1
        return n

    while True:
        t = src[ip]
        ip += 1
        if t < 16:
            if state == 0:
                n = t if t else long_length(15)
                out += src[ip:ip + n + 3]
                ip += n + 3
                state = 4
                continue
            h = src[ip]
            ip += 1
            if state < 4:
                length, dist = 2, (h << 2) + (t >> 2) + 1
            else:
                length, dist = 3, (h << 2) + (t >> 2) + 2049
            state = t & 3
        elif t < 32:
            n = (t & 7) or long_length(7)
            length = n + 2
            v = src[ip] | (src[ip + 1] << 8)
            ip += 2
            dist = 16384 + ((t & 8) << 11) + (v >> 2)
            if dist == 16384:
                assert ip == len(src), f"{len(src) - ip} bytes past end of stream"
                return bytes(out)
            state = v & 3
        elif t < 64:
            n = (t & 31) or long_length(31)
            length = n + 2
            v = src[ip] | (src[ip + 1] << 8)
            ip += 2
            dist = (v >> 2) + 1
            state = v & 3
        else:
            length = 3 + ((t >> 5) & 1) if t < 128 else 5 + ((t >> 5) & 3)
            h = src[ip]
            ip += 1
            dist = (h << 3) + ((t >> 2) & 7) + 1
            state = t & 3
        assert dist <= len(out), f"distance {dist} at output offset {len(out)}"
        for _ in range(length):
            out.append(out[len(out) - dist])
        out += src[ip:ip + state]
        ip += state


# --------------------------------------------------------------------------
# Container builder.
# --------------------------------------------------------------------------
def build(block, plain, *, version=0x1040, lib=0x20A0, needed=0x0940, method=1,
          level=1, flags=F_OS_UNIX | F_ADLER32_D, name=b"crafted", mode=0o100644,
          mtime=0x0000000166B76ADF, filt=None):
    h = struct.pack(">HH", version, lib)
    if version >= 0x0940:
        h += struct.pack(">H", needed)
    h += bytes([method])
    if version >= 0x0940:
        h += bytes([level])
    h += struct.pack(">I", flags)
    if flags & F_H_FILTER:
        h += struct.pack(">I", filt)
    h += struct.pack(">II", mode, mtime & 0xFFFFFFFF)
    if version >= 0x0940:
        h += struct.pack(">I", mtime >> 32)
    h += bytes([len(name)]) + name
    sum_ = zlib.crc32(h) if flags & F_H_CRC32 else zlib.adler32(h, 1)
    out = bytearray(MAGIC + h + struct.pack(">I", sum_ & 0xFFFFFFFF))
    out += struct.pack(">II", len(plain), len(block))
    if flags & F_ADLER32_D:
        out += struct.pack(">I", zlib.adler32(plain, 1) & 0xFFFFFFFF)
    if flags & F_CRC32_D:
        out += struct.pack(">I", zlib.crc32(plain) & 0xFFFFFFFF)
    if flags & F_ADLER32_C:
        out += struct.pack(">I", zlib.adler32(block, 1) & 0xFFFFFFFF)
    if flags & F_CRC32_C:
        out += struct.pack(">I", zlib.crc32(block) & 0xFFFFFFFF)
    out += block
    out += b"\x00\x00\x00\x00"
    return bytes(out)


# --------------------------------------------------------------------------
# Instruction builders.
# --------------------------------------------------------------------------
EOS = b"\x11\x00\x00"


def continuation(base, total):
    """The zero-bytes-then-non-zero-byte encoding that adds total-base."""
    r = total - base
    assert r >= 1
    zeros = (r - 1) // 255
    return b"\x00" * zeros + bytes([r - 255 * zeros])


def lit_run(payload):
    """A state-0 literal-run instruction covering payload (4 bytes or more)."""
    n = len(payload) - 3
    assert n >= 1, "runs of 1..3 literals exist only in the first-byte encoding"
    if n <= 15:
        return bytes([n]) + payload
    return b"\x00" + continuation(15, n) + payload


def pad(length):
    """An M3 match of `length` bytes at distance 1: it inflates the output
    cheaply, so the block stays smaller than what it decodes to, and it leaves
    state at 0 so an end-of-stream marker may follow."""
    assert length >= 34, "shorter runs do not need the continuation encoding"
    return b"\x20" + continuation(31, length - 2) + b"\x00\x00"


PADLEN = 400

# Deterministic filler, so the long literal run below is distinctive.
rng = 0x12345678
filler = bytearray()
while len(filler) < 24000:
    rng = (rng * 1103515245 + 12345) & 0x7FFFFFFF
    filler.append(32 + (rng >> 11) % 95)
filler = bytes(filler)

cases = []


def case(label, block, note, **kw):
    plain = decode(block)
    assert len(block) < len(plain), \
        f"{label}: block {len(block)} >= plain {len(plain)}; lzop refuses that"
    cases.append({"name": label, "note": note, "block": block.hex(),
                  "plain": plain.hex(), "stream": build(block, plain, **kw).hex()})


# The state-4 short match: 3 bytes from 2049..3072 back, reachable only when the
# previous instruction copied four or more literals. lzop's compressors never
# emit it, so it is the one instruction class the real corpus misses.
blk = lit_run(filler[:3003])            # literal run, length via continuation
blk += bytes([0b0000_10_01, 0x00])      # DD=2 SS=1 -> 3 bytes from 2051 back
blk += b"Q"                             # the single trailing literal SS asks for
blk += bytes([0b0000_11_00, 0x10])      # state 1..3 form: 2 bytes from 68 back
blk += pad(PADLEN) + EOS
case("state4-short-match", blk,
     "0000DDSS after four or more literals: 3 bytes from 2..3 KiB back, then "
     "the same opcode in its state 1..3 form: 2 bytes from 1 KiB or less back")

# The first-byte literal shortcut, the only spelling of a 1..3 byte literal run,
# at each of its three short lengths, each followed by the short match that the
# resulting state enables.
for n, tail in ((1, bytes([0b0000_00_00, 0x00])),
                (2, bytes([0b0000_01_00, 0x00])),
                (3, bytes([0b0000_10_00, 0x00]))):
    blk = bytes([17 + n]) + b"ABC"[:n] + tail + pad(PADLEN) + EOS
    case(f"first-byte-{n}-literals", blk,
         f"opening byte {17 + n}: {n} literal(s) (state {n}), then the 2-byte "
         f"match that state enables")

# The first-byte shortcut with four or more literals, which sets state 4.
blk = bytes([17 + 6]) + b"ABCDEF" + bytes([0b0100_00_00, 0x00]) + pad(PADLEN) + EOS
case("first-byte-6-literals", blk,
     "opening byte 23: 6 literals (state 4), then a 3-byte M2 match")

# Every length the two short-distance instructions can spell.
blk = lit_run(b"ABCDEFGH")
for op in (0x9C, 0xBC, 0xDC, 0xFC):     # 1 LL DDD SS, DDD=7 -> distance 8
    blk += bytes([op, 0x00])
for op in (0x5C, 0x7C):                 # 0 1 L DDD SS, DDD=7 -> distance 8
    blk += bytes([op, 0x00])
blk += pad(PADLEN) + EOS
case("m2-all-lengths", blk,
     "128..255 with LL=0..3 (lengths 5..8) and 64..127 with L=0..1 (lengths "
     "3..4), all at distance 8")

# An end-of-stream instruction whose length field went through the continuation
# encoding: the distance is what declares the end, so the length has to be read
# and discarded first.
blk = lit_run(b"length is read before distance") + pad(PADLEN) + b"\x10\x01\x00\x00"
case("eos-with-length-continuation", blk,
     "0001HLLL with LLL=0: a length continuation byte precedes the LE16 "
     "distance that declares end of stream")

# Trailing literals on an M3 and an M4 instruction, where the count comes from
# the LE16 operand rather than from the opcode.
blk = lit_run(filler[:20000])
blk += bytes([0x22]) + struct.pack("<H", (98 << 2) | 2) + b"xy"
blk += bytes([0x13]) + struct.pack("<H", ((17000 - 16384) << 2) | 3) + b"pqr"
blk += pad(PADLEN) + EOS
case("m3-m4-trailing-literals", blk,
     "the 1..3 trailing literals of an M3 and an M4 instruction, whose count "
     "comes from the low two bits of the LE16 distance operand")

# --------------------------------------------------------------------------
# Crafted containers.
# --------------------------------------------------------------------------
simple = lit_run(b"container variations under test") + pad(PADLEN) + EOS

case("no-name", simple, "a header with a zero-length filename", name=b"")
case("old-version", simple,
     "version below 0x0940: no versionNeeded, no level and no mtime high word",
     version=0x0930)
case("all-checksums", simple,
     "all four optional block checksums at once, the only configuration that "
     "pins down the order of the fields: Adler-32 then CRC-32 of the "
     "uncompressed bytes, then Adler-32 then CRC-32 of the compressed bytes",
     flags=F_OS_UNIX | F_ADLER32_D | F_CRC32_D | F_ADLER32_C | F_CRC32_C)
case("crc32-header-and-data", simple,
     "a CRC-32 header checksum and a CRC-32 of the uncompressed bytes, with no "
     "Adler-32 anywhere",
     flags=F_OS_UNIX | F_CRC32_D | F_H_CRC32)
case("no-checksums", simple, "no block checksums at all", flags=F_OS_UNIX)
for m in (1, 2, 3):
    case(f"method-{m}", simple,
         f"method {m} decodes with the same LZO1X decoder as the others",
         method=m)

# A stored block: compressed length equal to uncompressed length.
storedBytes = bytes(range(256))
cases.append({"name": "stored-block",
              "note": "compressedSize == uncompressedSize: the payload is the "
                      "data itself and must never reach the LZO1X decoder",
              "block": storedBytes.hex(), "plain": storedBytes.hex(),
              "stream": build(storedBytes, storedBytes).hex(),
              "stored": True})

# --------------------------------------------------------------------------
# Put every case to the judge.
# --------------------------------------------------------------------------
failed = 0
for c in cases:
    stream = bytes.fromhex(c["stream"])
    want = bytes.fromhex(c["plain"])
    p = subprocess.run(["lzop", "-dc"], input=stream, capture_output=True)
    if p.returncode != 0:
        print(f"REJECTED by lzop: {c['name']}: {p.stderr.decode().strip()}",
              file=sys.stderr)
        failed += 1
    elif p.stdout != want:
        print(f"lzop DISAGREES: {c['name']}: got {len(p.stdout)} bytes, want "
              f"{len(want)}\n  got  {p.stdout[:64]!r}\n  want {want[:64]!r}",
              file=sys.stderr)
        failed += 1
    else:
        print(f"lzop agrees: {c['name']:<30} {len(want):6d} bytes from "
              f"{len(bytes.fromhex(c['block'])):6d}")

if failed:
    print(f"{failed} crafted case(s) not confirmed by lzop; crafted.json left "
          f"untouched", file=sys.stderr)
    sys.exit(1)

here = os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(here, "crafted.json"), "w") as f:
    json.dump(cases, f, indent=1)
    f.write("\n")
print(f"crafted.json: {len(cases)} cases, every one confirmed by lzop -dc")
