# Pentymento — Product Requirements Document (v1)

Nov 23, 2026 · @Toluwalase Akinyemi

## 1. Summary

Pentymento (binary `pentym`) is a single Go binary that backs up and restores self-hosted Postgres and MongoDB, with point-in-time recovery (PITR) and an "undo DROP" workflow that recovers a dropped table or collection without rewinding the whole database.

Positioning: *Point-in-time recovery and undo-DROP for self-hosted Postgres, MySQL and MongoDB, as a single binary.* v1 ships Postgres and MongoDB; MySQL follows in v2 and SQLite in v3.

| Item | Value |
| --- | --- |
| Product name | Pentymento (a stylized spelling of *pentimento*: traces of an earlier version showing through a repainted canvas) |
| Binary | `pentym` |
| Former working name | undrop, dropped because it collides with the UnDROP for InnoDB recovery toolkit and with the `UNDROP` SQL commands in Snowflake, Databricks, ClickHouse and others |
| Language | Go |
| License | Apache 2.0 |
| Origin | roadmap.sh "Database Backup Utility" project (advanced), extended well beyond the brief |
| Target release | v1.0.0 |
| Basis | Every requirement here comes from decisions 1–21 agreed in the planning discussion of 21–23 Nov 2026 |

Requirement language: **MUST** is required for v1, **SHOULD** is expected unless a documented reason prevents it, and **LATER** marks work explicitly deferred past v1.

## 2. Background and problem

Most self-hosted databases are protected by a cron job running `pg_dump` or `mongodump`, which can lose hours of data and cannot undo a single mistake without rewinding everything.

### 2.1 The brief

The roadmap.sh brief asks for a CLI that:

- backs up and restores MySQL, PostgreSQL, MongoDB and SQLite;
- accepts connection details, tests credentials before a backup and handles connection failures gracefully;
- supports full, incremental and differential backups where the engine allows, with compression;
- stores backups locally and in a cloud service (S3, GCS or Azure Blob);
- logs start and end times, status, duration and errors, with an optional Slack notification on completion;
- restores whole databases, or selected tables or collections where supported;
- handles large databases efficiently, is secure and reliable, minimizes load on the server, runs on Windows, Linux and macOS, and has clear docs and a help command.

### 2.2 Why the ordinary solution falls short

The typical submission wraps the native dump tools, gzips the output, uploads it and relies on cron. That meets the checklist but leaves the real problems unsolved:

- **Recovery point equals the backup interval.** A `DROP TABLE` at 14:59 with hourly backups loses up to 59 minutes.
- **Restores are all or nothing.** Rewinding a whole database to undo one dropped table throws away every good write made since.
- **Every full backup costs its full size.** Daily fulls of a 50 GB database come to roughly 1.5 TB a month before compression.
- **Backups are rarely proven restorable.** A backup that has never been restored is not known to work.
- **The hard parts of the brief are vague.** "Incremental" means WAL in Postgres and the oplog in MongoDB; "efficient" means streaming with constant memory; "secure" means encryption and keeping secrets out of the process list.

### 2.3 Who is underserved

Managed services (RDS, Cloud SQL, Supabase, Neon) already include PITR, and most do not let external tools stream physical WAL. Self-hosted databases on a VPS, Docker, bare metal or a Kubernetes operator often have nothing better than `pg_dump` on cron. Pentymento targets that gap.

## 3. Goals, non-goals and success criteria

v1 succeeds when a dropped Postgres table and a dropped MongoDB collection can each be recovered to just before the drop, without losing later writes elsewhere, through the same commands.

### 3.1 Goals

1. **Undo accidents.** Recover to just before a destructive event (DROP, TRUNCATE, dropDatabase, bulk delete) found from the database's own logs.
2. **Continuous PITR.** Capture WAL and oplog continuously, so the recovery point is seconds, not the backup interval.
3. **Surgical restores.** Restore into a side instance and extract only the affected tables or collections by default.
4. **Cheap, frequent backups.** Deduplicate snapshots so daily or hourly full backups cost little.
5. **Proven backups.** Verify restorability automatically, including the recovery point actually reached.
6. **Secure by default.** Always encrypt; agents can write backups but cannot read or delete them.
7. **Restores never depend on the agent.** The repository is the single source of truth; the CLI works alone.
8. **Low impact.** Constant memory regardless of database size, throttled I/O, measured overhead on the database.
9. **Cross-platform single binary.** Linux, macOS and Windows; installable as a service.
10. **Designed to grow to teams** without a rewrite (section 5.4).

### 3.2 Non-goals for v1

- MySQL, SQLite, sharded MongoDB, standalone MongoDB PITR.
- Physical WAL capture on managed Postgres services (they rarely allow it); managed databases get snapshots only.
- Logical-decoding DML detection on Postgres and the optional DDL event trigger (both v1.x).
- GCS, Azure Blob, SFTP and rclone backends; `pentym copy` between repositories.
- Pack compaction during prune; the Postgres `archive_command` mode.
- A control plane, web UI, SSO, user accounts or access roles; OpenTelemetry tracing.

### 3.3 Success criteria

- The two-act demo (section 11.2) runs as an automated test and is recorded for the README.
- Five headline benchmarks are measured and published with raw data (section 10.3).
- All quality gates in section 11.2 pass, including two consecutive weeks of green nightly model-based and kill tests.
- No known bug in the categories data loss, silent gap or false "verified".

## 4. Users and deployment

Pentymento is designed for teams but built for solo developers first, and focuses on people who run their own Postgres or MongoDB.

### 4.1 Personas

| Persona | Setup | Needs | v1 priority |
| --- | --- | --- | --- |
| Solo developer / indie hacker | One VPS, 1–3 databases, often on the app host | `pentym init` done in under two minutes, sane defaults, cheap storage (R2, B2, local disk), a clear `status` | Experience built for first |
| Small team / startup | A handful of self-hosted databases, sometimes two engines, several people | Multiple named targets, shared repository, notifications, per-role credentials, `--json` output and stable exit codes for CI | Design target |
| Platform team / enterprise | Fleets, central control, policies, audit, SSO, UI | Control plane, access roles, approvals | Out of scope for v1; enabled later by the growth seams |

### 4.2 Where pentym runs

Nothing has to run on the database host: Postgres replication and MongoDB oplog tailing both work over the network.

| Part | Network access to | Needs locally | Runs on |
| --- | --- | --- | --- |
| Agent (capture, scheduling) | Database and storage | CPU for hashing and compression, a small disk cache | The database host or any machine that can reach it |
| `pentym backup` (one-shot) | Database and storage | As the agent | As the agent, plus cron or CI |
| `pentym restore` / `verify` | Storage, plus production for extraction | Disk about the size of the database; Docker or matching database binaries | Admin machine, restore host, CI runner |
| `list`, `status`, `events`, `check`, `prune` | Storage only | Almost nothing | Anywhere |

### 4.3 Supported deployment setups

1. **All on one host** (solo VPS): simplest; peer authentication over a Unix socket means no password; I/O throttling on by default. If the host dies, everything already uploaded is safe.
2. **Dedicated backup host** (small teams): agents for several databases on one VM; backup load never touches the database hosts; requires TLS to the database and access rules (`pg_hba.conf`, MongoDB) for the backup host.
3. **Kubernetes**: one Deployment per target with 2 replicas that fail over through the repository lease.

`pentym init` MUST ask which setup applies and configure accordingly: same host → peer or socket authentication plus throttling; separate host → require TLS and print the access-rule lines needed.

Resource footprint: memory MUST stay constant whatever the database size; CPU is spent mainly on zstd and BLAKE3 during base backups; restores temporarily need about the database's size in disk.

## 5. Principles and core architecture

One binary runs in two modes over one repository, and the repository, not the agent, is the source of truth for every backup and restore.

&#91;embedded content: pentym architecture · agent, CLI, repository, side instance\]

The agent writes to the repository with write-only credentials; the CLI restores from the repository through a side instance and extracts only what was lost, whether or not the agent is running.

### 5.1 Principles

