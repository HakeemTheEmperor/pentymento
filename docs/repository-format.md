# Repository format

This document specifies how Pentymento stores data, so that a repository can be read without Pentymento. Cryptographic constructions (slots, key objects, chunk and metadata encryption) are specified in [crypto.md](crypto.md).

## Layout

```text
config                                       format version and parameters (plaintext JSON)
keys/slot-<id>                               key slots (JSON; see crypto.md)
keys/private                                 the private key, sealed with the master key
keys/public                                  the public key, sealed with the metadata key
snapshots/<id>                               a committed snapshot
index/<hash>                                 index files
data/<xx>/<hash>                             packs of encrypted chunks
wal/<target-id>/<timeline>/<segment>…        Postgres WAL segments
wal/<target-id>/<timeline>.history.zst.enc   Postgres timeline history files
oplog/<target-id>/<first>-<last>.zst.enc     MongoDB oplog batches
targets/<target-id>/info                     the target's name and engine
targets/<target-id>/checkpoints/<gen>-<seq>  capture checkpoints
events/<target-id>/<time>-<rand>             destructive events
heartbeat/agent-<host>/<time>                agent heartbeats
journal/<yyyy>/<mm>/<time>-<rand>            operations journal
locks/shared-<id>, locks/exclusive-<id>      repository locks
locks/lease-<target-id>/<generation>         capture leases
```

Keys are "/"-separated segments of `A–Z a–z 0–9 . _ -`, never starting with "." (see `internal/backend`). `<time>` is Unix nanoseconds as 19 zero-padded digits, so keys sort by time; `<rand>` is 8 random hexadecimal characters; `<gen>` and `<seq>` are 20 zero-padded digits.

### Objects are written once

No object is ever rewritten. State that changes (checkpoints, heartbeats, verification results, pins) is recorded as a new object with a sortable name, and readers take the newest; prune removes superseded records. Every write except the config uses conditional create. This keeps the repository usable with write-only credentials and S3 Object Lock.

Two exceptions:

- **Locks and leases** are created with conditional create, then refreshed and released by overwriting the same object (see "Locks and leases"). Overwriting needs only write permission, and under Object Lock it adds a version.
- **Key rotation** rewrites each log segment's header (see crypto.md).

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

## Packs

A pack holds encrypted chunks back to back, then a trailer that describes them, so the index can be rebuilt by reading the end of each pack:

```text
chunk 0 | chunk 1 | … | sealed trailer | trailer length (uint32, big-endian) | "PTMp" | version (1 byte, = 1)
```

- Each chunk is `seal(data key, compressed chunk, "pentymento chunk v1" ‖ 0x00 ‖ chunk ID)` (see crypto.md), with one random data key per pack.
- A pack is closed after the chunk that brings it to 16 MiB, so packs are 16 to 20 MiB.
- The trailer, before sealing, is: wrapped-key length (uint16) | wrapped data key | chunk count (uint32) | one 44-byte entry per chunk: chunk ID (32) | offset of the encrypted chunk in the pack (uint32) | its length (uint32) | the chunk's raw length (uint32). It is sealed with the metadata key, with associated data `"pentymento metadata v1" ‖ 0x00 ‖ "pack-trailer"`.
- A pack's key is `data/<first two hex characters>/<hash>`, where `<hash>` is the BLAKE3-256 hash of the pack's stored bytes, in hexadecimal. `check` verifies every pack against its name.

All integers are big-endian.

## Index

The index maps every chunk ID to its pack, offset and lengths, and every pack to its wrapped data key. It is the union of all index files.

An index file, before compression and sealing:

```text
"PTMi" | version (1 byte, = 1) | pack count (uint32) | for each pack:
  pack hash (32) | wrapped-key length (uint16) | wrapped data key | chunk count (uint32) | 44-byte entries as in the pack trailer
```

It is compressed like a chunk (header byte, then a zstd frame), sealed with the metadata key (associated data `"pentymento metadata v1" ‖ 0x00 ‖ "index"`), and stored at `index/<hash>`, where `<hash>` is the BLAKE3-256 hash of the stored bytes. A backup writes an index file every 4096 new packs and one at the end, before its snapshot. When several index files mention a chunk, any of their locations is valid.

## Snapshots

A snapshot's stream is a sequence of chunks. Its ordered list of chunk IDs is stored through the chunk store itself, so a snapshot of any size is a small object:

- The 32-byte IDs are grouped into **chunk-list blocks**. A block ends after an ID whose first two bytes, read as a big-endian number, are below 2, once the block holds at least 1024 IDs; or when it reaches 131,072 IDs. Since chunk IDs are keyed hashes, blocks average about 32,768 IDs (1 MiB), and unchanged runs of chunks give unchanged blocks, which deduplicate between snapshots.
- Each block is stored as an ordinary chunk.

`snapshots/<id>` is a metadata object (JSON, sealed with the metadata key), where `<id>` is 128 random bits in hexadecimal:

