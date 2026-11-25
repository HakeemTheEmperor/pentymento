# Repository format

This document specifies how Pentymento stores data, so that a repository can be read without Pentymento. It grows with the implementation; the sections below cover how a snapshot's stream is cut into chunks, how chunks are named, and how each chunk is compressed. The layout, packs, the index, encryption and log segments follow.

## Chunking

A snapshot's stream is split into content-defined chunks with FastCDC (Xia et al., "FastCDC: a Fast and Efficient Content-Defined Chunking Approach for Data Deduplication", USENIX ATC 2016), using normalized chunking at level 2.

| Parameter | Value |
| --- | --- |
| Minimum chunk size | 262,144 bytes (256 KiB) |
| Average chunk size | 1,048,576 bytes (1 MiB), so `bits` = 20 |
| Maximum chunk size | 4,194,304 bytes (4 MiB) |

### Gear table

The gear table is 256 unsigned 64-bit values derived from the repository's 32-byte chunk-ID key, so chunk boundaries cannot be predicted without the key:

```text
raw  = BLAKE3-derive-key(context = "pentymento chunker gear table v1",
                         key material = chunk-ID key, output length = 2048 bytes)
G[i] = little-endian uint64 of raw[8i : 8i+8], for i = 0..255
```

### Cut points

Two masks select the high bits of the 64-bit gear hash, which depend on the last 64 bytes:

```text
maskS = the top (bits + 2) = 22 bits set     // stricter, before the average size
maskL = the top (bits - 2) = 18 bits set     // looser, after it
```

To find the length of the next chunk in the remaining data `d` of length `n`:

```text
if n <= Min: the chunk is all of d
n      = min(n, Max)
normal = min(Avg, n)
fp     = 0
for i in Min-64 .. Min-1:  fp = (fp << 1) + G[d[i]]      // warm-up, no cut
for i in Min .. normal-1:  fp = (fp << 1) + G[d[i]]; if fp & maskS == 0: length = i + 1, stop
for i in normal .. n-1:    fp = (fp << 1) + G[d[i]]; if fp & maskL == 0: length = i + 1, stop
otherwise length = n
```

Arithmetic is modulo 2^64. Starting the hash 64 bytes before the minimum size makes every cut decision depend only on the 64 bytes before it, not on where the chunk began.

## Chunk IDs

A chunk's ID is the keyed BLAKE3 hash, 256 bits, of its raw content (before compression and encryption), using the repository's chunk-ID key as the BLAKE3 key. IDs are written as 64 lowercase hexadecimal characters. Without the key, equal IDs reveal nothing about content.

## Chunk compression

Each chunk is compressed on its own, so it can be read with a single range request. The stored plaintext of a chunk (before encryption) is a one-byte header followed by data:

| Header | Data |
| --- | --- |
| `0x00` | The raw chunk |
| `0x01` | One zstd frame (RFC 8878) holding the chunk |

A chunk is stored raw when zstd would save less than 3%: when the compressed frame is more than 97% of the raw size. Readers must reject any other header byte, a `0x01` header with no frame, and content that decodes to more than the maximum chunk size.