1. **Restores never depend on the agent.** The CLI MUST be able to list, verify and restore from the repository alone.
2. **Nothing correctness-critical goes only through the control socket.** If deleting the socket code would break a restore, the design is wrong.
3. **Never imply protection that doesn't exist.** `status` MUST report the actual recovery window, gaps included, and stale agents.
4. **The agent is an optimization over a correct one-shot core.** It is built from the same primitives (chunker, repository, uploader).
5. **Any future control plane is optional.** Restores never depend on it either.

### 5.2 Operating modes

- **One-shot commands** (`backup`, `restore`, `verify`, `prune`, `list`, `status`, `events`, `check`, `catch-up`) work with or without an agent and can be run from cron or CI.
- **`pentym agent`** adds continuous log capture and the built-in scheduler. PITR requires it.
- Engines declare their capabilities; the tool reports them plainly:

| Engine | Snapshots | PITR | Log capture | Release |
| --- | --- | --- | --- | --- |
| Postgres 16, 17, 18 | Yes | Yes | Agent (physical replication slot); `archive_command` LATER | v1 |
| MongoDB 7.0, 8.0, 9.0 (replica sets) | Yes | Yes | Agent (oplog tail) | v1 |
| MongoDB standalone | Yes | No | None (no oplog) | v1, snapshots only |
| MySQL | Yes | Yes | Agent (binlog stream) | v2 |
| SQLite | Yes | No | Snapshots only | v3 |

### 5.3 How the CLI and agent interact

- **Repository first.** The agent writes heartbeats, checkpoints, events and schedule state to the repository; the CLI reads them. Snapshots and log segments share one repository, so users can start one-shot and add the agent later without migrating.
- **Local control socket.** The agent MUST open a Unix socket (Linux, macOS) or named pipe (Windows), protected by file permissions and never exposed to the network, serving JSON over HTTP. It is used only for `status --live` (replication lag, slot WAL retention, upload queue depth), `agent reload` (reload config without dropping replication connections) and `backup --now`. When unreachable, commands MUST fall back to the repository and say so ("agent not reachable; showing last heartbeat from 14:02:11"). The same handlers can later become `pentym server`'s API.
- **Leases.** The agent MUST hold a lease in the repository (one active capturer per target), renewed periodically and expiring after a set time (for example 2 minutes). Liveness is judged from the lease, never from the socket, so an unreachable socket never leads to a second capturer.
- **`pentym catch-up <target>`.** While the agent is down, Postgres keeps WAL for the slot and MongoDB keeps oplog entries until rollover. `catch-up` MUST connect, upload what is still retained, record a checkpoint and exit. It runs by hand, from cron or from a standby agent. Running it automatically when status sees a stale heartbeat is opt-in (catch\_up.auto: true); otherwise status prints the exact command to run. If WAL was dropped (`max_slot_wal_keep_size`) or the oplog rolled over, it MUST report the exact gap and recommend a new base backup.
- **Behaviour with the agent down:** `restore`, `backup`, `list`, `events`, `check`, `verify` and `prune` work fully; `status` works with a warning ("agent last seen 2h 14m ago; PITR covers up to 14:02:11"); only socket features are unavailable. Continuous capture stops, so the recovery window stops growing.

### 5.4 Growth seams (built into v1)

1. Core Go library with a thin CLI; a server or API is just another caller.
2. Namespaced repository layout (`targets/<target-id>/…`) from day one.
3. Versioned repository format; binaries refuse formats newer than they understand.
4. Append-only **operations journal** in the repository (who, host, what, when, result) — the future audit log.
5. Locks and heartbeats carry identity (user, host, process, agent ID).
6. Every operation is labelled **read** (list, verify), **write** (backup, capture) or **destructive** (prune, rewind); today it drives confirmation prompts, later access roles.
7. Agents use write-only storage credentials; pruning uses separate credentials; S3 Object Lock or append-only mode supported.
8. Pluggable secret providers.
9. Agent state (checkpoints) lives in the repository, with a lease, so a standby agent can take over.
10. Composable, declarative config (includes, environment overrides).
11. `--json` output, stable exit codes and Prometheus metrics.

Growth path: **v1** single binary; **v2** optional `pentym server` (agents register; fleet health, config and policy distribution, audit view, API); **v3** access roles, SSO, UI and restore approvals. Not in v1: web UI, SSO, user and permissions server, multi-tenant hosting, approval workflows.

## 6. v1 scope

v1 covers Postgres and MongoDB in depth, with MongoDB introduced at milestone M2 so that two very different engines shape the engine interface.

### 6.1 Engines and versions

| Engine | Versions in the test matrix | Topologies | Notes |
| --- | --- | --- | --- |
| Postgres | 16, 17, 18 (add 19 if released by kickoff) | Self-hosted primary | 14 reaches end of life in Nov 2026; WAL parsing is tested per major version |
| MongoDB | 7.0, 8.0, 9.0 | Replica sets, including single-node | Standalone gets snapshots only; sharded clusters are out of scope |

Order after v1: **MySQL** (v2, binlogs via `go-mysql`), then **SQLite** (v3, snapshots via the backup API with the chunk store giving incremental backups). This order goes from most to least similar, so each engine reuses more earlier work.

### 6.2 Platforms

- **Linux:** first-class for both agent and CLI.
- **macOS and Windows:** CLI fully supported; the agent runs as a service but gets less testing in v1.
- **Architectures:** amd64 and arm64 release binaries.
- **Development on Windows:** run test databases through Docker Desktop with WSL2; running the integration suite inside WSL is usually smoother.

### 6.3 In v1

| Area | Ships |
| --- | --- |
| Core | Versioned repository format, chunk store (FastCDC, keyed BLAKE3, zstd, encryption, packs, rebuildable index), plain objects for logs, locks, leases, operations journal |
| Storage | Local filesystem and S3-compatible (AWS S3, MinIO, R2, B2, Wasabi, Spaces) |
| Backup | Postgres native `BASE_BACKUP` (fallback `pg_basebackup`); MongoDB `mongodump --oplog --archive`; streamed into the chunk store with no temporary files |
| Continuous capture | Agent: Postgres WAL over a physical slot; MongoDB oplog tail; checkpoints in the repository; heartbeat; slot-lag and oplog-window monitoring; gap detection |
| Detection | Postgres commit-record scanning plus catalog timeline; MongoDB drop, dropDatabase, rename and bulk delete; events timeline |
| Restore | Side instance and extract by default (`--at`, `--xid` on Postgres, `--before-event`); `--mode=rewind` opt-in |
| Trust | `verify` (quick, standard, deep), `check`, `doctor` |
| Operations | Built-in scheduler, retention with PITR window, two-phase prune, `status`, `history`, webhook / Slack / heartbeat-ping notifications, Prometheus metrics, `debug bundle` |
| Security | Write-only asymmetric encryption, key slots, per-role credentials, secret references, no secrets on command lines or in logs |
| Service | `agent install` on Linux, macOS and Windows; container image; example Kubernetes manifests |
| Docs | `--help` everywhere, README, repository-format spec, crypto spec, architecture doc, operations guide |

Everything excluded from v1 is listed in sections 3.2 and 11.3.

## 7. Functional requirements

This section specifies what each part of pentym MUST do in v1.

### 7.1 Repository and storage format

The repository uses a hybrid layout: a deduplicated chunk store for snapshots and plain objects for log segments.

```
repo/
  config                     # repo format version, key parameters
  keys/                      # key slots
  locks/
  snapshots/<id>             # snapshot = chunk refs + metadata
  index/<id>                 # chunk → pack mapping, wrapped data keys
  data/<xx>/<pack-id>        # ~16 MB packs of chunks
  wal/<target>/<timeline>/<segment>.zst.enc
  events/<target>/...
  heartbeat/agent-<host>
```