| Field | Meaning |
| --- | --- |
| `id` | The snapshot ID |
| `target` | The target ID |
| `engine`, `engine_version` | The database engine and its version |
| `start_position`, `end_position`, `timeline` | Where the base backup starts and ends in the engine's log (LSN and timeline, or oplog timestamps) |
| `inventory` | The engine's object inventory, recorded at backup time, for verification |
| `start`, `end` | When the backup ran |
| `size`, `chunks` | The stream's length in bytes and number of chunks |
| `list` | The chunk IDs of the chunk-list blocks, in order |

To read a snapshot: for each list block, read the block chunk; for each 32-byte ID in it, read that chunk; concatenate the chunks. Every chunk is checked against its ID after decryption and decompression.

### Commit order

A snapshot is committed only after its stream ends and the process that produced it exits cleanly: the last pack and its index file are written first, the snapshot object last, with conditional create. A failed or cancelled backup leaves only packs that no snapshot references, which a later prune removes once they are older than the safety margin.

## Log segments

Captured WAL and oplog are stored as individual objects, so recovery reads them in order and each can expire on its own. Each object is compressed as a whole (the chunk header byte, then one zstd frame; stored raw when that saves less than 3%) and then sealed with its own data key, wrapped to the public key and carried in the object's header (see "Sealed objects" in crypto.md), bound to its object key. A segment holds at most 64 MiB before compression.

| Object | Key |
| --- | --- |
| Complete WAL segment | `wal/<target-id>/<timeline>/<segment>.zst.enc` |
| Partial WAL segment | `wal/<target-id>/<timeline>/<segment>.partial-<end-lsn>.zst.enc` |
| Timeline history | `wal/<target-id>/<timeline>.history.zst.enc` |
| Oplog batch | `oplog/<target-id>/<first>-<last>.zst.enc` |

- `<timeline>` is the Postgres timeline as 8 uppercase hexadecimal digits, and `<segment>` the 24-character Postgres segment file name, such as `000000010000000000000003`.
- A partial segment holds WAL up to `<end-lsn>`, 16 uppercase hexadecimal digits, so successive partials of one segment are distinct objects; prune removes those a later partial or the complete segment supersedes.
- An oplog batch is named by its first and last entries' timestamps, each written `<seconds>.<increment>` with 10 zero-padded digits per part, so keys sort by time.
- Storing a segment that already exists succeeds: a resent segment after a crash is a harmless duplicate.

## Local metadata cache

Not part of the repository: pentym keeps a disposable copy of immutable objects (index files and snapshots) in the user's cache directory, under `pentym/<repository ID>/`, to save requests. Objects are cached exactly as stored, still encrypted. A cached index file is used only if it matches its hash name, and a cached snapshot only if it decrypts; anything else is fetched again. Deleting the cache is always safe.

## Locks and leases

Lock and lease records are plaintext JSON under `locks/`; they hold no secrets:

```json
{"version": 1, "kind": "lease", "id": "<target-id>", "generation": 3, "seq": 41, "state": "held",
 "holder": {"user": "pentym", "host": "db-1", "pid": 4211, "agent_id": "…"}, "time": "2026-11-26T12:00:00Z"}
```

`kind` is `shared`, `exclusive`, `lease` or `claim`; `state` is `held` or `released`; `time` is the writer's clock, for people only, and is never compared across hosts.

### Repository locks

| Lock | Object | Held by | Excludes |
| --- | --- | --- | --- |
| Shared | `locks/shared-<id>` | Snapshot backups | The exclusive lock |
| Exclusive | `locks/exclusive-<id>` | Prune's garbage collection, index rebuilds | Every other lock |

`<id>` is 16 random hexadecimal characters. A newcomer creates its record, then lists the others; if a conflicting live lock exists it releases its own and backs off (between two exclusive locks, the lower ID wins). Holders refresh their record every 5 minutes by overwriting it with a higher `seq`. A lock is **live** while `held` and modified within the last 30 minutes **by storage time**: its modification time is compared with that of a probe object written at the moment of checking, both set by the storage, so hosts' clocks never meet. Released and stale records are deleted by prune; a holder that can delete deletes its own record on release.

### Capture leases

The lease for a target's generation `g` is `locks/lease-<target-id>/<g>`, `g` written as 20 zero-padded digits.

- **Acquire:** create the next generation's object with conditional create, so exactly one candidate wins it: when no lease exists, when the newest record is `released`, or when the newest record has stayed unchanged (same key, `seq` and `state`) for 2 minutes **on the candidate's own monotonic clock**.
- **Renew:** every 30 seconds, overwrite the record with the next `seq`, after checking no newer generation exists.
- **Step down:** a holder that has not renewed successfully for 90 seconds stops capturing, before any candidate can take over at 2 minutes.
- **Release:** overwrite the record as `released`; a standby takes over at its next check (every 15 seconds). Lease records are never deleted on release: the newest generation must survive, so generations only increase. Prune may delete older generations' records.
- **Fencing:** checkpoints carry the writer's generation and are refused when a newer generation exists; because checkpoint keys sort by generation, a late write from an older holder would in any case sort below the new holder's.

Without conditional create, candidates write `locks/lease-<target-id>/<g>-claim-<id>`, wait 10 seconds, and the lowest claim wins and writes the generation record; such a lease is reported as best-effort.

