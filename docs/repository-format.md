# Repository format

This document specifies how Pentymento stores data, so that a repository can be read without Pentymento. Cryptographic constructions (slots, key objects, chunk and metadata encryption) are specified in [crypto.md](crypto.md).

## Layout

```text
config                                       format version and parameters (plaintext JSON)
keys/slot-<id>                               key slots (JSON; see crypto.md)
keys/private                                 the private key, sealed with the master key
keys/public                                  the public key, sealed with the metadata key
targets/<target-id>/info                     the target's name and engine
targets/<target-id>/checkpoints/<gen>-<seq>  capture checkpoints
events/<target-id>/<time>-<rand>             destructive events
heartbeat/agent-<host>/<time>                agent heartbeats
journal/<yyyy>/<mm>/<time>-<rand>            operations journal
```

Keys are "/"-separated segments of `A–Z a–z 0–9 . _ -`, never starting with "." (see `internal/backend`). `<time>` is Unix nanoseconds as 19 zero-padded digits, so keys sort by time; `<rand>` is 8 random hexadecimal characters; `<gen>` and `<seq>` are 20 zero-padded digits.

### Objects are written once

No object is ever rewritten. State that changes (checkpoints, heartbeats, verification results, pins) is recorded as a new object with a sortable name, and readers take the newest; prune removes superseded records. Every write except the config uses conditional create. This keeps the repository usable with write-only credentials and S3 Object Lock.

The one exception is key rotation, which rewrites each log segment's header (see crypto.md).

## Config

`config` is plaintext JSON and holds no secrets:

```json
{
  "version": 1,
  "id": "3f9c2a7d18e04b6f9a1c5d2e7b8f0a14",
  "encryption": "encrypted",
  "chunker": {"min": 262144, "avg": 1048576, "max": 4194304},
  "created": "2026-11-25T13:00:00Z"
}
```

| Field | Meaning |
| --- | --- |
| `version` | The repository format. A binary refuses a version newer than it understands; only `pentym repo upgrade` changes it. |
| `id` | 128 random bits, hexadecimal |
| `encryption` | `encrypted`, or `insecure-test-mode` for an unencrypted development repository |
| `chunker` | Chunk sizes in bytes |

`config` is written last when a repository is created, so a location without it is not a repository and an interrupted creation can be retried.

## Target IDs

Paths use an opaque 16-hex-character target ID: the first 8 bytes of keyed BLAKE3 with the metadata key as key, over `"pentymento target id v1" ‖ 0x00 ‖ target name`. The same name always gives the same ID in a repository; without the metadata key the ID reveals nothing about the name. In insecure test mode the hash is unkeyed.

## Metadata objects

Every object below is compact JSON, encrypted with the metadata key and bound to its own key (see crypto.md). Unknown fields are rejected.

| Object | Fields |
| --- | --- |
| `targets/<id>/info` | `name`, `engine` |
| Checkpoint | `target`, `generation` (the capture lease's), `seq`, `position` (engine-specific: LSN and timeline, or oplog timestamp), `time` |
| Event | `target`, `type`, `object`, `xid`, `lsn`, `oplog_ts`, `position`, `time` |
| Heartbeat | `host`, `agent_id`, `state` (`running` or `stopped`), `time` |
| Journal entry | `who`, `host`, `operation`, `target`, `run_id`, `credential` (the reference used, never a secret), `start`, `end`, `result` (`ok`, `failed` or `skipped`), `detail` (scrubbed of secrets) |

The newest checkpoint of a target is the last key under its `checkpoints/` prefix: the newest lease generation, then the highest sequence.

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
