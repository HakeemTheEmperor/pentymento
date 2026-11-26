# Commands

Every command has `--help` with examples, prints its result on stdout (as JSON with `--json`), and exits with a stable [exit code](exit-codes.md). This page covers the commands that are built so far: `init`, `key`, `check`, `repo`, `doctor`, `backup`, `list` and `dump`.

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

It unlocks the repository with any key slot; an agent slot is enough. It prints the snapshot ID on stdout, and with `--json` the snapshot and what the backup stored. `--now`, which runs the backup through the agent, is not built yet.

## `pentym list`

Lists the snapshots, oldest first: ID, target, engine and version, start time, duration, size of all its streams, and whether it is pinned (with the reason). `--json` adds the log positions, the timeline, the streams and the backup method. It unlocks the repository with any key slot. Log coverage is added with continuous capture.

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
