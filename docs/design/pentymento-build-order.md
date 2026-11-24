# Pentymento — Build Order (v1)

Nov 24, 2026 · @Toluwalase Akinyemi

## 1. Purpose

This document splits Pentymento v1 into 35 chunks and fixes the order in which they are built. It is written for an AI coding agent: build one chunk at a time, in order, and do not start a chunk until every chunk it depends on is complete. It contains no dates or durations.

### 1.1 Source documents

This order adds no requirements of its own. Everything a chunk builds is specified in:

| Document | Abbreviation | Governs |
| --- | --- | --- |
| [PRD (v1)](https://claude.ai/artifact/F82YZzwaeTWprEJJ5VsjXP) | PRD | What must be built, defaults (9.1), testing (10), definition of done (11.2) |
| [Software Design Document (v1)](https://claude.ai/artifact/3H5XENx5KYN8YBcveLCRJf) | SDD | How it is built: components, interfaces, formats, flows, coordination, source layout (3.5) |
| [Brand Guidelines](https://claude.ai/artifact/3eHMbQ4C1iKhx9SfRrQDtx) | Brand | Names and identifiers, CLI output, message writing, docs style, glossary, assets |

References such as "PRD 7.3" or "SDD 6.2" point to section numbers in these documents.

### 1.2 Precedence

1. If this order and a source document disagree, the **source document** wins.
2. Between source documents: the **PRD** wins on *what*, the **SDD** on *how*, the **Brand Guidelines** on names, wording and presentation.
3. Anything still unclear is a question for the maintainer (section 2.5), never a guess.

### 1.3 Chunk format

Every chunk below has the same parts:

- **Goal:** what exists when the chunk is done.
- **Depends on:** chunks that must be complete first.
- **Read:** the source sections to follow.
- **Build:** the concrete work.
- **Tests:** what must be tested in this chunk.
- **Done when:** the acceptance criteria; all must hold before moving on.

## 2. Rules for the building agent

These rules apply to every chunk.

### 2.1 Scope

- Build exactly what the chunk and its source sections specify. Do not add features, flags, dependencies, engines or backends that the source documents don't name.
- Everything in PRD 3.2 and 11.3 (out of scope for v1) stays out, even if it looks easy to add.
- Defaults come from PRD 9.1. Don't invent new values; if a value is missing, ask (2.5).
- Use the source layout in SDD 3.5. New packages need a reason tied to a component in SDD 3.

### 2.2 Quality gates (every chunk)

- `go build`, `go vet`, `golangci-lint`, `staticcheck` clean.
- `go test -race ./...` passes, with `goleak` where goroutines are started.
- The chunk's own tests (unit, fuzz, property, integration as listed) are written in the same chunk, not later.
- CI (from C02 onward) is green before the chunk is considered done.
- Never delete, skip or weaken an existing test to make a chunk pass. A failing earlier test means the chunk is not done.
- No secret ever appears in logs, errors, command-line arguments, journal entries or notifications (PRD 8.3); once C04 exists, the secret-leak test runs on every chunk.

### 2.3 Conventions

- Go, pure Go only (no cgo). Module path `github.com/HakeemTheEmperor/pentymento`.
- Every source file starts with the Apache 2.0 SPDX header and copyright line (PRD 10.4).
- Commits follow the Go project style: a subject of the form \<package path>: \<lowercase summary> (for example internal/repo: commit snapshot only after a clean finish), at most about 72 characters, no trailing full stop, where the prefix is a real directory or all: / docs: for cross-cutting changes; then a blank line, a body explaining why, references such as Fixes #42, and a DCO `Signed-off-by:` line.
- Names, identifiers, CLI output and messages follow Brand 2, 10 and 11; terms follow the glossary in Brand 12.3.
- Every error or warning a user can act on gets a `PTM-` message ID registered in the message registry with a docs entry (Brand 11.3).
- Every new command has `--help` with examples, `--json` output and stable exit codes (PRD 7.15).
- Use the libraries the source documents name (for example `pglogrepl`, the MongoDB Go driver, `aws-sdk-go-v2`, `klauspost/compress`, `zeebo/blake3`, `golang.org/x/crypto`, `filippo.io/age`, `kardianos/service`, `testcontainers-go`). Never implement cryptographic primitives.

### 2.4 Working method

1. Read the chunk's **Read** sections in full before writing code.
2. Build, test, and check every **Done when** item.
3. Update the docs the chunk touches (reference docs, message entries) in the same chunk.
4. Report what was built, what was tested, and any open questions, then move to the next chunk.

### 2.5 When to stop and ask the maintainer

- The source documents are silent, ambiguous or contradictory on something the chunk needs.
- A specified library or approach turns out not to work (for example a protocol detail differs across Postgres or MongoDB versions).
- A chunk would require changing the repository format, a public command, a default value, or anything in another chunk's scope.
- A test can only pass by weakening a safety rule.

When asking, state the problem, the options and a recommendation, and do not proceed on that point until answered.

## 3. Chunk overview

The 35 chunks follow the PRD milestones M0–M6 (PRD 11.1). Build them in ID order; the dependency column shows what each one needs. MongoDB enters at M2 so that two real engines shape the engine interface (PRD 6).

| ID | Chunk | Phase | Depends on |
| --- | --- | --- | --- |
| C01 | Repository scaffold | M0 | — |
| C02 | CI pipelines | M0 | C01 |
| C03 | CLI framework and output | M0 | C01 |
| C04 | Config and secrets | M0 | C03 |
| C05 | Telemetry foundations | M0 | C03 |
| C06 | Backend interface, local backend, conformance suite | M1 | C02 |
| C07 | Chunker, hashing, compression | M1 | C02 |
| C08 | Crypto and key slots | M1 | C04 |
| C09 | Repository format | M1 | C05, C06, C07, C08 |
| C10 | Locks and leases | M1 | C09 |
| C11 | `init`, `check`, index rebuild, `repo upgrade` | M1 | C10 |
| C12 | Engine interface, tool runner, instance providers | M2 | C11 |
| C13 | Postgres snapshots | M2 | C12 |
| C14 | MongoDB snapshots | M2 | C12 |
| C15 | S3 backend | M2 | C11 |
| C16 | Full restore and `dump` | M2 | C13, C14 |
| C17 | Agent runtime and control socket | M3 | C05, C12 |
| C18 | Postgres WAL capture | M3 | C13, C17 |
| C19 | MongoDB oplog capture | M3 | C14, C17 |
| C20 | `catch-up` and `status` | M3 | C18, C19 |
| C21 | Postgres point-in-time restore and extraction | M4 | C16, C18 |
| C22 | MongoDB point-in-time restore and extraction | M4 | C16, C19 |
| C23 | Postgres event detection | M5 | C18 |
| C24 | MongoDB event detection | M5 | C19 |
| C25 | `events` and `--before-event` | M5 | C21, C22, C23, C24 |
| C26 | Verify | M6 | C21, C22 |
| C27 | Scheduler | M6 | C17 |
| C28 | Retention and prune | M6 | C20, C26 |
| C29 | Notifications | M6 | C20, C25, C26, C27 |
| C30 | Observability completion | M6 | C29 |
| C31 | Service installation and deployment files | M6 | C17 |
| C32 | `init` and `doctor` completion | M6 | C28, C31 |
| C33 | Documentation and brand assets | M6 | C32 |
| C34 | Benchmarks | M6 | C32 |
| C35 | Demo test, soak and release | M6 | C30, C33, C34 |

A phase is complete when all its chunks are done and the matching milestone criterion in PRD 11.1 holds.

## 4. Phase M0 — Skeleton

M0 produces an empty but correctly structured project: layout, CI with the database matrix, the CLI frame, config, secrets and logging.

### C01 — Repository scaffold

- **Goal:** the repository exists with the agreed layout, licensing and project files.
- **Depends on:** —
- **Read:** SDD 3.5; PRD 10.4; Brand 13, 15.
- **Build:**
  - `go.mod` with module path `github.com/HakeemTheEmperor/pentymento`.
  - Directory layout from SDD 3.5 (`cmd/pentym/`, `internal/…`, `docs/`, `bench/`, `testdata/`, `deploy/`), with package stubs.
  - `LICENSE` (Apache 2.0), `NOTICE` (copyright Toluwalase Akinyemi), SPDX header on every Go file.
  - `CONTRIBUTING.md` (Go-style commit messages and DCO sign-off, with examples), `SECURITY.md` (`security@pentymento.dev`), `TRADEMARKS.md` (Brand 15).
  - `Makefile` targets: `test`, `test-integration`, `bench`, `lint`.
  - Linter configuration for `golangci-lint` and `staticcheck`.
- **Tests:** a trivial test so `make test` runs.
- **Done when:** `go build ./...` and `make test` pass on Linux, macOS and Windows; every file has its header.

### C02 — CI pipelines

- **Goal:** the PR, nightly and release pipelines exist, with the database version matrix running.
- **Depends on:** C01
- **Read:** PRD 10.1, 10.2; SDD 13.1.
- **Build:**
  - **PR pipeline:** lint, unit, `-race`, DCO and commit-message check (Go-style subject prefix matching a real directory or all:/docs:, lowercase summary, length limit, Signed-off-by line; implemented without Node tooling), dependency licence check (`go-licenses`), `govulncheck`, `gosec`; unit tests on Linux, macOS and Windows.
  - **Integration harness** using `testcontainers-go`: a smoke test that starts Postgres 16, 17, 18 and MongoDB 7.0, 8.0, 9.0 (as a single-node replica set) and connects. PR runs Postgres 17 and MongoDB 8.0; nightly runs the full matrix.
  - **Nightly and release pipelines** as empty jobs, filled by later chunks.
  - Benchmark job placeholder for `benchstat` comparison.
- **Tests:** the smoke tests above.
- **Done when:** a PR runs the PR pipeline under 15 minutes; nightly starts all six database versions successfully.

### C03 — CLI framework and output

- **Goal:** `pentym` exists as a command tree with the house output style and the message registry.
- **Depends on:** C01
- **Read:** PRD 7.15; Brand 2, 10, 11.
- **Build:**
  - Root command, `--version`, `--help` with examples, subcommand stubs for every command in PRD 7.15 returning "not implemented".
  - Global flags: `--json`, `--color=auto|always|never`, `--ascii`, `-q`, `-v`, `--debug`, `--log-format`; `NO_COLOR` support; colour only on a TTY.
  - Output helpers: status symbols with ASCII fallback, aligned borderless tables, units (KiB, MiB, GiB), durations, local times with zone abbreviation, RFC 3339 UTC in JSON, the `→` next-step line, stdout/stderr split.
  - Stable exit-code table.
  - Message registry (`PTM-` IDs, short name, docs link) and the CI check that every ID has a `docs/errors/<ID>` entry and none is reused.
  - Confirmation-prompt helper (default No; typed-name confirmation for the most destructive operations).
- **Tests:** golden-output tests for tables, symbols, colour on/off, ASCII fallback and JSON; registry check.
- **Done when:** every PRD 7.15 command appears in `--help`; output matches Brand 10; the registry check runs in CI.

### C04 — Config and secrets

- **Goal:** configuration with multiple named targets and secrets held only as references.
- **Depends on:** C03
- **Read:** PRD 7.15 (config), 8.3; SDD 4.4, 8.5.
- **Build:**
  - YAML config loader: multiple named targets from day one, includes, environment overrides, validation with clear messages.
  - Secret providers `env:`, `file:`, `keychain:`, `cmd:`; resolution at the moment of use.
  - Plaintext secrets rejected unless `allow_plaintext_secrets: true` and the file isn't group- or world-readable.
  - The `Secret` type: `String()`, `Format`, `MarshalJSON`, `slog.LogValuer` return `[redacted]`.
  - Error scrubbing helper for driver errors.
  - The secret-leak test harness (run jobs with known fake secrets and scan all output), added to CI.
- **Tests:** each provider; plaintext rules; redaction through every formatting path; leak harness.
- **Done when:** no code path prints a `Secret`; the leak test runs on every PR.

### C05 — Telemetry foundations

- **Goal:** structured logging, metrics registry and progress reporting that later chunks plug into.
- **Depends on:** C03
- **Read:** PRD 7.13; SDD 10.1–10.3.
- **Build:**
  - `slog` setup: JSON for services, text in a terminal; fields `target`, `job`, `run_id`, `component` on every line.
  - Prometheus registry with the metric names from PRD 7.13 declared (values wired by later chunks); labels per target and job only.
  - Progress renderer: stderr progress line on a TTY, periodic lines otherwise, JSON event stream with `--json`.
  - A small internal interface around logging and metrics so tracing can be added later.
- **Tests:** log field presence, JSON format, progress modes.
- **Done when:** a stub command can log, emit a metric and show progress in all three modes.

## 5. Phase M1 — Repository core

M1 builds the engine-agnostic heart of Pentymento: storage, the data pipeline, encryption, the repository format and coordination. Nothing in M1 talks to a database.

### C06 — Backend interface, local backend, conformance suite

- **Goal:** the storage abstraction and a crash-safe local implementation, proven by a reusable suite.
- **Depends on:** C02
- **Read:** PRD 7.8; SDD 4.1.
- **Build:**
  - The `Backend` interface exactly as in SDD 4.1, including `Capabilities()`.
  - Local backend: temp file, `fsync`, rename, directory `fsync`; `O_CREAT|O_EXCL` conditional create; rename retries on Windows; `data/<xx>/` fan-out; network-filesystem detection hook for `doctor`.
  - Backend conformance suite: conditional create, range reads, listing large prefixes, concurrent writers, interrupted uploads.
  - Fault-injecting wrapper: latency, errors, cut uploads, crash at call N.
- **Tests:** the conformance suite against the local backend, with and without faults, on all three OSes.
- **Done when:** the suite passes on Linux, macOS and Windows and is ready to run against any future backend.

### C07 — Chunker, hashing, compression

- **Goal:** the first stages of the data pipeline.
- **Depends on:** C02
- **Read:** PRD 7.1, 7.7; SDD 5.3, 6.1 (steps 3–5).
- **Build:**
  - FastCDC chunker: 256 KiB minimum, 1 MiB average, 4 MiB maximum (PRD 9.1).
  - Keyed BLAKE3 chunk IDs over raw content (`zeebo/blake3`).
  - zstd compression (`klauspost/compress`), default level, levels `fastest | default | better | best`; one-byte header `none`/`zstd`; store raw when saving is under 3%.
  - Parallel worker pool with a configurable bound.
- **Tests:** chunker determinism and bounds; boundary stability under insertion; property test that reassembly equals input; fuzzing of decoders; compression round trips and the skip rule.
- **Done when:** all properties hold; microbenchmarks exist for chunker, BLAKE3 and zstd.

### C08 — Crypto and key slots

- **Goal:** the write-only encryption scheme and key management.
- **Depends on:** C04
- **Read:** PRD 8.1, 8.2; SDD 8.1–8.4.
- **Build:**
  - Key types: X25519 repository key pair, metadata key, chunk-ID key, per-pack and per-segment data keys.
  - XChaCha20-Poly1305 with random 24-byte nonces; data-key wrapping to the public key (X25519, HKDF, AEAD, as `age` does).
  - Key slots: passphrase (Argon2id), age identity (`filippo.io/age`), agent slot (metadata key, chunk-ID key, public key only).
  - Slot operations backing `key add`, `key list`, `key remove`, `key export --paper`; re-wrap logic for `key rotate`.
  - `--insecure-test-mode` that refuses non-local backends.
- **Tests:** round trips; tamper and wrong-key failures; **an agent slot cannot decrypt any data**; slot add/remove; rotation re-wraps without touching ciphertext.
- **Done when:** every test above passes and only `golang.org/x/crypto`, `filippo.io/age` and `zeebo/blake3` primitives are used.

### C09 — Repository format

- **Goal:** the on-disk repository: every object type, the pack and index machinery, and the snapshot write path.
- **Depends on:** C05, C06, C07, C08
- **Read:** PRD 7.1; SDD 5.1–5.6, 6.1.
- **Build:**
  - Layout from SDD 5.1 with opaque target IDs; `config` with the format version and the refusal of newer formats.
  - Object types from SDD 5.2: snapshot, index (with wrapped data keys), pack, log segment, timeline history, event, checkpoint, heartbeat, journal entry; encryption per the table.
  - Packer (\~16 MiB packs), index writer and reader, snapshot commit (index first, snapshot last, only after a clean finish).
  - Log-segment writer with naming from SDD 5.4.
  - Journal writer (who, host, operation, target, credential reference, start, end, result).
  - Disposable local metadata cache.
  - A synthetic snapshot path: stream any `io.Reader` through the pipeline into a snapshot and back out.
- **Tests:** write and read every object type; snapshot round trip from an arbitrary stream; constant memory on large streams; partial streams never commit; format-version refusal.
- **Done when:** a multi-GiB synthetic stream round-trips with flat memory, and deduplication is visible on a second, slightly changed stream.

### C10 — Locks and leases

- **Goal:** safe concurrency without any external coordinator.
- **Depends on:** C09
- **Read:** SDD 7.1–7.3; PRD 9.1 (lease and lock defaults).
- **Build:**
  - Shared and exclusive repository locks with holder identity; refresh every 5 min; stale after 30 min.
  - Capture leases: conditional-create acquisition; takeover only after the lease is unchanged for 2 min on the candidate's monotonic clock; renew every 30 s; standby check every 15 s; step-down after 90 s without renewal; graceful release; generation numbers.
  - Generation fencing on checkpoint writes.
  - Fallback write-wait-list protocol when conditional create is unsupported, with leases marked best-effort.
- **Tests:** lock exclusion; stale-lock removal; two-candidate lease races; holder kill and takeover timing; clock-skew tests; fenced stale checkpoint write rejected.
- **Done when:** two agents never hold capture for the same target at once in any test, including with skewed clocks.

### C11 — `init`, `check`, index rebuild, `repo upgrade`

- **Goal:** a user can create, inspect and verify the integrity of a repository.
- **Depends on:** C10
- **Read:** PRD 7.15, 8.2, 7.14 (upgrades); SDD 5.5, 5.6, 6.5 (L0).
- **Build:**
  - `pentym init` (repository part): create the repository, key pair and keys, at least two full slots or one slot plus a paper key, an agent slot; conditional-create test run twice; print results. Deployment-setup questions and role printing come in C32.
  - `pentym check`: L0 integrity (objects exist, AEAD tags and hashes verify, index consistent, a slot unlocks), sampled or full.
  - Index rebuild from packs.
  - `pentym key …` commands wired to C08.
  - `pentym repo upgrade`: the explicit format-migration command. In v1 there is one format, so it reports that the repository is current; it takes the exclusive lock, asks for confirmation and journals.
  - Model-based repository tests (`rapid`): random sequences of write, pin, crash and restart against an in-memory model; prune is added to the model in C28.
- **Tests:** the model-based tests; kill tests mid-index-commit; `check` detecting injected corruption.
- **Done when:** M1's milestone holds — chunk store, encryption, packs and index, locks and the local backend work, and property and crash tests pass.

## 6. Phase M2 — Snapshots

M2 connects the repository to real databases: base backups and full restores for both engines, on local and S3 storage. The engine interface is finalized here against Postgres and MongoDB together.

### C12 — Engine interface, tool runner, instance providers

- **Goal:** the database-side abstractions every engine and restore path uses.
- **Depends on:** C11
- **Read:** SDD 3.2, 4.2, 4.3; PRD 7.2 (shelling out safely), 7.6.
- **Build:**
  - The engine interface covering the responsibilities in SDD 4.2 (capabilities, connection and privilege checks, base-backup stream, capture, gap detection, events, side-instance restore, extraction, rewind, inventory and checks). Draft it now; finalize it while building C13 and C14.
  - Tool runner: no secrets in arguments (temporary `.pgpass` or Mongo `--config` files with restrictive permissions, cleaned up after crashes), streamed stdout, process groups or Windows job objects, context cancellation, captured stderr tagged with `run_id`, exit-code checks, version checks, rejection of source-compression flags.
  - Instance providers: local binaries and Docker, chosen in configured order, major-version match, guaranteed cleanup.
  - `pentym doctor` foundation: tool presence and versions, instance-provider availability, backend capabilities.
- **Tests:** tool runner cancellation and cleanup on all OSes; no secret in any child argument; instance providers start and clean up both engines.
- **Done when:** a side instance of each supported version starts and is cleaned up through both providers.

### C13 — Postgres snapshots

- **Goal:** `pentym backup` and `list` for Postgres.
- **Depends on:** C12
- **Read:** PRD 7.2; SDD 6.1, 4.2 (Postgres column).
- **Build:**
  - Native `BASE_BACKUP` over a replication connection via `pglogrepl` (support verified for 16, 17, 18), streaming the tar straight into the pipeline; fallback to `pg_basebackup`.
  - Snapshot metadata: start and end LSN, timeline, inventory (catalog summary).
  - Shared lock during backup; journal entry; metrics; progress.
  - `pentym list` showing snapshots.
- **Tests:** backups on Postgres 16, 17, 18; constant memory; dedupe on a second backup after small changes; cancellation leaves no committed snapshot.
- **Done when:** a Postgres snapshot can be listed and its stream reassembled (L1) on all three versions.

### C14 — MongoDB snapshots

- **Goal:** `pentym backup` for MongoDB.
- **Depends on:** C12
- **Read:** PRD 7.2; SDD 6.1, 4.2 (MongoDB column).
- **Build:**
  - `mongodump --oplog --archive` to stdout through the tool runner into the pipeline; no `--gzip`.
  - Snapshot metadata: oplog timestamps, inventory (collection summary).
  - Standalone `mongod` detected and reported as snapshots-only.
- **Tests:** backups on MongoDB 7.0, 8.0, 9.0 replica sets and a standalone; dedupe; cancellation.
- **Done when:** MongoDB snapshots can be listed and reassembled on all three versions, and the engine interface now fits both engines without engine-specific branches in callers.

### C15 — S3 backend

- **Goal:** S3-compatible storage behind the same interface.
- **Depends on:** C11
- **Read:** PRD 7.8; SDD 4.1.
- **Build:**
  - `aws-sdk-go-v2` with custom endpoints and the default credential chain; `If-None-Match: *` conditional create; multipart above \~16 MiB with per-part retries; backoff with jitter (1 s to 5 min); bounded concurrency; parallel range reads with read-ahead.
  - Conditional-create detection and fallback.
- **Tests:** the C06 conformance suite against MinIO in `testcontainers-go`, with faults.
- **Done when:** the suite passes on MinIO and a repository works identically on local and S3 storage.

### C16 — Full restore and `dump`

- **Goal:** restore a whole snapshot into a side instance, and export a snapshot as a plain file.
- **Depends on:** C13, C14
- **Read:** SDD 6.4 (steps 3–4); PRD 7.1 (export).
- **Build:**
  - Restore the latest or a chosen snapshot into a side instance for each engine (no log replay yet).
  - `pentym dump <snapshot>` exporting a plain file.
  - Parallel range reads with read-ahead from S3.
- **Tests:** restore equality via deterministic per-table and per-collection checksums; constant memory; restore from S3.
- **Done when:** M2's milestone holds — backup and full restore for both engines on local and S3 storage, with constant-memory streaming.

## 7. Phase M3 — Continuous capture

M3 adds the agent and continuous WAL and oplog capture, so the recovery point becomes seconds, and makes recovery from agent downtime explicit.

### C17 — Agent runtime and control socket

- **Goal:** `pentym agent` runs as a long-lived process with leases, heartbeats, a control socket and safe startup and shutdown.
- **Depends on:** C05, C12
- **Read:** PRD 5.3, 7.14; SDD 9.1, 9.3–9.6.
- **Build:**
  - Process structure from SDD 9.1 under one root context: lease loop per target, heartbeat every 30 s, upload worker pool, control-socket server. Capture loops are plugged in by C18 and C19.
  - Startup sequence (SDD 9.4), including refusing to start if the private key is unlockable while `agent.write_only: true`, and crash-leftover cleanup.
  - Graceful shutdown (SDD 9.5): stop new jobs, abandon uncommitted snapshots, flush hooks for capture, final `state: stopped` heartbeat, lease release, 30 s deadline.
  - Control socket (Unix socket or named pipe, local only, JSON over HTTP): live status, reload without dropping connections, backup-now, health, readiness, metrics; pprof only when enabled. CLI client with fallback to the repository and the "agent not reachable" message.
  - Opt-in TCP health port serving only `/health`, `/ready`, `/metrics`.
- **Tests:** lease handover between two agent processes; graceful shutdown releases the lease; write-only guard; socket fallback.
- **Done when:** two agents for one target run side by side with exactly one holder, and failover works on kill and on graceful stop.

### C18 — Postgres WAL capture

- **Goal:** continuous WAL capture with crash-safe resume and gap detection.
- **Depends on:** C13, C17
- **Read:** PRD 7.3; SDD 6.2 (Postgres); PRD 9.1 (recovery-point and alert defaults).
- **Build:**
  - Physical replication slot, `START_REPLICATION PHYSICAL` via `pglogrepl` from the checkpoint LSN.
  - Completed segments uploaded as objects; partial segment every 10 s when new WAL exists; `.partial` on shutdown.
  - Checkpoint (with lease generation) after each confirmed upload; standby status updates at least every 10 s.
  - Timeline history files kept.
  - Slot monitoring: retained WAL and capture lag against thresholds; recommendation of `max_slot_wal_keep_size`.
  - Gap detection (slot invalidated or WAL removed): record, alert hook, force a base backup.
  - A hook where the event detector (C23) will receive WAL records.
- **Tests:** `kill -9` at random points with no unreported gap; forced slot invalidation reported as a gap; Postgres 16, 17, 18.
- **Done when:** the agent survives kills, resumes without gaps, and warns on slot lag.

### C19 — MongoDB oplog capture

- **Goal:** continuous oplog capture with rollover detection.
- **Depends on:** C14, C17
- **Read:** PRD 7.3; SDD 6.2 (MongoDB).
- **Build:**
  - Tailable cursor on `local.oplog.rs` from the checkpoint timestamp (not change streams).
  - Batches flushed every 10 s, 16 MB or 10,000 entries; checkpoint after each confirmed upload.
  - Rollover detection (checkpoint older than the oldest oplog entry): record the gap, alert hook, force a base backup.
  - Oplog-window monitoring against thresholds.
  - A hook for the event detector (C24).
- **Tests:** kill tests; a deliberately tiny oplog forcing rollover; MongoDB 7.0, 8.0, 9.0.
- **Done when:** capture survives kills and every rollover is reported.

### C20 — `catch-up` and `status`

- **Goal:** users can see their real protection and recover capture without an agent.
- **Depends on:** C18, C19
- **Read:** PRD 5.3, 7.15; SDD 6.2 (catch-up), 7.3, 7.4 (reported window).
- **Build:**
  - `pentym catch-up <target>`: take the lease only if expired, drain what the slot or oplog retains, checkpoint, report gaps, release and exit.
  - `pentym status`: per target, the configured and actual recovery window, agent heartbeat age (warning at 5 min, critical at 15 min), last verify (filled by C26), dead-man check (filled by C27); `--live` through the socket.
  - Auto catch-up from `status` only when `catch_up.auto: true` and the lease has expired; otherwise print the command.
- **Tests:** catch-up after agent downtime fills the window; status shows gaps and stale agents correctly.
- **Done when:** M3's milestone holds — capture for both engines survives kills, resumes without gaps, detects gaps, and warns on slot lag.

## 8. Phase M4 — Point-in-time restore

M4 turns captured logs into restores to any point, using a side instance and extracting only what was lost by default.

### C21 — Postgres point-in-time restore and extraction

- **Goal:** `pentym restore` for Postgres to a time, xid or LSN, extracting tables into production.
- **Depends on:** C16, C18
- **Read:** PRD 7.5; SDD 6.4.
- **Build:**
  - Target resolution: `--at`, `--xid`, LSN. (`--before-event` is added in C25.)
  - Selection of the anchor snapshot and segments; refusal with the exact gap and nearest reachable points when logs are missing.
  - Full key slot unlock; side instance; replay with `recovery_target_time` or `recovery_target_xid` and `recovery_target_inclusive = false`.
  - Extraction: `pg_dump -t` from the side instance, `pg_restore` into production under a safe name (for example `orders_recovered_20261122`) or `--in-place`, preserving indexes, constraints and sequences; uses the restore credential.
  - `--mode=rewind`: restore the whole cluster to the point, with typed-target-name confirmation.
  - Cleanup and journal entry.
- **Tests:** restore to an xid and to a time; expected rows present or absent; extraction keeps indexes and constraints; later writes to other tables untouched; refusal on gaps; Postgres 16, 17, 18.
- **Done when:** a dropped table can be restored to just before a known xid without affecting other tables.

### C22 — MongoDB point-in-time restore and extraction

- **Goal:** `pentym restore` for MongoDB to a time, extracting collections into production.
- **Depends on:** C16, C19
- **Read:** PRD 7.5; SDD 6.4.
- **Build:**
  - `--at` resolution; snapshot and batch selection; gap refusal.
  - `mongorestore --oplogReplay --oplogLimit <ts>` into the side instance.
  - Extraction with `mongodump --nsInclude` and `mongorestore --nsFrom/--nsTo` under a safe name, or `--in-place`.
  - `--mode=rewind` with typed-target-name confirmation.
- **Tests:** restore to a timestamp; collection extraction; untouched later writes elsewhere; MongoDB 7.0, 8.0, 9.0.
- **Done when:** M4's milestone holds — side-instance restore and extraction for both engines, and rewind when chosen.

## 9. Phase M5 — Event detection

M5 finds destructive operations in the captured logs and makes "restore to just before it" a single command.

### C23 — Postgres event detection

- **Goal:** DROP TABLE, DROP DATABASE and TRUNCATE are detected from WAL, and rewrites are not mistaken for drops.
- **Depends on:** C18
- **Read:** PRD 7.4; SDD 6.3 (Postgres).
- **Build:**
  - Commit-record scanning for dropped relfilenodes; database-drop records.
  - Catalog timeline: (oid, relname, relfilenode) refreshed every 30 s and at each base backup, stored per target.
  - Classification: oid gone → DROP; oid kept with new relfilenode → TRUNCATE or rewrite; only DROP TABLE, DROP DATABASE and TRUNCATE reported; TOAST tables and indexes attributed to their parent; unknown relfilenodes reported as tables created after the last refresh.
  - Events written to the repository with xid, LSN and time, and passed to a notification hook (wired in C29).
- **Tests:** every event type detected; `VACUUM FULL`, `CLUSTER` and table-rewriting `ALTER TABLE` **not** reported as drops; a WAL corpus from 16, 17, 18 in `testdata/`; fuzzing of the WAL record parser.
- **Done when:** classification is correct on all three versions with no false drops.

### C24 — MongoDB event detection

- **Goal:** drop, dropDatabase, renameCollection and bulk deletes are detected from the oplog.
- **Depends on:** C19
- **Read:** PRD 7.4; SDD 6.3 (MongoDB).
- **Build:**
  - `op: "c"` commands for drop, dropDatabase and renameCollection.
  - Bulk-delete detection from bursts of `op: "d"` entries on one collection.
  - Events written to the repository with oplog timestamps and passed to the notification hook.
- **Tests:** each event type on MongoDB 7.0, 8.0, 9.0; ordinary deletes not flagged as bulk deletes; oplog parser fuzzing.
- **Done when:** every event type is detected on all three versions.

### C25 — `events` and `--before-event`

- **Goal:** users can list events and restore to just before one.
- **Depends on:** C21, C22, C23, C24
- **Read:** PRD 7.4 (events timeline), 7.5; SDD 6.3, 6.4.
- **Build:**
  - `pentym events <target> --since <duration>` listing numbered events (number, time, xid or timestamp, type, object), with `--json`.
  - `pentym restore <target> --before-event <n>` for both engines, stopping just before that transaction or oplog entry.
- **Tests:** end to end on both engines: drop, list, restore before the event, object back, later writes intact.
- **Done when:** M5's milestone holds — `events` and `--before-event` work for both engines.

## 10. Phase M6 — Trust, operations and release

M6 makes Pentymento safe to leave running and ready to ship: verification, scheduling, retention, notifications, observability, installation, documentation, benchmarks and the release.

### C26 — Verify

- **Goal:** `pentym verify` proves restorability at levels L0–L5.
- **Depends on:** C21, C22
- **Read:** PRD 7.11; SDD 6.5.
- **Build:**
  - Levels L0–L5 and presets `quick`, `standard`, `deep`.
  - L3 recovery-point checks (`pg_last_xact_replay_timestamp()` and LSN; last applied oplog timestamp); L4 inventory comparison within tolerance plus `pg_amcheck` and `validate`; L5 random point in the window.
  - Optional canary (`pentym.canary`, updated every 60 s by the agent when enabled).
  - The snapshot's **verified** marker set only on a pass; human report and `--json`; 6 h time limit; guaranteed cleanup; journal entry; `status` shows the last verify.
- **Tests:** each level on both engines; injected corruption and a wrong recovery point both fail verify and leave the snapshot unmarked.
- **Done when:** all presets work on both engines and failures are never marked verified.

### C27 — Scheduler

- **Goal:** built-in schedules for backup, verify, prune and check.
- **Depends on:** C17
- **Read:** PRD 7.9; SDD 9.2; PRD 9.1 (scheduling defaults).
- **Build:**
  - Schedule parsing (`daily 02:00`, `every 6h`, `after backup`, 5-field cron) in the target's time zone; own loop with an injectable clock.
  - No overlapping runs; missed-run grace (the smaller of 6 h and half the interval); fixed per-target jitter; 1 heavy job per agent; optional I/O throttling.
  - Schedule state in the repository; only the lease holder runs schedules.
  - Dead-man's switch (1.5× the interval, at least interval + 1 h), also applied by `status`.
  - Every run and skip journaled.
- **Tests:** fake-clock tests for overlap, missed runs, jitter and DST; failover neither duplicates nor drops runs.
- **Done when:** schedules behave correctly across agent restarts and failovers.

### C28 — Retention and prune

- **Goal:** retention policy and safe two-phase prune.
- **Depends on:** C20, C26
- **Read:** PRD 7.10; SDD 6.6, 7.4.
- **Build:**
  - Policy evaluation: GFS rules, `keep_within`, pins (`pentym pin` with `--reason`), the anchor rule, safety rules (newest successful and newest verified kept; refuse after 3 failed backups).
  - Two-phase prune: forget without the chunk-store lock; garbage collection under the exclusive lock with the safety margin (the larger of 24 h and 2× the longest backup run); index rewritten after pack deletion.
  - Interactive dry run with confirmation; journaled.
  - `doctor` checks for Object Lock mismatches and bucket lifecycle rules; actual-window reporting confirmed in `status`.
  - Prune added to the C11 model-based tests.
- **Tests:** table-driven policy tests; model-based tests with prune; kill tests mid-prune; a backup running during prune never loses data.
- **Done when:** prune never deletes referenced data in any test and the safety rules hold.

### C29 — Notifications

- **Goal:** actionable, routed, reliable notifications.
- **Depends on:** C20, C25, C26, C27
- **Read:** PRD 7.12; SDD 10.4; Brand 11.
- **Build:**
  - Condition tracker (2 min hold, 5 min clear, 1 h grouping; destructive events and verify failures immediate).
  - Routing by target, severity and event type; local outbox with retries.
  - Channels: webhook (versioned JSON, `X-Pentym-Signature` HMAC), Slack, heartbeat pings.
  - Every critical and warning condition from PRD 7.12 wired, with Brand 11 message text and the next command; `redact_object_names`.
  - `pentym notify test`.
- **Tests:** each condition raises the right severity; deduplication and resolved messages; channel outage doesn't block work; HMAC verification; no secrets or row data in payloads.
- **Done when:** every condition in PRD 7.12 reaches its configured channel with an actionable message.

### C30 — Observability completion

- **Goal:** every metric, history and debugging tool in place.
- **Depends on:** C29
- **Read:** PRD 7.13; SDD 10.1–10.3.
- **Build:**
  - All metrics in PRD 7.13 wired with real values.
  - `pentym history <target> --since`.
  - `pentym debug bundle` (version, doctor output, redacted config, recent logs, journal entries).
  - Windows Event Log for warnings and errors; optional rotating `log.file`.
  - Grafana dashboard JSON and example Prometheus alert rules in `deploy/`.
- **Tests:** metric presence and values in integration tests; debug bundle contains no secrets.
- **Done when:** the Grafana dashboard shows live data from a running agent.

### C31 — Service installation and deployment files

- **Goal:** the agent installs as a service everywhere and runs in containers.
- **Depends on:** C17
- **Read:** PRD 7.14; SDD 9.7, 11.
- **Build:**
  - `pentym agent install | uninstall | start | stop` via `kardianos/service`, with `--print`; users, config locations and stop timeouts from SDD 9.7.
  - Multi-architecture container image (non-root).
  - Example Kubernetes manifests: Deployment per target, 2 replicas, `terminationGracePeriodSeconds: 45`, ConfigMap and Secrets, optional health port probes.
- **Tests:** install, start, stop and uninstall on Linux, macOS and Windows (release pipeline); Kubernetes failover between replicas.
- **Done when:** the agent runs as a service on all three OSes and fails over between Kubernetes replicas.

### C32 — `init` and `doctor` completion

- **Goal:** setup and diagnostics are complete and security-minded.
- **Depends on:** C28, C31
- **Read:** PRD 4.3, 7.15, 8.3; SDD 8.5, 11.
- **Build:**
  - `init`: deployment-setup questions (same host → peer or socket auth and throttling; separate host → require TLS and print access rules), recommended mode from acceptable data loss, default schedules and retention, storage estimate, `--print-sql` and `--print-mongo` for minimal roles.
  - `doctor`: missing and excess privileges (for example a superuser replication role), TLS to remote databases, network-filesystem repositories, tool and server versions, plaintext secrets.
- **Tests:** generated roles work with least privilege on every supported version; `doctor` flags each misconfiguration.
- **Done when:** a new user reaches a first backup in under five minutes following `init`.

### C33 — Documentation and brand assets

- **Goal:** the documentation and asset kit required for v1.
- **Depends on:** C32
- **Read:** PRD 9 (documentation), 11.2 (documentation); Brand 3, 9, 12, 14.
- **Build:**
  - README: tagline, subtitle, pronunciation, "Why the name", quick start, demo GIF slot, "By the numbers" slot (filled by C34).
  - `docs/architecture.md`, `docs/repository-format.md`, `docs/crypto.md`, the operations guide (deployment setups, minimal roles, Object Lock, the GitHub Actions example, troubleshooting), a reference for every command and the defaults, and an entry for every message ID — organized per Brand 12.1, with Limitations sections.
  - Asset kit in `docs/brand/` (Brand 14): D1 mark and lockups as SVG masters with exports, favicon, social preview, README banner, terminal theme, brand README.
- **Tests:** CI checks for broken links and for every message ID having a docs entry; glossary terms used consistently.
- **Done when:** every documentation item in PRD 11.2 exists.

### C34 — Benchmarks

- **Goal:** reproducible, published numbers.
- **Depends on:** C32
- **Read:** PRD 10.3.
- **Build:**
  - `bench/` with dataset generators (pgbench scales for about 10, 50 and 100 GB with seeded churn; a seeded MongoDB generator), baseline scripts (`pg_dump`/`mongodump` plus gzip plus upload), reference-tool configs, and `make bench`.
  - The five headline metrics, at least three runs each on documented hardware, with raw CSV/JSON committed; README "By the numbers" filled only with measured values.
  - Microbenchmarks compared with `benchstat` in CI.
- **Done when:** `make bench` reproduces the published numbers and no unmeasured number appears anywhere.

### C35 — Demo test, soak and release

- **Goal:** the v1 definition of done is met and v1.0.0 is released.
- **Depends on:** C30, C33, C34
- **Read:** PRD 11.2; Brand 9.3, 13.
- **Build:**
  - The two-act demo as an automated `testscript` test, and the scripted recording for the README GIF.
  - Nightly soak under load with random kills; nightly chaos and long fuzzing confirmed running.
  - A v1.0 golden repository committed to `testdata/`.
  - GoReleaser: signed binaries for Linux, macOS and Windows on amd64 and arm64 with checksums; the container image on ghcr; Homebrew tap and Scoop bucket; changelog; `v1.0.0` tag.
  - Launch write-up draft.
- **Done when:** every checkbox in PRD 11.2 is ticked, including two consecutive weeks of green nightly model-based and kill tests.