- **Snapshot pipeline:** source stream → FastCDC chunker (\~1 MB average) → keyed BLAKE3 chunk ID → dedupe check against the index → zstd → XChaCha20-Poly1305 → pack (\~16 MB) → upload. Each unique chunk is stored once; a snapshot is a list of chunk IDs.
- **Log segments** (WAL, oplog batches) MUST be stored as individual objects, each compressed and encrypted, so recovery reads them sequentially and each can expire on its own.
- **Chunk IDs** MUST be keyed hashes, so identical content is not revealed to anyone without the chunk-ID key.
- **The index MUST be rebuildable** by scanning packs (the index is otherwise a single point of failure).
- **Snapshots stand alone:** no fragile incremental chains.
- **Restores** fetch chunks with parallel range reads and read-ahead.
- **Memory:** the index fits in memory or uses on-disk lookups when the repository is large.
- **Export:** `pentym dump <snapshot>` MUST export a plain file, and the on-disk format MUST be published as a spec, so data is never locked in.
- **Namespacing:** target IDs in paths are opaque; friendly names live in encrypted metadata.
- **LATER:** compaction of partly used packs.

### 7.2 Base backups (snapshots)

- **Postgres:** native `BASE_BACKUP` over a replication connection (via `pglogrepl`), streaming the tar directly into the chunker. Fallback: shell out to `pg_basebackup`. pglogrepl base-backup support has been verified against Postgres 16, 17 and 18.
- **MongoDB:** shell out to `mongodump --oplog --archive` to stdout for a consistent base. A native rewrite is possible after v1.
- **No temporary files:** everything streams; constant memory regardless of database size.
- **The source MUST NOT compress** (`mongodump --gzip`, `pg_basebackup -Z`), since compressed streams defeat deduplication. pentym builds tool commands itself and rejects compression flags in any extra arguments.
- **Commit only on success:** a snapshot is committed only after the stream and any child process finish cleanly; a partial stream never becomes a snapshot. Orphaned chunks are removed by the next prune.
- **Shelling out safely:** no secrets on the command line (temporary `.pgpass` or Mongo `--config` files with 0600 permissions, deleted afterwards and cleaned up on next start; `PGPASSWORD` in the child environment only as a fallback); stdout streamed into the pipeline; each child in its own process group with context cancellation (job objects on Windows); stderr captured into structured logs; exit codes checked; tool versions checked up front.

### 7.3 Continuous capture (agent)

**Postgres**

- Stream WAL natively over a physical replication slot (`START_REPLICATION PHYSICAL`, via `pglogrepl`), the way `pg_receivewal` does.
- Upload completed segments as objects; on shutdown upload the current partial segment as `.partial`, then acknowledge the safely stored position so the slot can free WAL.
- Keep timeline history files as long as any snapshot or segment refers to that timeline.
- **Slot safety:** a slot retains WAL until acknowledged, so a dead agent can fill the primary's disk. pentym MUST monitor slot lag and retained WAL, recommend `max_slot_wal_keep_size`, and warn and alert at thresholds.

**MongoDB**

- Tail `local.oplog.rs` directly with a tailable cursor (not change streams, whose output cannot feed `--oplogReplay`), storing entries in batches.
- **Rollover detection:** if the last captured timestamp is older than the oldest oplog entry, report the gap, alert and force a fresh base backup. Monitor the oplog time window.

**Both**

- Checkpoints (last confirmed position) live in the repository; on restart, resume from the last confirmed position. Duplicate segments are harmless.
- Unfillable gaps MUST be reported loudly and trigger a new base backup, the same path as `catch-up`.

### 7.4 Destructive-event detection

**Postgres (v1):** the WAL holds no SQL text, so detection reads its records.

- Scan **commit records** for dropped relfilenodes and **database-drop records**.
- Keep a **catalog timeline** (oid, relname, relfilenode from `pg_class`), refreshed by the agent, to map relfilenodes back to table names as they were at that moment.
- Classify correctly: **oid gone = drop**; **oid survives with a new relfilenode = TRUNCATE or rewrite** (`VACUUM FULL`, `CLUSTER`, some `ALTER TABLE`). Rewrites MUST NOT be reported as drops. Handle TOAST tables and indexes.
- Postgres can stop recovery exactly before a transaction (`recovery_target_xid` with `recovery_target_inclusive = false`), so a found event gives an exact restore point.
- **v1.x (LATER):** a logical-decoding slot (`pgoutput`) for bulk DML detection (requires `wal_level = logical` and a restart; a second slot doubles WAL-retention risk), and an optional event trigger journaling DDL text (needs superuser, cannot see `DROP DATABASE`), which only enriches events.

**MongoDB (v1):** the oplog is structured documents.

- Detect `drop`, `dropDatabase` and `renameCollection` commands (`op: "c"`).
- Detect **bulk deletes** as a burst of `op: "d"` entries on one collection.

**Events timeline (both):**

```
pentym events prod --since 2h
#3  14:03:12  xid 8812  DROP TABLE   public.orders
#2  13:41:07  xid 8790  TRUNCATE     public.sessions
pentym restore prod --before-event 3
```

### 7.5 Restore

PITR on Postgres restores the whole cluster, so the default is to restore into a side instance and extract only what was lost.

1. Restore the base snapshot into a temporary instance and replay logs, stopping just before the bad transaction (Postgres) or timestamp (MongoDB).
2. Dump only the affected tables or collections from it.
3. Load them into production under a safe name such as `orders_recovered_20261122`, or in place with `--in-place`.

- **Targets:** `--at <time>`, `--xid <xid>` (Postgres), `--before-event <n>`; LSN targets are supported on Postgres.
- **Postgres extraction:** `pg_dump -t` from the side instance, `pg_restore` into production, preserving indexes, constraints and sequences.
- **MongoDB:** `mongorestore --oplogReplay --oplogLimit <ts>` into the side instance; extract with `mongodump --nsInclude` and load with `mongorestore --nsFrom/--nsTo`.
- **`--mode=rewind`** restores the whole cluster to the target point; it is destructive and opt-in.
- Restores read only the repository and never need the agent.

### 7.6 Instance providers

Restore and `verify` both need temporary database servers, behind one interface:

- **Local binaries:** find `postgres` or `mongod` on the PATH or at a configured path, check the major version matches the backup, run in a temporary directory.
- **Docker:** run `postgres:<major>` or `mongo:<version>` with the restore directory mounted (the easiest path on Windows and macOS).
- Configured order: `instance_provider: [docker, local]`. `pentym doctor` reports what is available and explains mismatches ("pg\_dump 15 can't dump a Postgres 17 server; install postgresql-client-17 or use the Docker provider").
- Cleanup MUST be guaranteed on success, failure and cancellation; crash leftovers are removed on the next run.

### 7.7 Compression

- **zstd via `klauspost/compress`** (pure Go, no cgo, so single-binary cross-compilation stays trivial).
- Default level (roughly zstd 3) for the agent and log segments; `compression: fastest | default | better | best` configurable per target.
- **Order:** chunk and hash the raw content, then compress, then encrypt. Dedupe is independent of compression settings.
- **Per chunk** (so single chunks can be range-read) and per log segment as a whole object.
- **Skip incompressible chunks:** if compression saves less than about 3%, store raw. A one-byte chunk header records `none` or `zstd`, leaving room for other algorithms without a format change.
- **Parallel** compression through a worker pool sized by the concurrency and I/O limits (7.9).

### 7.8 Storage backends

**v1:** local filesystem and S3-compatible storage (AWS S3, MinIO, Cloudflare R2, Backblaze B2, Wasabi, DigitalOcean Spaces). **LATER:** native GCS and Azure Blob, SFTP, an rclone bridge, and `pentym copy` for 3-2-1 replication between repositories.

Backend interface:

```go
type Backend interface {
    Put(ctx, key string, r io.Reader, opts PutOptions) error   // opts.IfNotExists
    Get(ctx, key string, rng *Range) (io.ReadCloser, error)     // range reads
    Stat(ctx, key string) (ObjectInfo, error)
    List(ctx, prefix string, fn func(ObjectInfo) error) error
    Delete(ctx, key string) error
    Capabilities() Capabilities   // ConditionalCreate, ObjectLock, StrongConsistency
}
```

