# Pentymento — Software Design Document (v1)

Nov 24, 2026 · @Toluwalase Akinyemi

## 1. Overview

This document describes how Pentymento v1 is built: its components, how they interact, the data they store and the flows that run through them. It implements the [Pentymento PRD (v1)](https://claude.ai/artifact/F82YZzwaeTWprEJJ5VsjXP) and the 21 design decisions recorded there, and adds nothing outside them.

### 1.1 Scope

- **In scope:** one Go binary, `pentym`, that runs one-shot commands and a long-running agent; Postgres 16, 17 and 18 and MongoDB 7.0, 8.0 and 9.0 (replica sets); local filesystem and S3-compatible storage; Linux, macOS and Windows on amd64 and arm64.
- **Out of scope for v1:** MySQL, SQLite, sharded MongoDB, GCS and Azure, logical-decoding DML detection on Postgres, the optional DDL event trigger, pack compaction, `archive_command` mode, `pentym copy`, a control plane, UI, SSO, access roles, OpenTelemetry tracing, PITR on managed databases.

### 1.2 Design principles

1. **The repository is the single source of truth.** Every backup, log segment, event, checkpoint, lease, heartbeat and journal entry lives there.
2. **Restores never depend on the agent** (or any future control plane).
3. **Nothing correctness-critical goes only through the control socket.**
4. **The agent is an optimization over a correct one-shot core,** built from the same components.
5. **Never imply protection that doesn't exist:** the actual recovery window, gaps and stale agents are always reported.
6. **Agents can write but not read or delete backups.**
7. **Everything streams,** with memory constant regardless of database size.

### 1.3 Conventions

- **MUST / SHOULD / LATER** follow the PRD: required for v1, expected unless a documented reason prevents it, and deferred past v1.
- **Target:** one configured database (a Postgres cluster or a MongoDB replica set) with a name, an engine and credentials.
- **Snapshot:** a base backup stored in the chunk store. **Log segment:** one stored unit of WAL or one oplog batch.
- **Side instance:** a temporary database server used by restore and verify.
- Default values referenced here are those in PRD section 9.1; all are configurable.

## 2. Architecture overview

The CLI and the agent are thin entry points over one operations layer; below it, database-side components talk to the production database and side instances, storage-side components own the repository, and cross-cutting services support both.

&#91;embedded content: component architecture · entry points, core library, three component groups, external systems\]

How the parts interact:

- **CLI → operations layer:** every command runs in-process against the repository; the control socket to the agent is used only for live status, reload and backup-now, and is optional.
- **Agent → operations layer:** the agent runs the same operations (backup, verify, prune, check) on schedule, plus continuous capture while it holds a target's lease.
- **Database side ↔ production database:** replication connections for base backups and WAL, a tailable cursor for the oplog, and shelled-out dump and restore tools.
- **Database side → side instance:** instance providers start a temporary server; restored objects are extracted from it into production.
- **Storage side ↔ repository:** packs, log segments, index, events, checkpoints, leases, heartbeats and journal entries, through the backend interface.
- **Cross-cutting → outside:** the notifier delivers to webhook, Slack and heartbeat-ping channels; telemetry exposes metrics for Prometheus and Grafana.

## 3. Components

All logic lives in a Go core library; the CLI and the agent are thin callers of it, so a future `pentym server` can call the same library.

### 3.1 Entry points

| Component | Responsibility | Calls |
| --- | --- | --- |
| **CLI** (`cmd/pentym`) | Parses flags and config, resolves the target, invokes core operations, renders human output or `--json`, sets stable exit codes, shows progress, asks for confirmation on destructive operations when interactive | Operations layer; control-socket client (optional) |
| **Agent runtime** (`pentym agent`) | Holds the capture lease per target, runs continuous capture, runs the scheduler, writes heartbeats, serves the control socket and optional health port, handles graceful shutdown | Capture, Scheduler, Lease manager, Control socket, Operations layer |
| **Operations layer** | One implementation per operation: init, backup, catch-up, restore, events, list, status, verify, check, prune, pin, dump, history, key management, doctor, notify test, debug bundle. Labels each operation read, write or destructive | Everything below |

### 3.2 Database side

| Component | Responsibility |
| --- | --- |
| **Engine drivers** (Postgres, MongoDB) | Implement the engine interface (section 4.2): capability declaration, base-backup stream, log capture, event detection, restore into a side instance, object extraction, inventory and integrity checks |
| **Postgres capture** | Physical replication slot, native `BASE_BACKUP` and `START_REPLICATION PHYSICAL` via `pglogrepl`; segment and partial-segment handling; standby status updates; slot monitoring |
| **Postgres event detector** | Scans commit records and database-drop records; maintains the catalog timeline; classifies drop vs truncate or rewrite |
| **MongoDB capture** | Tailable cursor on `local.oplog.rs`; batching; rollover detection; oplog-window monitoring |
| **MongoDB event detector** | Detects drop, dropDatabase and renameCollection commands and bulk-delete bursts in oplog entries |
| **Tool runner** | Runs `pg_basebackup` (fallback), `pg_dump`, `pg_restore`, `mongodump` and `mongorestore` safely: no secrets in arguments, streamed stdout, process groups or job objects, captured stderr, exit-code checks, version checks, rejection of source compression flags |
| **Instance providers** (local binaries, Docker) | Start, stop and clean up side instances of the matching major version |

### 3.3 Storage side

| Component | Responsibility |
| --- | --- |
| **Chunker** | FastCDC content-defined chunking (256 KiB / 1 MiB / 4 MiB) |
| **Hasher** | Keyed BLAKE3 chunk IDs |
| **Compressor** | zstd per chunk and per log segment, with the skip-if-under-3% rule and a one-byte header |
| **Crypto** | Data-key generation, XChaCha20-Poly1305 encryption, X25519 key wrapping, metadata encryption, key slots |
| **Packer** | Groups encrypted chunks into \~16 MiB packs and records index entries |
| **Repository** | Owns the layout and formats: config and version check, snapshots, index, packs, log segments, events, journal, state, heartbeats, locks and leases; index rebuild |
| **Lock and lease manager** | Shared and exclusive repository locks; per-target capture leases with generation numbers |
| **Backends** (local, S3) | Implement the backend interface (section 4.1) |
| **Metadata cache** | Disposable local cache of index and snapshot lists, rebuilt from the repository |
| **Retention engine** | Applies GFS rules, PITR anchors, pins and safety rules; drives two-phase prune |

### 3.4 Cross-cutting

| Component | Responsibility |
| --- | --- |
| **Config loader** | YAML, includes, environment overrides, multiple named targets, secret references |
| **Secret providers** | `env:`, `file:`, `keychain:`, `cmd:`; the `Secret` type; resolve at use |
| **Scheduler** | Built-in schedules with overlap prevention, missed-run grace, jitter, concurrency limits, dead-man's switch |
| **Journal writer** | Appends operation records to the repository |
| **Notifier** | Routes events to webhook, Slack and heartbeat-ping channels via a local outbox |
| **Telemetry** | `slog` logs, Prometheus metrics, progress events, pprof (off by default) |
| **Control socket** | Local JSON-over-HTTP server (Unix socket or named pipe) for live status, reload, backup-now, health, readiness, metrics |
| **Service manager** | `agent install/uninstall/start/stop` via `kardianos/service` |

### 3.5 Source layout

Pentymento is one repository with one Go module, `github.com/HakeemTheEmperor/pentymento`, producing one binary. The CLI and agent cannot be split, the repository format and its readers must change together, and one version, changelog and release pipeline cover everything.

```
pentymento/
  cmd/pentym/            # main: CLI commands and `pentym agent`
  internal/
    ops/                 # operations layer (init, backup, restore, verify, prune, ...)
    agent/               # agent runtime: lease loops, capture loops, heartbeat
    engine/              # engine interface
      postgres/          # BASE_BACKUP, WAL capture, commit scanner, catalog timeline
      mongodb/           # mongodump runner, oplog tail, event detection
    instance/            # instance providers: local, docker
    toolrun/             # safe child-process runner
    chunker/             # FastCDC
    crypto/              # keys, slots, encryption, wrapping
    repo/                # layout, snapshots, index, packs, segments, journal, state
    lock/                # repository locks and capture leases
    backend/             # backend interface
      local/
      s3/
    retention/           # policy evaluation, two-phase prune
    scheduler/
    secrets/             # providers and the Secret type
    config/
    notify/
    telemetry/           # logging, metrics, progress
    control/             # control socket server and client
    service/             # agent install via kardianos/service
  docs/                  # architecture, repository-format, crypto, operations guide
  bench/                 # benchmark scripts, configs, raw results
  testdata/              # golden repositories, WAL corpus
  deploy/                # Kubernetes manifests, Grafana dashboard, Prometheus rules
  LICENSE  NOTICE  SECURITY.md  CONTRIBUTING.md  TRADEMARKS.md
```

- The core lives under `internal/` so its APIs can change freely; a package moves to an importable path only when there is a reason to publish it (for example a repository-format reader).
- A future `pentym server` (v2) starts as `cmd/pentym-server/` in this repository, calling the same core library.
- The package list is the initial layout for M0; package boundaries follow the components in sections 3.1–3.4.

## 4. Key interfaces

Four interfaces isolate what varies: where bytes are stored, which database is protected, how side instances run and where secrets come from.

### 4.1 Storage backend

Kept deliberately small; everything else (packs, index, locks, leases, segments) is built on these calls identically for every backend.

```go
type Backend interface {
    Put(ctx, key string, r io.Reader, opts PutOptions) error   // opts.IfNotExists
    Get(ctx, key string, rng *Range) (io.ReadCloser, error)     // range reads for chunks
    Stat(ctx, key string) (ObjectInfo, error)
    List(ctx, prefix string, fn func(ObjectInfo) error) error
    Delete(ctx, key string) error
    Capabilities() Capabilities   // ConditionalCreate, ObjectLock, StrongConsistency
}
```

| Backend | Conditional create | Notes |
| --- | --- | --- |
| Local filesystem | \`O\_CREAT | O\_EXCL\` |
| S3-compatible | `PUT` with `If-None-Match: *` | `aws-sdk-go-v2`, custom endpoints, multipart above \~16 MiB, backoff with jitter, bounded concurrency, parallel range reads |

`init` and `doctor` test conditional create by attempting the same create twice. Without it, the repository falls back to a write-wait-list protocol and leases are marked best-effort.

### 4.2 Engine

The engine interface is finalized at milestone M2 against the two real implementations. Each engine MUST provide:

| Responsibility | Postgres | MongoDB |
| --- | --- | --- |
| Declare capabilities (snapshots, PITR, capture method, extraction unit, supported versions) | Snapshots, PITR, physical slot, tables | Snapshots, PITR (replica sets only), oplog tail, collections |
| Check connection, version, role and privileges (for `doctor`) | Yes | Yes |
| Produce a base-backup stream | Native `BASE_BACKUP` tar stream; `pg_basebackup` fallback | `mongodump --oplog --archive` to stdout |
| Capture logs continuously and resume from a checkpoint | WAL from a physical slot, from the last confirmed LSN | Oplog tail from the last captured timestamp |
| Detect a capture gap | Slot invalidated or WAL removed | Oplog rolled past the checkpoint |
| Detect destructive events | Commit and database-drop records plus the catalog timeline | `op: "c"` commands and `op: "d"` bursts |
| Restore into a side instance up to a target | `recovery_target_time`, `recovery_target_xid` (non-inclusive), LSN | `mongorestore --oplogReplay --oplogLimit <ts>` |
| Extract objects into production | `pg_dump -t` then `pg_restore`, under a safe name | `mongodump --nsInclude` then `mongorestore --nsFrom/--nsTo` |
| Rewind (opt-in) | Restore the whole cluster to the target point | Restore the whole replica set to the target point |
| Record an inventory and verify contents | Catalog summary; `pg_amcheck`; `pg_last_xact_replay_timestamp()` and LSN | Collection summary; `validate`; last applied oplog timestamp |

### 4.3 Instance provider

Starts a side instance of a given engine and major version over a restore directory, returns connection details, and guarantees cleanup on success, failure and cancellation (crash leftovers are removed on the next run).

- **Local binaries:** `postgres` or `mongod` from the PATH or a configured path; version must match the backup.
- **Docker:** `postgres:<major>` or `mongo:<version>` with the restore directory mounted.
- Order comes from config, for example `instance_provider: [docker, local]`; `doctor` reports what is available.

### 4.4 Secret provider

Resolves a reference (`env:NAME`, `file:/path`, `keychain:service/item`, `cmd:<helper>`) to a `Secret` value at the moment of use, so rotated secrets take effect on the next connection. Native Vault and cloud secret managers are LATER implementations of the same interface.

## 5. Repository and data design

The repository is a hybrid: a deduplicated chunk store for snapshots and individual objects for log segments, with all target-specific data under opaque target IDs.

### 5.1 Layout

```
repo/
  config                         # format version, key parameters
  keys/                          # key slots (passphrase, age, agent)
  locks/                         # repository locks and per-target capture leases
  snapshots/<id>                 # snapshot = chunk refs + metadata
  index/<id>                     # chunk ID → pack + offset, wrapped data keys
  data/<xx>/<pack-id>            # ~16 MiB packs of encrypted chunks
  wal/<target-id>/<timeline>/<segment>.zst.enc
  events/<target-id>/...         # destructive-events timeline
  heartbeat/agent-<host>         # agent liveness
  targets/<target-id>/...        # per-target state: checkpoints, schedule state, verify results
  journal/...                    # append-only operations journal
```

- Target IDs in paths are opaque; friendly names are stored in encrypted metadata.
- All objects are written once and never modified; content-addressed objects with the same content get the same name, so duplicates are harmless.

### 5.2 Object types

| Object | Contents | Encrypted with | Written by |
| --- | --- | --- | --- |
| `config` | Repository format version, key parameters | — (no secrets) | `init` |
| Key slot | Repository private key wrapped by a passphrase (Argon2id) or age identity; agent slot holds metadata key, chunk-ID key and public key | Slot's own unlock method | `init`, `key` commands |
| Snapshot | Target, engine, version, start and end position (LSN or oplog timestamp), timeline, ordered chunk IDs, inventory, status (committed, verified), pins | Metadata key | `backup` |
| Index | Chunk ID → pack ID, offset, length; per-pack wrapped data key | Metadata key | `backup`, prune |
| Pack | Sequence of encrypted chunks | Per-pack data key | `backup` |
| Log segment | One WAL segment (or `.partial`) or one oplog batch, zstd-compressed | Per-object data key, wrapped to the public key | Agent, `catch-up` |
| Timeline history | Postgres timeline history files | Per-object data key | Agent |
| Event | Target, type, object name, xid or timestamp, position, time | Metadata key | Agent, `catch-up` |
| Checkpoint | Last confirmed position, lease generation | Metadata key | Agent, `catch-up` |
| Heartbeat | Host, agent ID, state (running, stopped), time | Metadata key | Agent |
| Lock / lease | Holder identity (user, host, process, agent ID), type, generation, refresh time | — (no secrets) | Any writer |
| Journal entry | Who, host, operation, target, credential reference used, start, end, result | Metadata key | Every operation |

### 5.3 Chunk format

Each stored chunk is a 24-byte random nonce followed by an XChaCha20-Poly1305 ciphertext under the pack's data key. The plaintext inside is a one-byte compression header (`none` or `zstd`) followed by the chunk bytes, so even whether a chunk compressed is hidden. Chunks are encrypted individually so a restore can fetch a single chunk with a range read.

Chunk IDs are keyed BLAKE3 hashes of the raw (uncompressed, unencrypted) content, so deduplication is independent of compression settings and reveals nothing to anyone without the chunk-ID key.

### 5.4 Log segment naming

- Postgres: `wal/<target-id>/<timeline>/<segment>.zst.enc`; partial segments carry their end position so successive partial uploads are distinct objects, and superseded partials are removed by prune once the full segment exists.
- MongoDB: one object per batch, named by its first and last oplog timestamps.

### 5.5 Index

- Loaded into memory, or looked up on disk when the repository is large.
- Cached locally (metadata cache) to reduce LIST and GET calls.
- **Rebuildable** by scanning packs (`check` and an index rebuild), because losing it would otherwise lose everything.
- Holds the wrapped data keys, so key rotation rewrites index files only, never packs.

### 5.6 Versioning and compatibility

- `config` records the repository format version; a binary MUST refuse to operate on a newer format than it understands, with a clear message.
- Golden repositories from each release are kept in `testdata/` and must remain readable.
- The format is published in `docs/repository-format.md`; `pentym dump <snapshot>` exports a plain file so data is never locked in.

**Upgrades and mixed versions**

- **Binary upgrades never change the format.** Upgrading is stop, replace binary, start; the agent resumes from its repository checkpoint.
- **Format changes are explicit:** `pentym repo upgrade` migrates a repository to a newer format. It is a destructive-labelled operation (confirmation when interactive), takes the exclusive repository lock, and is journaled. It is run only after every binary that uses the repository (agents, CLIs, CI jobs) has been updated, because older binaries refuse the newer format.
- **Mixed versions:** within one repository format, any combination of pentym versions must interoperate; golden-repository tests and compatibility tests enforce this.

## 6. Data flows

Six flows carry all of pentym's work: snapshot backup, continuous capture, event detection, restore, verify and prune.

### 6.1 Snapshot backup

&#91;embedded content: snapshot write pipeline · source stream to committed snapshot\]

Chunks already in the index are only referenced; new chunks are compressed, encrypted, packed and uploaded, and the snapshot is committed last.

1. **Prepare:** load config; resolve secrets; check the engine connection and tool versions; take a **shared** repository lock (refreshed every 5 min).
2. **Open the source stream:** Postgres native `BASE_BACKUP` over a replication connection (fallback `pg_basebackup`); MongoDB `mongodump --oplog --archive` to stdout. No compression at the source.
3. **Chunk** the stream with FastCDC (256 KiB / 1 MiB / 4 MiB).
4. **Hash** each chunk with keyed BLAKE3 and **check the index**; chunks already stored are only referenced.
5. For new chunks, **compress** with zstd (stored raw if savings < 3%) and **encrypt** with the pack's data key, in a worker pool bounded by the concurrency and I/O limits.
6. **Pack** into \~16 MiB packs; when a pack is full, wrap its data key to the public key, **upload** it, then record its index entries.
7. **Commit:** only after the stream ends and any child process exits cleanly, write the index files and then the snapshot object (with start and end positions and the inventory). A partial stream never becomes a snapshot; its orphaned packs are removed by a later prune.
8. **Finish:** release the lock, write the journal entry, emit metrics and notifications.

Memory stays constant: the stream is processed chunk by chunk and nothing is written to temporary files.

### 6.2 Continuous capture (agent)

**Postgres**

1. Acquire the target's capture lease (section 7) and read the checkpoint from the repository.
2. Open a replication connection and `START_REPLICATION PHYSICAL` on the slot from the last confirmed LSN.
3. Feed received WAL to the event detector (6.3) as it arrives.
4. Upload each completed segment as a compressed, encrypted object; upload the current partial segment every 10 s when new WAL exists.
5. After each confirmed upload, update the checkpoint (with the lease generation) and send a standby status update so the slot can free WAL (at least every 10 s).
6. Keep timeline history files while any snapshot or segment refers to that timeline.
7. Monitor retained slot WAL and capture lag against the alert thresholds.

**MongoDB**

1. Acquire the lease and read the checkpoint.
2. Open a tailable cursor on `local.oplog.rs` from the last captured timestamp. If that timestamp is older than the oldest oplog entry, record a **gap**, alert, and force a new base backup.
3. Feed entries to the event detector.
4. Flush a batch every 10 s, 16 MB or 10,000 entries, whichever comes first; update the checkpoint after each confirmed upload.
5. Monitor the oplog window against the thresholds.

**`catch-up`** runs the same steps once, without the scheduler: it acquires the lease (only if expired), drains what the slot or oplog still retains, records a checkpoint, reports any gap, and exits.

### 6.3 Event detection

**Postgres**

- The **catalog timeline** stores (oid, relname, relfilenode) snapshots from `pg_class`, refreshed every 30 s and at each base backup.
- For each **commit record**, read the list of relfilenodes the transaction drops; for each **database-drop record**, record a database drop.
- Map relfilenodes to names using the timeline entry valid at that moment. **oid gone → DROP**; **oid present with a new relfilenode → TRUNCATE or rewrite** (`VACUUM FULL`, `CLUSTER`, some `ALTER TABLE`). Only DROP TABLE, DROP DATABASE and TRUNCATE are reported; rewrites are not. TOAST tables and indexes are attributed to their parent table.
- A relfilenode not in the timeline is reported as an unknown table created after the last refresh.

**MongoDB**

- `op: "c"` entries with `drop`, `dropDatabase` or `renameCollection` become events.
- A burst of `op: "d"` entries on one collection becomes a **bulk delete** event.

**Both:** events are written to `events/<target-id>/` with their exact position (xid and LSN, or oplog timestamp), sent to the notifier as critical, and listed by `pentym events`, numbered for `--before-event`.

### 6.4 Restore and extract

&#91;embedded content: restore flow · side instance, extract by default, rewind opt-in\]

A restore never needs the agent: it reads only the repository, rebuilds a side instance, and by default extracts just the affected tables or collections.

1. **Resolve the target point** from `--at`, `--xid` (Postgres), LSN or `--before-event` (stops just before that transaction or oplog entry).
2. **Select** the anchor snapshot taken before the point and the log segments from its start to the point; report a gap if any are missing.
3. **Unlock** the private key from a full key slot; unwrap data keys.
4. **Start a side instance** through an instance provider (matching major version) and materialize the snapshot into it with parallel range reads.
5. **Replay** logs to the point: Postgres with `recovery_target_time` or `recovery_target_xid` and `recovery_target_inclusive = false`; MongoDB with `mongorestore --oplogReplay --oplogLimit <ts>`.
6. **Extract** only the affected objects: `pg_dump -t` then `pg_restore`, or `mongodump --nsInclude` then `mongorestore --nsFrom/--nsTo`, into production under a safe name such as `orders_recovered_20261122`, or in place with `--in-place`.
7. **Clean up** the side instance and temporary files; journal the result.

`--mode=rewind` instead restores the whole cluster to the point. It is destructive and requires confirmation when interactive.

### 6.5 Verify

| Level | Steps |
| --- | --- |
| L0 | Check referenced objects exist, chunk tags verify, index is consistent, a key slot unlocks (sampled in `quick`) |
| L1 | Decrypt and reassemble the snapshot stream; validate the tar or archive |
| L2 | Restore into a side instance (as 6.4 steps 2–5, without extraction); confirm it reaches consistency and accepts queries |
| L3 | Compare the reached point with the repository: `pg_last_xact_replay_timestamp()` and last applied LSN, or last applied oplog timestamp; optional canary value |
| L4 | Compare against the inventory recorded at backup time within tolerance; run `pg_amcheck` or `validate` (sampled or full) |
| L5 | Repeat L2–L3 at a random point inside the PITR window |

Presets: `quick` (L0 sampled, L1), `standard` (L0–L3, L4 sampled), `deep` (all). A pass marks the snapshot **verified**; a failure is a critical notification. Time limit 6 h; cleanup guaranteed.

### 6.6 Prune

1. **Plan:** apply the retention policy (section 7.4) to list snapshots to forget and the anchor per target. Interactive runs show this as a dry run and ask for confirmation.
2. **Phase 1 — forget (no chunk-store lock):** delete expired snapshot records; delete log segments older than each anchor's start position, keeping timeline history files still referenced and superseded `.partial` objects only once replaced.
3. **Phase 2 — garbage collection (exclusive lock):** mark every chunk referenced by a remaining snapshot; delete packs with no referenced chunk and older than the safety margin (the larger of 24 h and 2× the longest backup run); then rewrite the index without them; release the lock.
4. **Journal** what was deleted and the bytes freed.

A crash at any point leaves only unreferenced leftovers for the next prune; surviving index entries always point to existing packs.

## 7. Coordination and retention

Three mechanisms keep concurrent writers safe without any coordinator outside the repository: repository locks, per-target capture leases, and lease generations.

### 7.1 Repository locks

| Lock | Held by | Excludes |
| --- | --- | --- |
| Shared | Snapshot backups, while they run | Prune garbage collection |
| Exclusive | Prune phase 2 (garbage collection), index rebuild | All shared and exclusive holders |
| None | Log-segment uploads, prune phase 1 | — (segments are independent objects) |

- A lock is an object in `locks/` created with conditional create, carrying the holder's identity (user, host, process, agent ID).
- Holders refresh locks every 5 min; a lock not refreshed for 30 min is stale and may be removed, so a crashed process cannot block prune forever.
- Continuous capture never takes the chunk-store lock, so prune never has to stop capture.
- The scheduler runs prune after backup, so shared and exclusive locks rarely collide; a run that cannot get its lock is skipped and journaled.

### 7.2 Capture leases

&#91;embedded content: capture lease lifecycle · holder and standby agents\]

The 30-second gap between the holder stepping down (90 s) and a standby taking over (2 min) guarantees that two agents never capture at once; lease generations fence any late checkpoint write.

- **One active capturer per target.** The lease is an object in `locks/` holding the holder's identity, a **generation number** and its last renewal.
- **Acquire:** conditional create when no lease exists; when one exists, a candidate takes it only after observing it **unchanged for the full lease length (2 min) on its own monotonic clock**, then writes a new lease with generation + 1. Wall clocks are never compared across hosts.
- **Renew** every 30 s. Standbys check every 15 s.
- **Step down:** a holder that cannot renew for 90 s stops capturing before any standby can take over, so two agents never write at once.
- **Fencing:** checkpoint writes carry the lease generation; a write with an older generation than the current lease is rejected, so an agent that paused and woke late cannot overwrite the new holder's checkpoint.
- **Graceful release:** on shutdown the holder writes a final `state: stopped` heartbeat and deletes its lease, so a standby takes over immediately.
- **Liveness is read from the lease and heartbeat in the repository**, never from the control socket.
- Without conditional create on the backend, acquisition uses the write-wait-list fallback and the lease is reported as best-effort.

### 7.3 Who may capture

| Actor | May capture when |
| --- | --- |
| Agent (lease holder) | It holds the current lease |
| Standby agent (e.g. second Kubernetes replica) | The lease expired or was released |
| `catch-up` | The lease expired or was released; it releases the lease on exit |
| `status` | Never by default; only with `catch_up.auto: true` and an expired lease |

### 7.4 Retention policy evaluation

- **Snapshots** follow grandfather-father-son rules (`keep_last`, `keep_daily`, `keep_weekly`, `keep_monthly`, `keep_yearly`, `keep_within`); a snapshot is kept if any rule keeps it. Pinned snapshots are always kept.
- **Anchor:** the newest snapshot taken before the start of `pitr_window` is always kept; log segments older than the anchor's start position may be deleted.
- **Safety:** never delete the newest successful snapshot, nor the newest verified one when verify is enabled; if the last 3 backups failed, refuse to delete older good snapshots and alert.
- **Events** are kept for `events` duration and at least as long as the logs they point to; **journal** entries for `journal` duration, with old entries summarised in batches.
- **Reported window:** the actual PITR window is computed from the anchor and the contiguous segments after it, so gaps shorten it and `status` shows both configured and actual values.

## 8. Security design

The design goal is that a compromised database host can neither read nor delete backup history: the agent holds only a public key and write-only storage credentials.

### 8.1 Keys

| Key | Type | Held in | Used for |
| --- | --- | --- | --- |
| Repository key pair | X25519 | Public: agent slot and full slots. Private: full slots only | Wrapping and unwrapping per-pack and per-segment data keys |
| Metadata key | 256-bit symmetric | Agent slot and full slots | Snapshots, index, events, checkpoints, heartbeats, journal |
| Chunk-ID key | 256-bit symmetric | Agent slot and full slots | Keyed BLAKE3 chunk IDs |
| Data key | Random, one per pack or log-segment object | Never stored unwrapped | XChaCha20-Poly1305 encryption of chunks and segments |

### 8.2 Write path (agent, write-only)

1. Generate a fresh data key for each pack and each log-segment object.
2. Encrypt each chunk separately with XChaCha20-Poly1305 and a random 24-byte nonce.
3. Wrap the data key to the repository public key (X25519, HKDF, then an AEAD — the construction `age` uses); store the wrapped key in the index entry (packs) or alongside the object (segments).
4. Discard the data key. Deduplication works because the agent compares keyed chunk IDs against the index, which it can decrypt with the metadata key, without decrypting any chunk.

### 8.3 Read path (restore, verify, prune)

Unlock the private key from a full slot (passphrase via Argon2id, or an age identity), unwrap data keys, decrypt chunks and segments.

### 8.4 Key slots and lifecycle

- `init` creates the key pair, the metadata and chunk-ID keys, at least two full slots (or one slot plus a printed paper key via `key export --paper`) and an agent slot.
- `key add`, `key list`, `key remove` manage slots; removing a slot stops that unlock method.
- `key rotate` generates a new key pair and re-wraps every data key in the index files (and segment keys); packs are never rewritten.
- The agent MUST refuse to start if it can unlock the private key while `agent.write_only: true` (the default).
- `verify` and `check` confirm at least one slot unlocks; `status` warns when only one full slot exists.
- Cloud KMS and Vault slots are LATER.

### 8.5 Credentials

| Secret | Agent | One-shot backup | Restore / extract | Prune / admin |
| --- | --- | --- | --- | --- |
| DB backup credential | Yes | Yes | — | — |
| DB replication credential | Yes | `catch-up` only | — | — |
| DB restore credential | — | — | Yes | — |
| Storage, write-only | Yes | Yes | — | — |
| Storage, full | — | — | Read | Yes |
| Agent key slot | Yes | Yes | — | — |
| Full key slot | Never | — | Yes | Yes |

- Config holds only **references** (`env:`, `file:`, `keychain:`, `cmd:`); plaintext requires `allow_plaintext_secrets: true` and a file not readable by group or others.
- Prefer credential-free mechanisms: peer authentication over a Unix socket, client TLS certificates, `.pgpass` or service files, MongoDB x.509, the AWS default credential chain (instance roles, IRSA, OIDC).
- Minimal roles are generated by `init --print-sql` / `--print-mongo`: Postgres `pentym_backup` and `pentym_repl` with `REPLICATION` (not superuser) and catalog read access, and `pentym_restore` with create rights only in the target schema; MongoDB built-in `backup` plus read on `local`, and `restore` for extraction. `doctor` flags too much privilege as well as too little.
- The `Secret` type returns `[redacted]` from `String()`, `Format`, `MarshalJSON` and `slog.LogValuer`; driver errors are wrapped and scrubbed; secrets are resolved per connection.
- Child processes get credentials through temporary files with restrictive permissions (0600, or the service account's ACL on Windows), deleted afterwards and cleaned up after crashes; `PGPASSWORD` in the child environment only as a fallback. Never in arguments.
- The journal records the credential **reference** used, never the value.

### 8.6 Threat model summary

| Threat | Mitigation |
| --- | --- |
| Database host compromised | Agent has no private key (cannot read history) and write-only storage credentials (cannot delete); Object Lock or append-only mode supported |
| Storage bucket leaked | All data and metadata encrypted; chunk IDs keyed |
| Secret leakage via logs, process list or alerts | Secret references, `Secret` type, scrubbed errors, no secrets in arguments, metadata-only alerts, secret-leak test in CI |
| Forged webhook calls | HMAC signature header on webhook payloads |
| Stale agent writing after failover | Lease generations fence checkpoint writes |
| Network exposure of control functions | Control socket is local-only with file permissions; the optional health port exposes only `/health`, `/ready`, `/metrics` |

**Not hidden** (documented in `docs/crypto.md`): object sizes, counts and timing; chunk equality to holders of the chunk-ID key; compressed sizes reveal a little about content. Go's garbage collector cannot guarantee secrets are wiped from memory; buffers under our control are cleared.

**Libraries:** `golang.org/x/crypto` (chacha20poly1305, argon2, hkdf, curve25519), `filippo.io/age`, `zeebo/blake3`. No home-made primitives. Encryption is always on except `--insecure-test-mode`, which refuses non-local backends.

## 9. Agent runtime

The agent is one long-running process per host that runs, for each configured target, a capture loop, a lease loop and the scheduler, all cancelled through one root context.

### 9.1 Structure

| Loop | Per | Does |
| --- | --- | --- |
| Lease loop | Target | Acquires, renews (30 s) or watches (15 s) the capture lease; starts or stops the capture loop accordingly; steps down after 90 s without renewal |
| Capture loop | Target, only while holding the lease | Streams WAL or oplog, feeds the event detector, uploads segments, updates checkpoints |
| Heartbeat | Agent | Writes `heartbeat/agent-<host>` every 30 s |
| Scheduler | Agent | Runs due jobs for targets whose lease this agent holds |
| Upload workers | Agent | Bounded pool shared by compression, encryption and uploads |
| Notifier outbox | Agent | Delivers queued notifications with retries |
| Control socket server | Agent | Serves local requests |

### 9.2 Scheduler

- Schedules come from config per target (`daily 02:00`, `every 6h`, `after backup`, or 5-field cron, parsed with a library such as `robfig/cron/v3`'s parser) in the target's configured time zone. Jobs: `backup`, `verify`, `prune`, `check`.
- The loop is our own (about 200 lines) and takes an injectable clock for tests.
- **Overlap:** if a job is still running when it is due again, the new run is skipped and journaled.
- **Missed runs:** after downtime a missed run executes once if within the grace window (the smaller of 6 h and half the interval); otherwise it waits for the next slot.
- **Jitter:** a fixed per-target offset, the smaller of 5 min and 10% of the interval.
- **Concurrency:** at most 1 heavy job per agent by default, plus optional I/O throttling.
- **State** (last run, last success) is stored under `targets/<target-id>/` in the repository, and only the lease holder runs a target's schedules, so failover neither duplicates nor drops runs.
- **Dead-man's switch:** if no successful backup exists within 1.5× the interval (at least interval + 1 h), raise a critical alert. `status` and any command reading the repository apply the same check, which covers cron users.

### 9.3 Control socket

- Unix socket (Linux, macOS) or named pipe (Windows), protected by file permissions, never exposed to the network; JSON over HTTP so it can be debugged with `curl --unix-socket`.
- Serves: live status (replication lag, slot WAL retention, upload queue depth), config reload without dropping replication connections, backup-now through the running agent's connection and lock, health, readiness, Prometheus metrics, and pprof when `agent.debug.pprof: true`.
- Clients fall back to the repository when the socket is unreachable and say so ("agent not reachable; showing last heartbeat from 14:02:11").
- An opt-in TCP health port (off by default, bound to localhost or the pod IP) serves only `/health`, `/ready` and `/metrics`.

### 9.4 Startup

1. Load config and check the repository format version (refuse newer formats).
2. Unlock the agent slot; refuse to start if the private key is unlockable while `agent.write_only: true`.
3. Clean up leftovers from a previous crash (temporary credential files, temp directories).
4. Start the control socket, heartbeat, lease loops and scheduler.
5. On acquiring a lease, read the checkpoint and resume capture from the last confirmed position; if the position is no longer available, record the gap, alert and force a base backup.

### 9.5 Graceful shutdown (SIGTERM or service stop)

1. Stop starting new jobs.
2. Abandon any uncommitted snapshot (never committed, so nothing is half-written).
3. Flush capture: upload the partial WAL segment as `.partial` and send the final standby status update, or upload the current oplog batch and record its last timestamp.
4. Write a final heartbeat with `state: stopped` and release the lease.
5. Exit within the 30 s deadline regardless (service managers allow 45 s).

### 9.6 Crash recovery (`kill -9`, power loss)

- Restart resumes from the repository checkpoint. Postgres resends WAL after the last acknowledged position; MongoDB resumes from the stored timestamp.
- Re-uploaded segments are identical objects, so duplicates are harmless.
- If the agent stays down, the lease expires after 2 min and a standby or `catch-up` takes over.

### 9.7 Service integration

| Platform | Mechanism | Runs as | Config |
| --- | --- | --- | --- |
| Linux | systemd, `Restart=on-failure`, `TimeoutStopSec=45` | `pentym` user | `/etc/pentym/config.yaml` |
| macOS | launchd, `KeepAlive` | `_pentym` user | `/usr/local/etc/pentym/` |
| Windows | Windows service, automatic start, restart on failure | `NT SERVICE\pentym` | `%ProgramData%\pentym\` |
| Kubernetes | Deployment per target, 2 replicas, `terminationGracePeriodSeconds: 45` | Non-root container | ConfigMap and Secrets |

Installed with `pentym agent install` via `kardianos/service`; `--print` emits the unit files instead. Upgrades are stop, replace binary, start.

## 10. Observability and notifications

Every operation produces three records: structured log lines for debugging, metrics for monitoring, and a journal entry in the repository for history and audit. Conditions that need attention become routed notifications.

### 10.1 Logs

- `slog`; JSON on stdout for services, text in a terminal; `--log-format` overrides.
- Every line carries `target`, `job`, `run_id`, `component`; child-process stderr is captured line by line with the same `run_id`.
- `info` for lifecycle events, `debug` for detail; secrets redacted by the `Secret` type.
- Windows: warnings and errors also go to the Event Log. Optional rotating `log.file`.

### 10.2 Metrics

Served at `/metrics` (control socket and opt-in health port). Labels are per target and job, never per table.

| Metric | Source |
| --- | --- |
| `pentym_capture_lag_seconds{target}` | Capture loop |
| `pentym_pg_slot_retained_wal_bytes{target}` | Postgres capture |
| `pentym_mongo_oplog_window_seconds{target}` | MongoDB capture |
| `pentym_pitr_window_seconds{target}` | Retention engine (actual window) |
| `pentym_last_success_timestamp_seconds{target,job}` | Operations layer |
| `pentym_job_duration_seconds{job}` (histogram) | Operations layer |
| `pentym_job_failures_total{target,job}` | Operations layer |
| `pentym_repo_bytes{kind}`, `pentym_dedup_ratio` | Repository |
| `pentym_backend_request_duration_seconds{op}` and errors | Backends |
| `pentym_upload_queue_depth` | Upload workers |
| `pentym_events_detected_total{type}` | Event detectors |
| `pentym_lease_held{target}` | Lease loop |
| `pentym_verify_last_result{target,level}` | Verify |

The repository ships a Grafana dashboard and example Prometheus alert rules mirroring the critical notifications.

### 10.3 Journal, progress and debugging

- **Journal:** one entry per operation (backup, capture start and stop, verify, prune, restore, notification attempts, failover), including skips and reasons; read with `pentym history <target> --since 30d`.
- **Progress:** stderr progress bars in a terminal (bytes, rate, time remaining, dedupe so far); a JSON event stream with `--json`.
- **`pentym debug bundle`:** version info, `doctor` output, redacted config, recent logs, last journal entries.
- **Tracing:** OpenTelemetry is LATER; logging and metrics sit behind a small internal interface so spans can be added.

### 10.4 Notification pipeline

1. A component raises a **condition** (target, type, severity, details, suggested command).
2. The **condition tracker** applies the hold (2 min before alerting, 5 min clear before resolving) and groups repeats for 1 h. Destructive events and verify failures bypass the hold.
3. **Routing** matches target, minimum severity and event type to named channels.
4. The message is written to the local **outbox** and delivered with retries and backoff; delivery never blocks or fails the work that raised it.
5. Each attempt and result is **journaled**.

| Channel | Payload |
| --- | --- |
| Webhook | Versioned JSON schema, HMAC signature in `X-Pentym-Signature` |
| Slack | Formatted message via incoming webhook |
| Heartbeat ping | GET to a URL after each successful job (healthchecks.io, Uptime Kuma, Better Stack) |

| Severity | Conditions |
| --- | --- |
| Critical | Destructive event; verify failed; capture gap; dead-man's switch; slot retention or capture lag past critical; `check` found corruption |
| Warning | Single backup failed; agent failover; slot retention or capture lag past warning; oplog window low; prune refused to delete the last good snapshot; only one key slot |
| Info (off by default) | Backup succeeded (can be enabled); verify passed; prune summary |

Messages carry metadata only (object names, times, sizes, the next command); `notify.redact_object_names: true` hides names. Webhook URLs and HMAC secrets are secret references. `pentym notify test` sends a sample through the same pipeline.

## 11. Deployment

Nothing has to run on the database host: replication and oplog tailing both work over the network, and restores run wherever there is disk and compute.

| Setup | Agent runs on | Database connection | Notes |
| --- | --- | --- | --- |
| All on one host (solo VPS) | The database host | Peer authentication over a Unix socket (no password) | I/O throttling on by default; uploaded data survives the host's loss |
| Dedicated backup host (small teams) | A backup VM running the agents for several targets | TLS over the network; `pg_hba.conf` and MongoDB access rules allow the backup host | Backup load never touches database hosts |
| Kubernetes | One Deployment per target, 2 replicas | Through the database Service | Replicas compete for the lease; the standby takes over on failure |

- `pentym init` asks which setup applies: same host → peer or socket authentication and throttling; separate host → require TLS and print the access-rule lines needed.
- **Restore and verify** run on an admin machine, a restore host or a CI runner with disk about the size of the database and Docker or matching binaries.
- **Read-only commands** (`list`, `status`, `events`, `history`) need only storage access and a key slot, and run anywhere.
- **CI:** secrets arrive as environment variables (`env:` references); cloud storage via OIDC through the default credential chain; backup-only jobs get the agent slot, verify and restore jobs get a full slot behind a protected environment with required approval.

## 12. Failure modes

Each failure below either leaves the repository consistent and reports itself, or is prevented outright; none may silently lose data or produce a false "verified".

| Failure | Detection | Handling |
| --- | --- | --- |
| Agent killed mid-upload | Missing checkpoint advance | Restart resumes from the last confirmed position; re-uploads are identical objects |
| Agent down for long | Stale heartbeat (5 / 15 min); lease expiry | Standby or `catch-up` drains retained WAL or oplog; capture-lag alerts |
| Slot fills the primary's disk | Retained-WAL metric vs thresholds | Warning and critical alerts; `doctor` recommends `max_slot_wal_keep_size` |
| Slot invalidated or WAL removed | Replication start fails at the checkpoint | Gap recorded and alerted; actual PITR window shortened; new base backup forced |
| Oplog rolled over | Checkpoint older than the oldest oplog entry | Same as above |
| Two agents for one target | Lease state | Step-down at 90 s, takeover only after 2 min unchanged, generation-fenced checkpoints |
| Backup crashes mid-stream | Snapshot never committed | Orphaned packs removed by a later prune (after the safety margin) |
| Prune crashes mid-run | Leftover packs or index | Index rewritten only after pack deletion; next prune cleans up |
| Prune vs running backup | Locks | Backup holds shared lock; GC needs exclusive; young packs protected by the safety margin |
| Crashed process holds a lock | Lock not refreshed for 30 min | Lock treated as stale and removed |
| Backups silently failing | Dead-man's switch; failure counts | Critical alert; prune refuses to delete the last good snapshot |
| Backend lacks conditional create | `init` / `doctor` test | Fallback protocol; leases best-effort; warning shown |
| Repository on a network filesystem | `doctor` | Warning |
| Bucket lifecycle rules or Object Lock mismatch | `doctor` | Warning |
| Storage throttling or 5xx | Backend errors | Exponential backoff with jitter (1 s to 5 min); parts retried individually |
| Index lost or corrupt | `check` | Rebuild from packs |
| Chunk corrupted | AEAD tag or hash failure in `check` / `verify` | Exact chunks and affected snapshots reported; critical alert |
| Private key lost | — | Prevented: two slots or a paper key required at `init`; `status` warns on a single slot |
| Wrong tool or server version | Version checks | Clear error naming the fix (install the matching client or use Docker) |
| Notification channel down | Delivery errors | Outbox retries; work continues; attempts journaled |
| Restore target missing logs | Segment selection | Restore refuses with the exact gap and the nearest reachable points |
| Verify reaches the wrong point | L3 comparison | Verify fails; snapshot not marked verified; critical alert |

## 13. Testing, build and traceability

The design is proven mainly by tests that inject failure, and every design element traces back to a PRD decision.

### 13.1 How each component is tested

| Component | Tests |
| --- | --- |
| Chunker, hasher, compressor, crypto | Unit tests; fuzzing of every decoder; property test that reassembly equals input; tamper, wrong-key and "agent slot cannot decrypt" tests |
| Repository, locks, retention, prune | Model-based tests (random backup, prune, pin, crash and restart sequences against an in-memory model); kill tests mid-index and mid-prune |
| Leases | Two-agent races, holder kill and standby takeover, clock-skew tests |
| Backends | Shared conformance suite (local, MinIO) with a fault-injecting wrapper |
| Engines, capture, detectors | `testcontainers-go` across Postgres 16/17/18 and MongoDB 7.0/8.0/9.0; restore equality via per-table checksums; PITR to xid and time; event classification including rewrites; forced oplog rollover and slot invalidation |
| Scheduler | Fake-clock tests: overlap, missed runs, jitter, DST |
| CLI | `testscript` end-to-end, including the two-act demo |
| Agent | Soak under pgbench with random kills: flat memory, bounded lag, stable goroutines |
| Format compatibility | Golden repositories and a WAL corpus in `testdata/` |
| Everything | `-race`, `goleak`, secret-leak scan, `staticcheck`, `golangci-lint`, `govulncheck`, `gosec`, license and DCO checks |

Pipelines: every PR under 15 minutes (integration subset on Postgres 17 and MongoDB 8.0); nightly full matrix, chaos, long fuzzing and soak; release adds real AWS S3 and R2 and service installs on all three OSes.

### 13.2 Build and release

- Pure Go, no cgo; GoReleaser builds signed binaries for Linux, macOS and Windows on amd64 and arm64, with checksums; a multi-architecture container image; `go install`, a Homebrew tap and a Scoop bucket.
- Apache 2.0 with `LICENSE`, `NOTICE` and SPDX headers; DCO sign-off on commits.
- Microbenchmarks (chunker, BLAKE3, zstd, encryption, pack encoding) run in CI with `benchstat`; macro benchmarks run weekly and before releases via `make bench`.

### 13.3 Traceability to decisions

| Decision | Where in this document |
| --- | --- |
| 1 Agent and CLI in one binary | 1.2, 3.1, 9 |
| 2 Detection and side-instance restore | 6.3, 6.4 |
| 3 Hybrid storage | 5, 6.1 |
| 4 Teams by design, growth seams | 3 (core library), 5.1 (namespacing), 5.6 (versioning), 7 (identity, leases), 10.3 (journal) |
| 5 Postgres and MongoDB in v1 | 1.1, 4.2 |
| 6 Repository, socket, leases, catch-up | 6.2, 7.2, 7.3, 9.3 |
| 7 Native vs shelled-out, instance providers | 3.2, 4.2, 4.3 |
| 8 Scheduling | 9.2 |
| 9 Service and deployment | 9.5–9.7, 11 |
| 10 Encryption | 5.3, 8.1–8.4 |
| 11 Compression | 5.3, 6.1 |
| 12 Credentials | 4.4, 8.5 |
| 13 Retention | 6.6, 7.4 |
| 14 Backends | 4.1 |
| 15 Verify | 6.5 |
| 16 Notifications | 10.4 |
| 17 Observability | 10.1–10.3 |
| 18 License | 13.2 |
| 19 Testing | 13.1 |
| 20 Benchmarks | 13.2 |
| 21 Definition of done | PRD section 11.2 |

### 13.4 Out of scope

As listed in 1.1. The design leaves room for each: new engines implement the engine interface, new storage implements the backend interface, new secret sources implement the secret-provider interface, and a future `pentym server` calls the core library and reads the same repository and journal.
