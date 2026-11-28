# Commands

Every command has `--help` with examples, prints its result on stdout (as JSON with `--json`), and exits with a stable [exit code](exit-codes.md). This page covers the commands that are built so far: `init`, `key`, `check`, `repo`, `doctor`, `backup`, `list`, `dump`, `agent` and `agent reload`.

Secrets never go on the command line. Flags that take a passphrase take a [secret reference](config.md#secrets) such as `env:NAME` or `file:/path`, and refuse a plain value. In a terminal, pentym asks for passphrases instead, without echoing them.

## `pentym init`

Creates an encrypted repository at `repository.path` or in the bucket of `repository.s3` (see [Configuration](config.md#repository)) with:

- at least two **full key slots**, or one full slot and a **paper recovery code**: losing the only way to unlock would make every backup unreadable;
- an **agent slot**, which can write backups but never read them.

```sh
pentym init
pentym init --key env:PENTYM_KEY --key file:/root/second-key --agent-key env:PENTYM_AGENT_KEY
pentym init --key env:PENTYM_KEY --paper --agent-key env:PENTYM_AGENT_KEY
```

| Flag | Meaning |
| --- | --- |
| `--key <reference>` | Add a full passphrase slot; repeatable |
| `--age-recipient <age1…>` | Add a full slot unlocked by that age identity; repeatable |
| `--paper` | Add a full slot unlocked by a paper recovery code, printed once |
| `--agent-key <reference>` | The agent slot's passphrase |
| `--insecure-test-mode` | Create an unencrypted repository for local development; no slots |

In a terminal, `init` asks for whatever the flags leave out: the first passphrase, then a second one (leave it empty to get a paper recovery code instead), then the agent slot's passphrase. Each new passphrase is typed twice.

`init` refuses:

- fewer than two full slots counting the paper code ([PTM-4005](../errors/PTM-4005.md));
- two full slots with the same passphrase;
- an agent passphrase equal to a full slot's, because a database host holding it could then unlock everything;
- a location that already holds a repository ([PTM-3003](../errors/PTM-3003.md)).

It also tests that the storage honours conditional create, by creating the same object twice, records the result in the repository, and warns if it does not ([PTM-3005](../errors/PTM-3005.md)). `--insecure-test-mode` works only with a local path.

The paper recovery code is printed on stdout once and stored nowhere: write it down and keep it offline. It is never logged or journaled.

`--print-sql` and `--print-mongo` are not built yet.

## `pentym key`

Manages key slots: the ways to unlock the repository. Every slot opens the same keys; removing a slot stops that way of unlocking.

| Command | Unlocks with | Does |
| --- | --- | --- |
| `pentym key list` | nothing | Lists the slots; warns when only one full slot is left |
| `pentym key add` | a full slot | Adds a passphrase slot, or an age slot with `--age-recipient` |
| `pentym key remove <slot>` | a full slot | Removes a slot; the last full slot cannot be removed ([PTM-4002](../errors/PTM-4002.md)) |
| `pentym key export --paper` | a full slot | Adds a paper slot and prints its recovery code once |
| `pentym key rotate` | a full slot | Generates a new key pair and re-wraps every data key to it (see below) |

`key add` flags:

| Flag | Meaning |
| --- | --- |
| `--key <reference>` | The new slot's passphrase; in a terminal it is asked for instead |
| `--age-recipient <age1…>` | Make an age slot for that identity instead of a passphrase slot |
| `--agent` | Make an agent slot, which can write backups but never read them |
| `--label <name>` | A name shown in `key list` |

The commands that unlock read the passphrase, recovery code or age identity from the terminal, or from `repository.key` when there is no terminal. `--slot <id>` tries only that slot, which saves one Argon2id derivation per other slot.

Every change is journaled with the secret reference used, never the secret.

### `pentym key rotate`

Generates a new repository key pair, re-wraps every data key in the index and every log segment's key to it, and keeps the replaced private key, encrypted, as `keys/private-<n>`. Packs are never rewritten, and slots are unchanged. It holds the exclusive lock and is journaled; if it is interrupted, everything stays readable and running it again finishes the work.

Rotation protects data written after it. Whoever holds a replaced private key can still read data written before: see [Key rotation](../crypto.md#key-rotation).

## `pentym check [target]`

Checks the repository's integrity (verify level L0):

| Step | Checks |
| --- | --- |
| slots | Every key slot parses; warns with one full slot ([PTM-4006](../errors/PTM-4006.md)); every replaced private key opens |
| index | Every index file matches its name and decrypts |
| packs | Every pack the index lists exists and is long enough; warns about unindexed packs and stray objects |
| snapshots | Every snapshot decrypts, its chunk lists read back, and every chunk it references is indexed |
| data | A sample of packs is downloaded whole: its hash matches its name, its trailer matches the index, and every chunk decrypts and matches its ID |
| segments | A sample of log segments is downloaded and decrypted |

| Flag | Meaning |
| --- | --- |
| `--sample <percent>` | The share of packs and log segments read in full; default 5, at least one of each |
| `--full` | Read every pack and log segment |
| `--slot <id>` | Unlock with this slot only |

With a target, only that target's snapshots, the packs they use and its log segments are checked.

`check` needs a full key slot, holds a shared lock so prune cannot delete what it reads, and is journaled. It prints a table with one row per step and a message per problem ([PTM-7001](../errors/PTM-7001.md) to [PTM-7004](../errors/PTM-7004.md)); `--json` prints the whole report. It exits with code 7 when it finds an error; warnings alone exit 0.

## `pentym repo`

| Command | Unlocks with | Does |
| --- | --- | --- |
| `pentym repo rebuild-index` | any slot | Rebuilds the index from the pack trailers and replaces the old index files |
| `pentym repo upgrade` | a full slot | Upgrades the repository to the newest format this pentym understands |

Both hold the exclusive lock and are journaled, and both take `--slot <id>`.

`rebuild-index` writes the new index files before deleting the old ones, so an interrupted rebuild loses nothing. Packs whose trailer cannot be read are listed, and the command exits with code 7 ([PTM-7002](../errors/PTM-7002.md)).

`upgrade` is the only thing that changes the repository format: upgrading pentym never does. Run it only after every pentym that uses the repository (agents, CLIs, CI jobs) is upgraded, because older versions refuse a newer format ([PTM-3001](../errors/PTM-3001.md)). In a terminal it lists the steps and asks for confirmation (default No; declining exits with code 9). Format 1 is the only format so far, so it reports that the repository is current.

## `pentym doctor [target]`

Checks what pentym needs on this machine and prints one row per check:

| Check | Reports |
| --- | --- |
| `<engine> server` | Each directory in `tools.<engine>` or on PATH holding the server binary (`postgres`, `mongod`), with its version |
| `<engine> tools` | The client tools (`pg_dump`, `pg_restore`, `pg_basebackup`, `initdb`, `pg_ctl`; `mongodump`, `mongorestore`) and their versions |
| `docker` | Whether the Docker daemon is reachable, and its version |
| `instance providers` | The `instance_provider` order and what each can start |
| `repository` | The repository's storage (a local directory or an S3 bucket), its capabilities, and a conditional-create test compared with what `init` recorded |

With a target, only that target's engine is checked; otherwise the engines of the configured targets, or both when none is configured. Missing tools are failures when targets are configured, warnings otherwise. `doctor` exits with code 1 when a check fails; warnings alone exit 0. Connection and privilege checks for each target come with the engines.

## `pentym backup <target>`

Takes a base backup of a target and commits it as a snapshot (SDD 6.1):

1. Checks the connection and the backup role's privileges, as `doctor` does; a failure stops the backup, warnings are printed.
2. Takes a shared repository lock, so prune's garbage collection cannot run at the same time.
3. Records the target's inventory, then streams the backup straight into the repository: chunked, deduplicated, compressed, encrypted and packed. Nothing is written to temporary files, and memory stays constant whatever the database's size.
4. Commits the snapshot only if the backup finished cleanly. A failed or cancelled backup commits nothing; the packs it uploaded are removed by a later prune.
5. Journals the outcome.

For Postgres, the backup uses `BASE_BACKUP` over a replication connection, falling back to `pg_basebackup` for clusters without tablespaces. It includes the WAL the snapshot needs to restore on its own, and a temporary replication slot keeps the server from removing that WAL during the backup. Each archive (`base.tar`, one per tablespace, and the backup manifest) is stored as its own stream.

For MongoDB, the backup runs `mongodump --archive` from the [Database Tools](../tools.md#mongodb-snapshots-with-mongodump) and stores the archive as one stream, `mongodump.archive`. On a replica set it adds `--oplog`, so the snapshot restores to one consistent point, and records the first and last oplog timestamps it holds as its log positions. A standalone server has no oplog: its snapshots are taken without one, record no positions, and allow no point-in-time recovery, which the backup reports as a warning ([PTM-2011](../errors/PTM-2011.md)). Sharded clusters are refused.

It unlocks the repository with any key slot; an agent slot is enough. It prints the snapshot ID on stdout, and with `--json` the snapshot and what the backup stored. With `--now` the running agent takes the backup instead, through its control socket, with its own connection, key slot and lock; the result is printed the same way. If no agent answers, it says so with the last heartbeat in the repository and exits with code 5; run it without `--now` to take the backup here.

## `pentym list`

Lists the snapshots, oldest first: ID, target, engine and version, start time, duration, size of all its streams, and whether it is pinned (with the reason). `--json` adds the log positions, the timeline, the streams and the backup method. It unlocks the repository with any key slot. Log coverage is added with continuous capture.

## `pentym restore <target>`

Restores a target to just before a point in time and loads the tables you lost back into production, leaving everything else in production as it is. It reads only the repository, never needs the agent, and unlocks the repository with a full key slot. Postgres only in this build.

```
$ pentym restore prod-pg --xid 8812 --table orders
✓ prod-pg: restored public.orders as public.orders_recovered_20261122: 104 rows, as just before transaction 8812 (last change replayed: 14:02:58 WAT)
! prod-pg: left out foreign key orders_customer_id_fkey of public.orders_recovered_20261122, which refers to customers, a table that was not restored
  the restored rows are not checked against customers
→ ALTER TABLE "public"."orders_recovered_20261122" ADD CONSTRAINT "orders_customer_id_fkey" FOREIGN KEY (customer_id) REFERENCES customers(id);
```

**The point**, exactly one of:

| Flag | Restores to just before |
| --- | --- |
| `--at <time>` | a time: RFC 3339 (`2026-11-22T14:03:00+01:00`), or `2026-11-22 14:03[:12]` in local time |
| `--xid <id>` | a transaction's commit |
| `--lsn <position>` | a WAL position, such as `0/3A00F128` |
| `--before-event <n>` | an event (comes with event detection) |

pentym picks the newest snapshot taken before the point whose WAL continues, without a break, up to the point, restores it into a side instance (see [tools](../tools.md#restoring-a-snapshot-into-a-side-instance)), and replays the stored WAL to just before the point. For `--xid`, whose time is not known until replay, it replays from the earliest snapshot of the newest unbroken stretch of WAL. A point that cannot be reached, because it is after the newest stored change, before the earliest reachable point, or in a gap, is refused with the gap and the ranges that can be reached ([PTM-6002](../errors/PTM-6002.md), exit 8).

**Loading tables.** `--table` names a table to load, `orders` (schema `public`) or `billing.invoices`; repeat it for more. Each is loaded as `<table>_recovered_<date>`, the date being the restore point's in local time, with `_2`, `_3`… when that name is taken; its indexes and owned sequences get the same suffix, so nothing collides with the live table. With `--in-place` it is loaded under its own name instead, which only works for a table that no longer exists ([PTM-6003](../errors/PTM-6003.md)). Constraints, indexes, owned sequences with their values, defaults and triggers come along. A sequence continues from where replay left it, which can be up to 32 values past the last one used: Postgres writes sequences to the WAL in batches, so any recovery skips some values, but never hands one out twice. Foreign keys to tables that were not restored are left out, and printed with the statement that adds each back. Tables are loaded in one transaction, with the target's restore role (`credentials.restore`), so a failure loads nothing. A table that did not exist at the point is refused ([PTM-6004](../errors/PTM-6004.md)). stdout lists the tables written.

**Rewinding the whole cluster.** `--mode=rewind --data-dir <dir>` restores and replays the whole cluster to the point into `<dir>`, an empty or new directory on this machine, then stops it cleanly with the cluster's own configuration files back in place and pentym's side role removed. pentym never stops or writes to production: it prints the steps to replace production's data directory with `<dir>`, which loses every change after the point, so on a terminal it first asks you to type the target's name. A cluster with tablespaces cannot be rewound this way; restore its tables instead.

`--json` prints the snapshot used, where replay stopped, the tables loaded with their row counts, the foreign keys left out, and the rewound directory. The restore is journaled with the restore credential it used.

## `pentym dump <snapshot>`

Exports a snapshot as a plain file, decrypted and reassembled, so the data is never locked in: the database's own tools restore it without pentym (PRD 7.1).

| Snapshot | File |
| --- | --- |
| Postgres | A tar holding the snapshot's archives: `base.tar`, one `<oid>.tar` per tablespace, and `backup_manifest` when the backup has one. This is the layout `pg_basebackup --format=tar` writes to a directory. |
| MongoDB | The `mongodump` archive as stored, which `mongorestore --archive` reads (add `--oplogReplay` for a replica-set snapshot). |

```sh
pentym dump 3f9c2a7d18e04b6f9a1c5d2e7b8f0a14 --output prod-pg.tar
pentym dump 9b1e0c4f7a2d48e6b3c5f1a8d0e7c2b4 | mongorestore --archive --oplogReplay
```

| Flag | Meaning |
| --- | --- |
| `--output <file>` | Write to this file, which must not exist yet. It is created readable only by its owner and removed if the dump fails. |
| `--slot <id>` | Unlock with this key slot |

Without `--output` the dump goes to stdout, which must not be a terminal; `--json` then has nowhere to go, so it needs `--output`. The snapshot ID is the full ID that `pentym list` shows. The dump streams with constant memory, reading chunks in parallel ahead of the writer, and checks every chunk against its ID; it needs a full key slot and is recorded in the journal.

To restore a Postgres dump by hand, unpack it, then restore the archives as you would a tar-format `pg_basebackup`: `base.tar` into an empty data directory and each `<oid>.tar` into its tablespace's directory. The snapshot includes the WAL it needs, which Postgres replays when it starts.

## `pentym agent`

Runs the agent in the foreground until it is stopped with Ctrl-C or `SIGTERM` (SDD 9). `pentym agent install` and the other service commands are not built yet.

On start it:

1. opens the repository, refusing a newer format, and unlocks it with `repository.key`; with `agent.write_only` on (the default) it refuses a key slot that can read backups ([PTM-4007](../errors/PTM-4007.md));
2. removes what a crashed run on this machine left behind: side instances and temporary credential files;
3. writes a heartbeat to the repository, then one every 30 seconds;
4. runs a lease loop for every configured target. Exactly one agent captures each target: it holds the target's capture lease, renewing it every 30 seconds, while other agents check it every 15 seconds. If the holder cannot renew for 90 seconds it stops capturing; another agent takes over only after the lease has stood unchanged for 2 minutes, so two agents never capture at once.

On a graceful stop it finishes within 30 seconds: it stops capture, releases its leases, so a standby takes over at once, and writes a final heartbeat with `state: stopped`. After a crash, the lease expires after 2 minutes and a standby takes over.

`pentym agent reload`, or `SIGHUP` on Linux and macOS, reloads the configuration: new targets start, removed ones stop and release their leases, changed ones restart, and targets whose settings did not change keep running. A reload cannot switch repositories.

### Control socket

The agent serves JSON over HTTP on a local socket (`agent.socket`; by default `<work_dir>/agent.sock`, or a named pipe on Windows), never on the network. On Linux and macOS it is a Unix socket readable only by the agent's user; on Windows the pipe admits only the agent's user, Administrators and SYSTEM. A socket left by a crashed agent is replaced; a second agent on a live socket is refused.

| Request | Answer |
| --- | --- |
| `GET /v1/status` | Live state: the agent's ID and host, its last heartbeat, the upload queue depth, and per target whether it holds the lease, with the generation |
| `POST /v1/reload` | Reloads the configuration, as `pentym agent reload` does |
| `POST /v1/backup?target=<name>` | Takes a backup now, as `pentym backup <target> --now` does |
| `GET /health` | 200 while the process runs |
| `GET /ready` | 200 once the agent has started and until it begins to stop; 503 otherwise |
| `GET /metrics` | Prometheus metrics |
| `/debug/pprof/` | Go profiling, only with `agent.debug.pprof: true` |

It can be read with curl: `curl --unix-socket <work_dir>/agent.sock http://agent/v1/status`.

`agent.health_listen` opens an extra TCP port serving only `/health`, `/ready` and `/metrics`, for probes and Prometheus; no control request is ever reachable over the network.

Commands that use the socket fall back to the repository when no agent answers: they say "agent not reachable" with the newest heartbeat (when `repository.key` lets them read it without asking for a passphrase) and exit with code 5. Whether an agent is alive is judged from its heartbeat and lease in the repository, never from the socket.

Logs go to stdout: JSON for services, text in a terminal (`--log-format` overrides).

### WAL capture (Postgres)

While the agent holds a Postgres target's lease, it streams the target's WAL from a physical replication slot (`targets.<name>.slot`, default `pentym_<target>`), connecting as `credentials.replication` or, without it, `credentials.backup`. It creates the slot if it does not exist.

- Each completed WAL segment is stored as its own object as it fills; the segment in progress is stored every 10 seconds while new WAL arrives, and once more when the agent stops (as a `.partial`), so the recovery point is seconds.
- After each stored piece the agent writes a checkpoint carrying its lease generation, and only then tells Postgres the WAL is stored, so the slot frees only WAL that is safe in the repository. A restarted agent, after a crash or `kill -9`, resumes from the checkpoint; WAL stored twice is harmless.
- Timeline switches (after a failover) are followed, and each timeline's history file is stored.
- **Gaps.** If the WAL since the checkpoint can no longer be streamed (the slot was dropped or invalidated by `max_slot_wal_keep_size`, or the server removed the WAL), the agent records a gap in the repository and the journal, logs an error, counts it in `pentym_job_failures_total{job="capture"}`, and takes a base backup so recovery starts again after the gap. An invalidated slot is replaced; a slot that still holds WAL is never dropped.
- When capture starts with no checkpoint at all, the agent takes a base backup, once, so that recovery can start from captured WAL.
- **Monitoring.** Every 30 seconds the agent checks how much WAL the slot makes the server keep, and how far capture lags, and publishes `pentym_pg_slot_retained_wal_bytes` and `pentym_capture_lag_seconds`. It logs a warning or an error when a condition crosses a threshold, and again when it clears:

| Condition | Warning | Critical |
| --- | --- | --- |
| Capture lag | 5 min | 15 min |
| Retained WAL, `max_slot_wal_keep_size` set | 50% of it | 80% of it |
| Retained WAL, no limit set | 10 GB | 50 GB, with the advice to set `max_slot_wal_keep_size` |

A slot keeps WAL until the agent stores it, so a stopped agent makes the server keep WAL until it returns. Set `max_slot_wal_keep_size` so a long outage cannot fill the server's disk; the slot is then invalidated instead, which the agent reports as a gap. Alert delivery (webhooks, Slack) and configurable thresholds come with notifications.

### Oplog capture (MongoDB)

While the agent holds a MongoDB target's lease, it tails the replica set's oplog, `local.oplog.rs`, with a tailable cursor (not a change stream, whose output `mongorestore --oplogReplay` cannot replay), connecting as `credentials.replication` or, without it, `credentials.backup`. The user needs `read` on the `local` database, which the backup user already has. A standalone server has no oplog: the agent holds its lease without capturing and says so, and its snapshots allow no point-in-time recovery.

- Entries are stored in batches, each its own object holding the entries as they are in the oplog, BSON documents one after another (the format of `mongodump`'s `oplog.bson`). A batch is stored after 10 seconds, 16 MiB or 10,000 entries, whichever comes first, and once more when the agent stops, so the recovery point is seconds.
- Only entries a majority of the replica set has written are stored, so a failover never rolls back a stored entry. While the set has no majority, capture waits, and its lag grows.
- After each stored batch the agent writes a checkpoint at the batch's last entry, carrying its lease generation. A restarted agent resumes after the checkpoint; a batch stored twice is harmless.
- **Rollover.** The oplog is a fixed size: when the agent has been stopped, or falls behind, for longer than the oplog reaches back, entries it has not stored are overwritten. If the oplog's oldest entry is newer than the checkpoint, or it rolls past capture while capture reads it, the agent records a gap, logs an error, counts it in `pentym_job_failures_total{job="capture"}`, and takes a base backup, as for Postgres.
- When capture starts with no checkpoint at all, it starts from the newest entry and the agent takes a base backup, once.
- **Monitoring.** Every 30 seconds the agent checks how far back the oplog reaches (its window) and how far capture lags, and publishes `pentym_mongo_oplog_window_seconds` and `pentym_capture_lag_seconds`, with the same logging as for Postgres:

| Condition | Warning | Critical |
| --- | --- | --- |
| Capture lag | 5 min | 15 min |
| Oplog window | under 24 h | under 6 h, or capture lag over half the window |

The window is how long an agent can be stopped before capture loses entries. Enlarge the oplog with `replSetResizeOplog` if it holds less than a day.

## `pentym status`

Shows every configured target's real protection, read from the repository, so it works without an agent and from any machine that can read the repository, with any key slot.

```
TARGET    ENGINE       AGENT              PITR WINDOW                LAST VERIFY
prod-pg   postgres 17  ✓ capturing (4s)   7d 0h · up to 4s ago      —
prod-mg   mongodb 8.0  ✗ stale (2h 14m)   6d 22h · up to 14:02 WAT  —

✗ prod-mg: agent last seen 2h 14m ago on db2; changes since 14:02 WAT are not protected
→ pentym catch-up prod-mg
```

- **AGENT** is the agent holding the target's capture lease and how long ago it wrote its last heartbeat: `capturing`, `stale` (a warning from 5 minutes, critical from 15), `stopped` (the lease was released), or `none` (no agent has captured the target). A stale or stopped agent comes with the `pentym catch-up` command to run.
- **PITR WINDOW** is the window point-in-time recovery actually covers, and how recent its newest point is. It runs from the end of the earliest snapshot whose log was captured, through the stored WAL or oplog without a break, to the newest stored change; a missing WAL segment or a recorded gap ([PTM-5001](../errors/PTM-5001.md)) breaks it. When a gap makes it shorter than `retention.pitr_window`, status says so: "configured 7d 0h, actual 2d 4h (gap at 2026-11-21 03:12 WAT)". A MongoDB standalone server shows `snapshots only`.
- Postgres times inside the window come from when WAL was stored (every 10 seconds while capture runs), MongoDB times from the oplog itself. Postgres WAL is measured with the cluster's WAL segment size, which snapshots record; older snapshots without it count as the default 16 MiB.
- **LAST VERIFY** comes with `pentym verify`, and the missed-backup check with schedules.
- It warns when the repository has only one full key slot ([PTM-4006](../errors/PTM-4006.md)).
- **Automatic catch-up.** With `catch_up.auto: true`, status runs `pentym catch-up` itself for a target whose agent is stopped or stale and that has been captured before (it needs the database's capture credential, as catch-up does). Catch-up takes the lease only once it has expired, so it may wait up to 2 minutes. Without the setting, status only prints the command.
- `--live` also asks the agent on this machine, through its control socket, for each target it holds: capture lag, the WAL the replication slot retains (with `max_slot_wal_keep_size`) or the oplog window, and its upload queue. When the agent does not answer, status says so and shows what the repository holds.
- `--json` prints all of it, with times in RFC 3339 UTC; `last_verify` and `dead_man` are `null` until verify and schedules fill them.

## `pentym catch-up <target>`

Stores the WAL or oplog a target's server still retains while no agent captures it, then exits: by hand, from cron, or from a standby machine. Postgres keeps WAL for pentym's replication slot, and MongoDB keeps oplog entries until the oplog rolls over, so catch-up fills the recovery window back up to where the server stands when it starts.

```
$ pentym catch-up prod-pg
✓ prod-pg: caught up to 1/0/9A000148: stored 12 segments, 186 MiB in 41s
```

- **Only when no agent captures.** It takes the target's capture lease first: at once if the lease was released (a graceful stop), after the lease has stood unchanged for 2 minutes if an agent crashed (it says it is waiting). If an agent renews the lease meanwhile, that agent is capturing, and catch-up stores nothing and exits with code 1 ([PTM-8001](../errors/PTM-8001.md)). It releases the lease when it exits, so an agent that starts later takes over at once.
- It stores the log from the last checkpoint up to the server's position when it started, as the agent does (Postgres WAL segments and a final partial segment; MongoDB oplog batches of entries a majority has written), and writes a checkpoint after each. It connects as `credentials.replication` or, without it, `credentials.backup`, and unlocks the repository with any key slot, an agent slot included.
- **Gaps.** If the log since the checkpoint is gone (the slot was dropped or invalidated by `max_slot_wal_keep_size`, or the oplog rolled over), it records the gap in the repository and the journal, stores what the server still has, reports the gap and exits with code 8 ([PTM-5001](../errors/PTM-5001.md)). Unlike the agent it does not take a base backup itself; run `pentym backup <target>` so recovery starts again after the gap.
- A target with no log to capture (a standalone MongoDB server) exits with code 10.
- `--json` prints the report: the checkpoint before and after, the server position drained to, the segments and bytes stored, the lease generation and wait, and the gap, if any.

## `pentym agent reload`

Asks the running agent to reload its configuration through the control socket, and prints what changed: targets added, removed, restarted and unchanged (`--json` for the lists). Without a running agent it exits with code 5.