- **Conditional create** is required for locks and leases: `O_CREAT|O_EXCL` locally, `PUT` with `If-None-Match: *` on S3. `init` and `doctor` MUST test it by attempting the same conditional create twice. If unsupported, fall back to a weaker write-wait-list protocol, warn clearly and mark the lease best-effort.
- **Local:** write to a temp file, `fsync`, rename, `fsync` the directory; retry renames on Windows (antivirus and indexer locks); fan out directories (`data/ab/<pack>`); `doctor` warns if the repository is on a network filesystem (older NFS, SMB).
- **S3:** `aws-sdk-go-v2` with custom endpoints; single PUTs for \~16 MB objects, multipart for larger with per-part retries; exponential backoff with jitter on throttling and 5xx; bounded concurrent requests; parallel range reads with read-ahead on restore.
- **Local metadata cache** (index, snapshot list) to reduce LIST and GET calls; disposable and rebuilt from the repository.
- Docs MUST note egress costs (for example, R2 charges nothing for egress).

### 7.9 Scheduling

The agent has a built-in scheduler; one-shot commands remain usable from cron, Task Scheduler or CI.

```yaml
targets:
  prod-pg:
    engine: postgres
    schedule:
      backup: "daily 02:00"
      verify: "weekly sun 04:00"
      prune:  "after backup"
    timezone: Africa/Lagos
```

- Jobs: `backup`, `verify`, `prune`, `check`. `catch-up` runs from a standby agent or cron when capture stalls; automatic runs from status are opt-in (catch\_up.auto).
- Accepts friendly forms (`every 6h`, `daily 02:00`, `after backup`) and 5-field cron expressions (parsed with a small library such as `robfig/cron/v3`'s parser; the loop is our own, about 200 lines, tested with a fake clock).
- **No overlapping runs:** a run still in progress causes the next to be skipped and logged.
- **Missed runs:** after downtime, run once if still within a grace window (for example 6 hours); never replay every missed run.
- **Jitter** to avoid many agents hitting storage at the same instant.
- **Concurrency and I/O limits** per agent, protecting the database host.
- **Schedule state** lives in the repository; only the lease holder runs schedules.
- **Dead-man's switch:** alert if no successful backup exists within 1.5× the expected interval; `status` applies the same check for cron users.
- Every run, including skips and their reasons, is journaled.

### 7.10 Retention and prune

PITR to time *T* needs a snapshot taken before *T* plus every log segment from that snapshot to *T*.

```yaml
retention:
  pitr_window: 7d
  snapshots:
    keep_last: 3
    keep_daily: 7
    keep_weekly: 4
    keep_monthly: 12
    keep_yearly: 0
  events: 90d
  journal: 365d
```

- **Anchor rule:** the newest snapshot taken before the PITR window starts MUST be kept even if snapshot rules would drop it; logs older than the anchor's start position (LSN or oplog timestamp) may be deleted.
- **Snapshot rules** (grandfather-father-son): a snapshot is kept if any rule wants it; `keep_within: 30d` also supported.
- **Pins:** `pentym pin <snapshot> --reason "pre-migration"` keeps a snapshot until unpinned; pins are journaled.
- **Events** are kept for their configured time and at least as long as the logs they point to. The **journal** has its own long retention, with old entries summarised in batches.
- **Safety rules:**
  - Never delete the newest successful snapshot, nor the newest verified one when verify is enabled. If the last 3 backups failed, prune refuses to delete older good ones and alerts.
  - Report the actual window, not the configured one ("configured 7d, actual 2d 4h (gap at 2026-11-21 03:12)").
  - Interactive prune shows a dry run and asks for confirmation; scheduled runs journal everything.
  - `doctor` warns when bucket Object Lock periods exceed the policy (deletes would fail) and when bucket lifecycle rules could delete objects behind pentym's back.
- **Two-phase prune:**
  1. **Forget** (metadata): apply the policy, delete expired snapshot records, delete log segments older than each anchor. Log deletion needs no chunk-store lock, so continuous capture never blocks it.
  2. **Garbage collection:** take the exclusive chunk-store lock (snapshot backups hold a shared lock while running); mark chunks referenced by remaining snapshots; delete packs with no referenced chunk; never delete packs younger than about 24 hours; rewrite the index after deleting packs; release the lock. A crash leaves only leftovers for the next prune, never lost snapshot data.
- **`init` defaults:** `pitr_window: 7d`, `keep_last: 3`, `keep_daily: 7`, `keep_weekly: 4`, `keep_monthly: 6`, daily prune after backup, plus a printed storage estimate ("about 1.3× database size plus 7 days of WAL").

### 7.11 Verification

| Level | Checks | Needs a database |
| --- | --- | --- |
| L0 repository integrity | Referenced objects exist; chunk hashes and AEAD tags match; index consistent; at least one key slot unlocks | No |
| L1 reassembly | Decrypt and rebuild the whole snapshot stream; tar valid and complete; `mongodump` archive parses | No |
| L2 boot and recover | Side instance starts, replays logs, reaches a consistent state, accepts queries | Yes |
| L3 recovery point | Postgres: `pg_last_xact_replay_timestamp()` and last applied LSN match the repository; MongoDB: last applied oplog timestamp matches the last captured batch | Yes |
| L4 contents | Object inventory recorded at backup time compared within tolerance; `pg_amcheck` and MongoDB `validate` | Yes |
| L5 random point | Restore to a random moment within the PITR window to prove log continuity | Yes |

| Preset | Levels | Default schedule |
| --- | --- | --- |
| `--level quick` | L0 (sampled), L1 on the latest snapshot | Daily |
| `--level standard` | L0–L3, plus L4 on a sample | Weekly |
| `--level deep` | All levels, full L4 and L5 | Monthly |

- Never compares against live production (it keeps changing); checks run against what was recorded at backup time.
- **Optional canary** (opt-in, writes to the database): the agent updates a tiny `pentym.canary` table or collection each minute, and `verify` checks the restored value.
- Uses instance providers; refuses clearly on major-version mismatch; configurable time limit per run; guaranteed cleanup.
- Needs the full key slot (in CI it belongs in a protected environment).
- Output: human report and `--json` (pass/fail per level, durations, recovery point reached, differences). Results are journaled and the snapshot is marked **verified** in the repository; retention relies on that marker. Failures send a critical notification. `status` shows the last verification per target.

### 7.12 Notifications

**Channels in v1:** generic webhook (JSON, versioned stable schema, HMAC-signed with an `X-Pentym-Signature` header); Slack (incoming webhooks, formatted); heartbeat pings (GET after each successful job; compatible with healthchecks.io, Uptime Kuma, Better Stack). **LATER:** email (SMTP), Discord, Microsoft Teams, PagerDuty, ntfy.

| Severity | Events |
| --- | --- |
| Critical | Destructive event detected (DROP TABLE, dropDatabase, bulk delete); verify failed; log-capture gap (WAL lost, oplog rolled over); no successful backup within 1.5× the expected interval; slot lag past the critical threshold; `check` found corruption |
| Warning | Single backup failed; agent failover (lease taken over); slot lag past the warning threshold; oplog window shrinking below a safe margin; prune refused to delete the last good snapshot; only one key slot exists |
| Info | Backup succeeded; verify passed; prune summary. Off by default except "backup succeeded", which can be enabled to meet the brief |

- **Actionable:** every message includes what happened, the target, the time and the next command to run (for example `pentym restore prod-pg --before-event 3`).
- **Routing** by target, minimum severity and event type to named channels.
- **Anti-fatigue:** deduplication with cooldown; a resolved message when a condition clears; flapping protection.
- **Who alerts when pentym is down:** heartbeat pings let an external monitor alert when pings stop; the dead-man's switch also runs whenever any command reads the repository.
- **Reliable delivery:** a local outbox with retries and backoff; notifications never block or fail real work; every attempt and result is journaled.
- **Privacy:** webhook URLs and HMAC secrets are secret references; alerts carry metadata only, never row data; `notify.redact_object_names: true` hides table and collection names; HTTPS only.
- `pentym notify test --channel <name> --severity <level>` sends a sample alert.

### 7.13 Observability

- **Logs:** `slog`; JSON on stdout for services, text in a terminal (`--log-format` overrides). Every line carries `target`, `job`, `run_id` and `component`; child-process stderr is captured and tagged with the same `run_id`. `info` stays to a handful of lines per minute on a busy agent. Secrets are redacted automatically.
- **Metrics:** Prometheus at `/metrics` on the control socket and on the opt-in health port. Labels per target and job, never per table. Required metrics: `pentym_capture_lag_seconds`, `pentym_pg_slot_retained_wal_bytes`, `pentym_mongo_oplog_window_seconds`, `pentym_pitr_window_seconds`, `pentym_last_success_timestamp_seconds`, `pentym_job_duration_seconds` (histogram), `pentym_job_failures_total`, `pentym_repo_bytes{kind}`, `pentym_dedup_ratio`, `pentym_backend_request_duration_seconds` and errors, `pentym_upload_queue_depth`, `pentym_events_detected_total{type}`, `pentym_lease_held`, `pentym_verify_last_result`.
- **History:** the operations journal in the repository; `pentym history <target> --since 30d` (and `--json`).
- **Progress:** progress bars on stderr in a terminal (bytes, rate, time remaining, dedupe so far); a JSON event stream with `--json`.
- **Debugging:** `pentym debug bundle` (version info, `doctor` output, redacted config, recent logs, last journal entries); pprof endpoints on the control socket, off by default (`agent.debug.pprof: true`).
- **Ships with the repository:** a Grafana dashboard JSON and example Prometheus alert rules mirroring the critical notifications.
- **LATER:** OpenTelemetry tracing, behind a small internal interface; most useful once `pentym server` exists.

### 7.14 Running as a service

`pentym agent install` / `uninstall` / `start` / `stop` register the agent with the platform's service manager via `kardianos/service`; `--print` outputs the unit files for manual or config-management use.

| Platform | Mechanism | Runs as | Config location |
| --- | --- | --- | --- |
| Linux | systemd unit (`Restart=on-failure`) | Dedicated `pentym` user | `/etc/pentym/config.yaml` |
| macOS | launchd daemon (`KeepAlive`) | Dedicated `_pentym` user | `/usr/local/etc/pentym/` |
| Windows | Windows service (automatic start, restart on failure) | Virtual account `NT SERVICE\pentym` | `%ProgramData%\pentym\` |
| Kubernetes | Deployment per target, 2 replicas competing for the lease | Non-root container | ConfigMap plus Secrets |

- **Graceful shutdown** (SIGTERM or service stop): stop starting jobs; abandon uncommitted backups safely; upload the partial WAL segment as `.partial` and acknowledge the stored position (MongoDB: upload the current batch and record its timestamp); write a final `state: stopped` heartbeat and release the lease; hard deadline about 30 seconds.
- **Crash recovery:** resume from the repository checkpoint at the last confirmed position; unfillable gaps reported and a new base backup forced.
- **Logging:** JSON to stdout (journald, Kubernetes); warnings and errors also to the Windows Event Log; optional rotating `log.file`.
- **Health:** `/health` and `/ready` on the control socket; an opt-in TCP port, off by default and bound to localhost or the pod IP, exposes only `/health`, `/ready` and `/metrics`. No control action is ever reachable over the network.
- **Upgrades:** stop, replace binary, start; the format version check refuses incompatible repositories with a clear message. The repository format is never changed as a side effect of an upgrade: a format change happens only through an explicit pentym repo upgrade, run after every binary using that repository has been updated. Within one repository format, any mix of pentym versions (agents, CLIs, CI jobs) MUST work together.
- **Least privilege:** the service user needs only its config and secret files, a local cache directory and network access.

### 7.15 CLI surface and configuration

| Command | Purpose |
| --- | --- |
| `pentym init` | Create an encrypted repository; at least two key slots or an exported paper key; an agent slot; conditional-create test; deployment-setup questions; recommended mode from the acceptable data loss; storage estimate; `--print-sql` / `--print-mongo` for minimal roles |
| `pentym backup <target>` | Take a base snapshot now (`--now` routes through a running agent) |
| `pentym agent` | Run continuous capture and schedules; `install`, `uninstall`, `start`, `stop`, `reload` |
| `pentym catch-up <target>` | Drain retained WAL or oplog after agent downtime |
| `pentym restore <target>` | `--at`, `--xid`, `--before-event`, `--in-place`, `--mode=rewind` |
| `pentym events <target>` | List detected destructive events (`--since`) |
| `pentym list` | List snapshots and log coverage |
| `pentym status` | Recovery windows, last heartbeat, last verify, dead-man check; `--live` via the socket |
| `pentym verify <target>` | `--level` quick, standard or deep |
| `pentym check` | Repository integrity |
| `pentym prune` | Apply retention (dry run and confirmation when interactive) |
| `pentym pin <snapshot>` | Keep a snapshot until unpinned (`--reason`) |
| `pentym dump <snapshot>` | Export a plain file |
| `pentym history <target>` | Show the operations journal |
| `pentym key add / list / remove / rotate / export --paper` | Manage key slots |
| `pentym doctor` | Tools, versions, capabilities, privileges, bucket settings |
| `pentym notify test` | Send a sample notification |
| `pentym debug bundle` | Collect a redacted support archive |

- Every command MUST have `--help` with examples, `--json` output and stable exit codes.
- Config is YAML, declarative and composable (includes, environment overrides), supports multiple named targets from day one, and holds secrets only as references (section 8.3).
- Operations labelled destructive (prune, rewind) MUST ask for confirmation when interactive.

## 8. Security requirements

A compromised database host MUST be able to neither read nor delete backup history: agents hold only a public key and write-only storage credentials.

### 8.1 Encryption design

Encryption is always on. The only exception is `--insecure-test-mode` for local development, which refuses non-local backends.

| Key | Type | Held by | Used for |
| --- | --- | --- | --- |
| Repository key pair | X25519 public/private | Public: agents and CLI. Private: only those allowed to restore | Wrapping data keys |
| Metadata key | Symmetric | Agents and admins | Encrypting snapshots, index, journal, events, checkpoints |
| Chunk-ID key | Symmetric | Agents and admins | Keyed BLAKE3 chunk IDs |

**Writing (agent):**

1. Generate a random data key per pack (and per log-segment object).
2. Encrypt each chunk separately with XChaCha20-Poly1305, the data key and a random 24-byte nonce, so restores can fetch single chunks.
3. Wrap the data key to the repository public key (X25519, HKDF, then an AEAD, the same construction `age` uses) and store the wrapped key in the pack's index entry.
4. Discard the data key; the agent cannot read what it wrote.

**Deduplication still works:** the agent checks chunk IDs (keyed hashes of plaintext) against the index, which is encrypted with the metadata key it holds, without decrypting any chunk.

**Restoring:** unlock the private key from a key slot, unwrap data keys, decrypt chunks.

### 8.2 Key slots, rotation and recovery

- The private key is stored only inside slots in `keys/`: a **passphrase slot** (Argon2id with tuned parameters, the default), an **age identity slot** (for automation), and **cloud KMS / Vault slots** LATER.
- **Agent slot:** contains only the metadata key, chunk-ID key and public key.
- Commands: `pentym key add`, `key list`, `key remove`, `key rotate`, `key export --paper`.
- **Rotation:** `key rotate` generates a new key pair and re-wraps every data key in the index; since wrapped keys live in the index, rotation never rewrites pack data.
- **Guard:** with `agent.write_only: true` (the default), the agent MUST refuse to start if it holds the private key.
- **Key loss** makes every backup unreadable by design. `init` MUST require a second passphrase slot or a printed paper recovery code; `verify` and `check` confirm a slot unlocks; `status` warns when only one slot exists.
- **Not hidden (documented in `docs/crypto.md`):** object sizes, counts and timing; chunk equality (only to holders of the chunk-ID key); compressed sizes reveal a little about content.
- **Libraries only, no home-made primitives:** `golang.org/x/crypto` (chacha20poly1305, argon2, hkdf, curve25519), `filippo.io/age`, `zeebo/blake3`. The composition is specified for public review.

### 8.3 Credentials and secrets

| Secret | Agent | One-shot backup | Restore and extract | Prune and admin |
| --- | --- | --- | --- | --- |
| DB backup credential | Yes | Yes | — | — |
| DB replication credential | Yes | `catch-up` only | — | — |
| DB restore credential | — | — | Yes | — |
| Storage credential, write-only | Yes | Yes | — | — |
| Storage credential, full | — | — | Read | Yes |
| Agent key slot | Yes | Yes | — | — |
| Full key slot | Never | — | Yes | Yes |
| Notification webhook URLs | Yes | Yes | Yes | Yes |

- **Secrets are references, never values:** `env:`, `file:` (Docker and Kubernetes secret mounts), `keychain:` (macOS Keychain, Windows Credential Manager, Linux Secret Service), `cmd:` (runs a helper such as `op read` or `vault kv get`). Native Vault and cloud secret managers are LATER.
- **Plaintext values in config** are rejected unless `allow_plaintext_secrets: true` is set and the file isn't group- or world-readable; `doctor` warns either way.
- **Prefer no secret:** Postgres peer authentication over a Unix socket, client TLS certificates, `.pgpass` or service files; MongoDB x.509; the AWS default credential chain (instance roles, IRSA, OIDC) instead of static keys.
- **Least privilege:** `init --print-sql` / `--print-mongo` generate minimal roles. Postgres: `pentym_backup` and `pentym_repl` with `REPLICATION` but not superuser, catalog read access, and a separate `pentym_restore` with create rights only in the target schema. MongoDB: built-in `backup` plus read on `local`, and `restore` only for extraction.
- **`doctor` flags over-privilege**, not just missing privilege ("pentym\_repl is a superuser; it only needs REPLICATION").
- **`Secret` type:** `String()`, `Format`, `MarshalJSON` and `slog.LogValuer` return `[redacted]`.
- **Resolved when used:** each new connection re-reads the reference, so rotation needs no restart.
- **Scrubbed errors:** driver errors are wrapped and passwords stripped before logging.
- **Child processes:** temporary credential files with restrictive permissions (0600, or the service account's ACL on Windows), deleted afterwards and cleaned up after crashes.
- **Honest limits:** Go's garbage collector cannot guarantee secrets are wiped; buffers we control are cleared, and the security doc says so.
- **Journal** records which credential reference an operation used, never its value.
- No secret may ever appear on a command line or in logs, journal entries or notifications.

### 8.4 CI and automation

- CI hands secrets over as environment variables, consumed through `env:` references.
- Cloud access SHOULD use OIDC (short-lived credentials via the default chain), not stored keys.
- Give CI the least powerful key slot that does the job: backup-only workflows get an agent slot; verify and restore workflows get a full slot behind a protected environment with required approval.
- The operations guide includes a worked GitHub Actions example of a weekly `verify` job.

### 8.5 Storage-side protection

- Agents use write-only storage credentials; deletion requires separate prune credentials.
- S3 Object Lock or append-only mode is supported for ransomware resistance.
- HTTPS for all remote storage and notifications; TLS required to databases on separate hosts.

## 9. Non-functional requirements

The non-functional bar is set by the brief's constraints: efficient on large databases, low impact on the server, secure, reliable, cross-platform and well documented.

| Area | Requirement |
| --- | --- |
| Memory | Constant regardless of database size; everything streams; no temporary dump files |
| Database impact | Throttled I/O and bounded concurrency; overhead on throughput and p99 latency measured and published; snapshots read without locking (base backups, consistent `mongodump --oplog`) |
| Recovery point | Seconds while the agent runs, bounded capture lag under load; the actual window always reported |
| Recovery time | Undoing a drop restores only the affected objects, not the whole database |
| Crash safety | Snapshots commit only after a clean finish; atomic local writes; duplicate segments harmless; prune crashes leave only leftovers; resume from repository checkpoints |
| Availability of capture | Lease-based single capturer per target; standby takeover after lease expiry or immediate on graceful release; `catch-up` when no agent runs |
| Integrity | Per-chunk hashes and AEAD tags; rebuildable index; `check` and `verify` |
| Portability | Single static binary, pure Go (no cgo); Linux, macOS, Windows on amd64 and arm64 |
| Compatibility | Versioned repository format; binaries refuse newer formats; golden repositories keep old versions readable; per-version WAL handling for Postgres 16–18 and MongoDB 7.0–9.0 |
| Usability | `init` usable in about two minutes; quick start to a first backup in under five minutes; clear, specific error messages; `--help` with examples on every command |
| Automation | `--json` on every command, stable exit codes, Prometheus metrics, JSON progress events |
| Security | Section 8 in full |
| Documentation | README (pitch, quick start, demo GIF, "By the numbers"), `docs/architecture.md`, `docs/repository-format.md`, `docs/crypto.md` (design and threat model), operations guide (deployment setups, minimal roles, Object Lock, GitHub Actions example, troubleshooting), `SECURITY.md`, `CONTRIBUTING.md` |

### 9.1 Defaults

Every value below is configurable. None of them can lose data or report false coverage if set badly; at worst it delays something or costs a little more.

**Leases and liveness**

| Setting | Default | Why |
| --- | --- | --- |
| Lease length | 2 min | Rides out slow storage calls; a slow takeover loses nothing because the slot or oplog retains the data |
| Lease renewal | every 30 s | Four chances to renew per lease length |
| Standby check | every 15 s | Notices expiry within about 15 s |
| Holder steps down | after 90 s without a successful renewal | The holder stops capturing before a standby can take over, so two agents never write at once |
| Heartbeat | every 30 s |  |
| `status` stale agent | warning at 5 min, critical at 15 min since the last heartbeat |  |

- Lease expiry MUST be judged on the standby's own monotonic clock (the lease object unchanged for the lease length), never by comparing timestamps across hosts.
- Each lease carries a generation number, and checkpoint writes carry it, so an agent holding an outdated lease cannot overwrite the new holder's checkpoint.

**Recovery point**

| Setting | Default | Why |
| --- | --- | --- |
| Partial WAL segment upload | every 10 s when new WAL exists | A 16 MB segment can take hours to fill on a quiet database; this keeps the recovery point to seconds |
| MongoDB oplog batch flush | 10 s, 16 MB or 10,000 entries, whichever first | Same recovery point as Postgres |
| Stored position reported to Postgres | every 10 s and after each confirmed upload | Lets the slot free WAL promptly |
| Catalog timeline refresh | every 30 s and at each base backup | A table created and dropped between refreshes is reported as an unknown table; the event is still detected |
| Canary update (opt-in) | every 60 s |  |

The 10-second uploads cost about 8,600 PUT requests a day per target, roughly $0.04 a day on S3.

**Repository safety**

| Setting | Default | Why |
| --- | --- | --- |
| Lock refresh | every 5 min | Long operations keep their locks fresh |
| Stale lock | 30 min without a refresh | A crashed process cannot block prune forever |
| Pack safety margin | the larger of 24 h and 2× the longest backup run observed | A flat 24 h is unsafe if a full backup takes longer than that |
| Prune last-good rule | refuse if the last 3 backups failed |  |

**Scheduling**

| Setting | Default | Why |
| --- | --- | --- |
| Missed-run grace window | the smaller of 6 h and half the interval | A flat 6 h makes no sense for hourly jobs |
| Jitter | the smaller of 5 min and 10% of the interval, a fixed offset per target | Predictable and testable with a fake clock |
| Dead-man's switch | 1.5× the interval, and at least interval + 1 h | Avoids alerts after one slow run of a frequent job |
| Simultaneous heavy jobs per agent | 1 | Protects the database host |

**Shutdown and time limits**

| Setting | Default |
| --- | --- |
| Graceful shutdown deadline | 30 s |
| systemd `TimeoutStopSec` | 45 s |
| Kubernetes `terminationGracePeriodSeconds` | 45 s (Kubernetes' own default of 30 s would cut the final flush short) |
| `verify` time limit per run | 6 h |
| Retry backoff (storage, database) | 1 s up to 5 min, with jitter |

**Alert thresholds**

| Condition | Warning | Critical |
| --- | --- | --- |
| Capture lag | 5 min | 15 min |
| Retained slot WAL, `max_slot_wal_keep_size` set | 50% of it | 80% of it |
| Retained slot WAL, limit not set | 10 GB | 50 GB, and `doctor` recommends setting the limit |
| Oplog window | under 24 h | under 6 h, or capture lag over half the window |

Notifications: a condition must hold for 2 min before alerting and be clear for 5 min before a resolved message; repeats are grouped for 1 h; destructive events and verify failures alert immediately with no hold.

**Data path**

FastCDC chunks 256 KiB minimum, 1 MiB average, 4 MiB maximum; packs 16 MiB; compression skipped below 3% savings; zstd default level; retention defaults as in section 7.10.

## 10. Testing, benchmarks and licensing

Testing is weighted towards correctness under failure, and every published number must be measured and reproducible.

### 10.1 Testing strategy

| Layer | Covers | Tools |
| --- | --- | --- |
| Unit | Chunker determinism and bounds; encryption round trips, tamper and wrong-key failures, an agent slot cannot decrypt; retention (GFS, anchors, pins, last-good rule); scheduler with a fake clock (overlap, missed runs, jitter, DST); event classifier (drop vs truncate vs rewrite); config, secret references, redaction | `testing`, table-driven tests |
| Fuzzing and properties | Every byte parser (packs, index, WAL records seeded from real WAL per Postgres version, oplog, metadata) never panics; chunk reassembly equals input; model-based repository tests (random backup, prune, pin, crash, restart sequences vs an in-memory model: committed snapshots restorable, referenced chunks never deleted, `check` passes) | Go fuzzing, `pgregory.net/rapid` |
| Backend conformance | Same suite for every backend, plus a fault-injecting wrapper (latency, errors, cut uploads, crash at call N): conditional create, range reads, large listings, concurrent writers | Local, MinIO |
| Database integration | Postgres 16/17/18 × MongoDB 7.0/8.0/9.0; restore equality via deterministic per-table checksums; PITR to xid and time; events including false positives (`VACUUM FULL`, `CLUSTER`, rewriting `ALTER TABLE`); extraction; forced oplog rollover and slot invalidation; wrong tool versions | `testcontainers-go` |
| Crash and chaos | `SIGKILL` at random points (mid-chunk, mid-upload, mid-index commit, mid-prune); network faults; disk full; lease races and clock skew | Subprocess tests, toxiproxy |
| CLI end-to-end | Real commands, expected output and exit codes; the v1 demo as an automated test | `testscript` (`rogpeppe/go-internal`) |
| Soak | Hours under pgbench load with random kills: flat memory, bounded lag, stable goroutines | Scheduled job |
| Compatibility | Golden repositories from each release must stay readable; WAL corpus per Postgres version | Fixtures in `testdata/` |

Applied everywhere: `go test -race`, `goleak`, a secret-leak test (run with known fake secrets and scan logs, journal and notifications), `staticcheck`, `golangci-lint`, `govulncheck`, `gosec`, a dependency license check (`go-licenses`) and a DCO check.

### 10.2 CI pipelines

| Pipeline | Contents | Target |
| --- | --- | --- |
| Every PR | Lint, unit, race, short fuzzing, conformance (local and MinIO), CLI tests, integration subset (Postgres 17, MongoDB 8.0) | Under 15 minutes |
| Nightly | Full version matrix, chaos, long fuzzing, soak, golden repositories | Hours |
| Release | Everything above, real AWS S3 and R2, service install on Linux, macOS and Windows | Gates the release |

Unit, local-backend and CLI tests run on Linux, macOS and Windows; database integration runs on Linux; Windows-specific behaviour (rename retries, named pipes, service install) has dedicated tests. Locally: `make test` and `make test-integration` (Docker Desktop with WSL2).

### 10.3 Benchmarks

| # | Metric | Headline |
| --- | --- | --- |
| 1 | Peak memory vs database size (10, 50, 100 GB) | Yes |
| 2 | Storage after 30 days of daily snapshots vs 30 naive dumps | Yes |
| 3 | Time to undo a drop vs a full restore | Yes |
| 4 | Database overhead: pgbench TPS and p99 latency with agent off, capturing, and during base backup (throttled and not) | Yes |
| 5 | Capture lag p50/p99 at different transaction rates | Yes |
| 6 | Backup throughput and CPU per compression level |  |
| 7 | Time from DROP commit to alert |  |
| 8 | Full and point-in-time restore time |  |
| 9 | Prune and GC time vs repository size |  |
| 10 | Verify duration per level |  |

MongoDB gets the equivalent set.

- **Comparisons:** first against the baseline (`pg_dump` / `mongodump` plus gzip plus upload on cron); then pgBackRest, WAL-G and Percona Backup for MongoDB as reference points, configured per their docs, configs published, corrections invited.
- **Datasets:** pgbench at scales reaching about 10, 50 and 100 GB with seeded daily churn (2–5% of rows); a seeded MongoDB document generator with YCSB-style mixes.
- **Method:** fixed, documented hardware (named instance type, local NVMe, S3 in the same region); at least three runs, median with min and max; cold and warm where relevant; scripts, configs and raw CSV/JSON in `bench/`; `make bench` reproduces; limitations stated.
- **Microbenchmarks** (chunker, BLAKE3, zstd, encryption, pack encoding) in CI compared with `benchstat`; regressions beyond about 10% flag the PR. **Macro benchmarks** weekly and before releases, with history kept.
- **README "By the numbers"** shows the five headline metrics. No number is published before it is measured.

### 10.4 License

- **Apache 2.0**, chosen over MIT for its explicit patent grant and patent-retaliation clause (relevant to original designs such as write-only encryption, keyed dedupe, WAL event detection and lease failover), its explicit exclusion of trademark rights (protecting the Pentymento name), and clear contribution terms. Accepted trade-offs: incompatibility with GPLv2-only projects and small redistribution obligations.
- `LICENSE` and `NOTICE` at the root; SPDX header in every source file.
- Contributions via the DCO (`Signed-off-by:`), enforced in CI; no CLA.
- CI fails if a dependency with an incompatible license is added.

## 11. Delivery

v1 is delivered in seven milestones and is done only when every item of the checklist below is ticked.

### 11.1 Milestones

| # | Milestone | Done when |
| --- | --- | --- |
| M0 | Skeleton | Repo layout, CLI framework, config, CI with Postgres 16/17/18 and MongoDB matrix |
| M1 | Repository core | Chunk store, encryption, packs and index, locks; local backend; property and crash tests pass (engine-agnostic) |
| M2 | Snapshots | `backup` and `restore` (full) for both engines; engine interface designed against two real implementations; S3 backend; constant-memory streaming |
| M3 | Continuous capture | Postgres WAL streaming and MongoDB oplog tailing; survive `kill -9`, resume without gaps; gap detection for both; slot-lag warnings |
| M4 | Point-in-time restore | Side-instance restore and extraction for both (`--at`; Postgres `--xid`; MongoDB collection extraction); `--mode=rewind` |
| M5 | Events | Postgres commit scanning and catalog timeline; MongoDB drop, dropDatabase, rename and bulk-delete detection; `events`, `--before-event` |
| M6 | Trust and operations | `verify` for both engines, `check`, scheduler, retention and prune, notifications, metrics, docs, benchmarks |

### 11.2 Definition of done for v1

**Postgres (16, 17, 18)**

- [ ] `init` creates an encrypted repository with at least two key slots (or a paper key) and an agent slot
- [ ] Native `BASE_BACKUP` streams into the chunk store with constant memory; `pg_basebackup` fallback works
- [ ] The agent streams WAL through a physical slot, survives `kill -9`, resumes without gaps and reports gaps it cannot fill
- [ ] Commit scanning and the catalog timeline classify DROP TABLE, DROP DATABASE and TRUNCATE correctly; rewrites are not reported as drops
- [ ] `restore --at`, `--xid`, `--before-event` into a side instance, then extraction under a safe name; `--mode=rewind` works when chosen
- [ ] Slot-lag monitoring, warnings and critical alerts work

**MongoDB (7.0, 8.0, 9.0, replica sets)**

- [ ] `mongodump --oplog` snapshots into the chunk store
- [ ] The agent tails the oplog, survives `kill -9` and detects rollover gaps
- [ ] Events: drop, dropDatabase, rename, bulk delete
- [ ] `restore --at`, `--before-event` into a side instance (`--oplogReplay --oplogLimit`), then collection extraction under a safe name

**Shared**

- [ ] Local and S3-compatible backends with conditional-create detection and fallback
- [ ] Hybrid storage, zstd, write-only encryption
- [ ] Locks, leases, two-agent failover, `catch-up`
- [ ] Scheduler (no overlaps, missed runs, jitter, dead-man's switch), retention with anchors and safety rules, two-phase prune
- [ ] `verify` (quick, standard, deep), `check`, `doctor` including the over-privilege check
- [ ] Notifications: HMAC-signed webhook, Slack, heartbeat pings, deduplication and resolved messages
- [ ] Observability: JSON logs, Prometheus metrics, journal, `history`, `debug bundle`
- [ ] `agent install` on Linux, macOS and Windows; container image; example Kubernetes manifests
- [ ] Control socket: `status --live`, `agent reload`, `backup --now`
- [ ] Secret providers `env:`, `file:`, `keychain:`, `cmd:`; no secrets on command lines or in logs
- [ ] `--json` and stable exit codes on every command

**Quality gates**

- [ ] All testing layers green across PR, nightly and release pipelines
- [ ] Model-based repository tests and kill tests green nightly for two consecutive weeks
- [ ] Secret-leak test, `govulncheck`, `gosec` and the license check pass
- [ ] A v1.0 golden repository committed for future compatibility tests
- [ ] No known bug in the categories data loss, silent gap or false "verified"

**Documentation**

- [ ] README with pitch, quick start under five minutes, demo GIF and "By the numbers"
- [ ] `docs/architecture.md`, `docs/repository-format.md`, `docs/crypto.md`, operations guide
- [ ] `--help` with examples on every command
- [ ] `SECURITY.md`, `CONTRIBUTING.md` (DCO), `LICENSE` and `NOTICE`

**Benchmarks**

- [ ] Five headline metrics measured on documented hardware (at least three runs each), published with raw data
- [ ] Baseline comparison done; reference comparisons published with their configs
- [ ] `make bench` reproduces the numbers

**The demo (automated test and recorded video)**

- [ ] `pentym init`, `agent install`, pgbench load plus a MongoDB load generator
- [ ] Act 1: `DROP TABLE pgbench_accounts`; the Slack alert arrives with the recovery command; `restore --before-event` brings it back while later writes to other tables are untouched
- [ ] Act 2: `db.orders.drop()`; same flow, same commands
- [ ] `status` shows the recovery windows; `verify --level standard` passes; headline numbers shown at the end

**Release**

- [ ] Signed binaries for Linux, macOS and Windows (amd64 and arm64) via GoReleaser, with checksums
- [ ] Multi-architecture container image
- [ ] `go install`, a Homebrew tap and a Scoop bucket
- [ ] Tagged `v1.0.0` with a changelog
- [ ] A launch write-up on the design

### 11.3 Explicitly out of scope for v1

MySQL, SQLite, GCS and Azure, sharded MongoDB, Postgres logical-decoding DML detection, the optional event trigger, pack compaction, `archive_command` mode, `pentym copy`, a control plane, UI, SSO, access roles, OpenTelemetry traces, and PITR on managed databases.

### 11.4 Roadmap after v1

| Release | Adds |
| --- | --- |
| v1.x | Postgres logical-decoding DML detection; optional DDL event trigger |
| After v1 (not yet scheduled) | Pack compaction; `archive_command` mode; GCS, Azure, SFTP, rclone; `pentym copy`; more notification channels; native Vault and cloud KMS |
| v2 | MySQL; optional `pentym server` control plane; OpenTelemetry tracing once the server exists |
| v3 | SQLite; access roles, SSO, UI, restore approvals |
| Later idea | Event detection and single-table extraction on managed databases through a logical-replication slot |

### 11.5 Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| A dead agent's slot fills the primary's disk | `max_slot_wal_keep_size`, slot-lag metrics, warning and critical alerts |
| Oplog rolls over during agent downtime | Window monitoring, rollover detection, `catch-up`, forced new base backup |
| WAL format changes between Postgres majors | Support only community-supported versions; per-version handling; WAL corpus tests |
| Catalog timeline inaccuracy (TOAST, indexes, changes between refreshes) | Oid-based classification; false-positive integration tests |
| Concurrent writers corrupt the repository | Shared and exclusive locks, leases, pack safety margin, model-based and kill tests |
| Provider lacks conditional writes | Detected at `init` and `doctor`; weaker fallback with a clear warning |
| Network filesystems break atomic creates | `doctor` warning |
| Lost private key | Two slots or a paper key required; slot checks in `verify`, `check`, `status` |
| Users believe they are covered when they are not | Actual window in `status`, heartbeat staleness, dead-man's switch, external pings |
| Scope creep | Core first, Postgres and MongoDB only, explicit out-of-scope list |
| Windows filesystem quirks | Rename retries, Windows-specific tests |
| Index loss | Rebuildable from packs |
| False "verified" | L3 recovery-point checks; verified marker only after passing |

### 11.6 Decision log

| # | Decision | Outcome |
| --- | --- | --- |
| 1 | Agent or one-shot CLI | One binary, both modes; repository is the source of truth |
| 2 | Postgres "before this statement" | Timestamp/xid/LSN plus commit-record scanning and events in v1; logical decoding and event trigger later; side-instance-and-extract restore by default |
| 3 | Storage format | Hybrid: chunk store for snapshots, plain objects for logs |
| 4 | Target user | Designed for teams, built for solo developers first, self-hosted focus, 11 growth seams |
| 5 | v1 scope | Postgres and MongoDB (MongoDB from M2); then MySQL, SQLite |
| 6 | CLI and agent communication | Repository for correctness, local socket for convenience, lease liveness, `catch-up` |
| 7 | Shell out or native | Native capture, detection and Postgres base backup; shell out for dump, restore and replay; instance providers |
| 8 | Scheduling | Built-in scheduler in the agent; one-shot commands still cron-friendly |
| 9 | Running as a service | Self-installing service plus container and Kubernetes; deployment setups |
| 10 | Encryption | Asymmetric, write-only agents, key slots |
| 11 | Compression | zstd with chunk-first, skip-incompressible and no-source-compression rules |
| 12 | Credentials | Secret references, per-role credentials, least privilege, `Secret` type |
| 13 | Retention | GFS plus PITR window with anchors, safety rules, two-phase prune |
| 14 | Backends | Local and S3-compatible in v1 |
| 15 | Verify | Levels L0–L5 with quick, standard and deep presets |
| 16 | Notifications | Webhook, Slack, heartbeat pings; actionable, routed, deduplicated |
| 17 | Observability | slog logs, Prometheus metrics, journal; OpenTelemetry later |
| 18 | License | Apache 2.0 |
| 19 | Testing | Failure-focused, eight layers, three CI pipelines |
| 20 | Benchmarks | Five headline metrics, baseline first, reproducible |
| 21 | Definition of done | Section 11.2 |

### 11.7 Open items

- [x] Confirm the GitHub repository name, pkg.go.dev path and domain for Pentymento / `pentym` are free
- [ ] Check whether Postgres 19 is released at kickoff and add it to the matrix if so
- [x] Verify `pglogrepl` base-backup support against Postgres 16, 17 and 18
- [x] Confirm default values (section 9.1)
