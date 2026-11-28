# How pentym runs database tools

Some work is done by the databases' own tools: `pg_basebackup` (as a fallback), `pg_dump` and `pg_restore` for Postgres, `mongodump` and `mongorestore` for MongoDB. pentym runs them as child processes with these rules (PRD 7.2).

## No secrets on the command line

A password on a command line is visible to every user of the machine (`ps`, `/proc`, Task Manager) and often ends up in shell history and logs. pentym never puts one there:

- **Postgres tools** get a temporary `.pgpass` file, named by `PGPASSFILE`.
- **MongoDB tools** get a temporary `--config` file holding the password.
- `PGPASSWORD` in the child's environment is used only as a fallback when a file cannot be.

pentym checks every command before running it and refuses one whose arguments, or environment outside that fallback, contain a secret.

The temporary files are created in their own directory, readable only by pentym's user (mode `0600` in a `0700` directory; on Windows, an access list granting only that user, without inherited entries). Each file is named after the process that wrote it and deleted as soon as the tool exits, whether it succeeded, failed or was cancelled. Files left by a pentym that crashed are deleted the next time pentym starts a side instance. They live in `secrets/` under `work_dir` (see [Configuration](reference/config.md#side-instances)).

## Streams, logs and exit codes

- The tool's standard output is streamed straight into pentym's pipeline; nothing is buffered to disk.
- Its standard error is logged line by line, tagged with the tool and the run ID, with known secrets replaced by `[redacted]`. The last lines are kept for the error message if the tool fails.
- A non-zero exit code is a failure, and a stream from a failed tool never becomes a snapshot.
- Tool versions are read from `--version` before use.

## Stopping a tool

Each tool runs in its own process group (Linux, macOS) or job object (Windows), together with anything it starts. When an operation is cancelled, the whole group is asked to stop, then killed after 5 seconds; on Windows the job is ended at once. Processes a tool leaves behind when it exits are stopped too. On Windows, a crash of pentym itself also ends the job, because the job is set to kill its processes when pentym's handle to it closes.

## No compression at the source

Compressed input cannot be deduplicated: a small change reshuffles every compressed byte after it. pentym compresses after chunking, so tools must not compress their output. pentym builds tool commands itself and refuses arguments that turn compression on:

| Tool | Refused |
| --- | --- |
| `pg_basebackup` | `-z`, `--gzip`, `-Z <level>`, `--compress=<method>` |
| `pg_dump`, `pg_dumpall` | `-Z <level>`, `--compress=<method>` |
| `mongodump` | `--gzip` |

`-Z 0` and `--compress=none`, which turn compression off, are allowed.

## MongoDB snapshots with mongodump

MongoDB snapshots are taken with `mongodump` from the [MongoDB Database Tools](https://www.mongodb.com/docs/database-tools/) (version 100 or later; the older `mongodump` that came with the server is refused). pentym looks for it in `tools.mongodb`, then on `PATH`, and runs:

```text
mongodump --uri=<uri> --username=<user> --config=<temporary file> --archive [--oplog]
```

- The target's `uri` holds no credentials; the password is only in the temporary `--config` file. With x.509 (`authMechanism=MONGODB-X509`) neither `--username` nor `--config` is passed.
- `--archive` with no file name writes one archive to standard output, which streams into the repository as the snapshot's one stream, `mongodump.archive`.
- `--oplog` is added on a replica set, so the archive restores to one consistent point. pentym reads the archive as it passes and records the first and last oplog timestamps it holds as the snapshot's start and end (`<seconds>:<ordinal>`). A standalone server has no oplog: its snapshot is taken without `--oplog`, records no positions, and allows no point-in-time recovery ([PTM-2011](errors/PTM-2011.md)).
- A stream that is not a complete archive (wrong magic number, a document or block cut short, a collection left open) fails the backup, even if `mongodump` exited cleanly.

## Restoring a snapshot into a side instance

Restores and verification bring a snapshot up in a side instance: a temporary server of the snapshot's version, started by an instance provider (local binaries or Docker) over a new directory under `work_dir`, listening on `127.0.0.1` only. pentym reads the snapshot with parallel range reads while it restores, holds the repository's shared lock until the data is in place, and removes the instance and its directory afterwards, or at once if the restore fails.

**Postgres.** `base.tar` is unpacked into the data directory and each tablespace archive into `pg_tblspc/<oid>` as an in-place tablespace, so nothing points outside the directory. The WAL in the snapshot brings the cluster to a consistent state when it starts. The cluster's own `postgresql.conf`, `postgresql.auto.conf`, `pg_hba.conf` and `pg_ident.conf` are kept beside pentym's, renamed with `.pentym-original`; the side instance runs without archiving, preloaded libraries or TLS.

The restored cluster's roles have passwords pentym does not know, so pentym gives itself a login:

1. It starts the instance with access rules that trust every login. The instance listens only on `127.0.0.1` (under Docker, its port is published only there).
2. As soon as the cluster has recovered, pentym logs in, creates the role `pentym_side` as a superuser with a random password, and replaces the access rules with one that admits only `pentym_side`, with its password.
3. It reloads the rules and checks that a login without a password is refused before it uses the instance.

The window lasts a few seconds. It never touches production: it applies only to the side instance, which is deleted after the restore.

**Replaying WAL to a point (Postgres).** For a restore to a point, pentym copies the stored WAL the restore needs into the side instance's `pg_wal` (a partial segment padded with zeros to the segment size), writes `recovery.signal`, sets a `restore_command` that fails for every file (archive recovery requires one; Postgres then reads each file from `pg_wal`), and sets `recovery_target_time`, `recovery_target_xid` or `recovery_target_lsn` with `recovery_target_inclusive = off` and `recovery_target_action = 'promote'`. `hot_standby = off` keeps every login out until replay has finished, so the trust window above opens only after it. Postgres records times only on commits, so a time after the last commit in the WAL is reached by replaying all of it; the data is the same.

**Loading tables into production (Postgres).** `pg_dump --format=custom --no-owner` reads the tables from the side instance and is piped straight into `pg_restore --single-transaction --exit-on-error --no-owner` against production, as the target's restore role: a failure loads nothing, and nothing is written to disk. Both come from `tools.postgres` or PATH; `pg_dump` must be at least the restored server's major version, which pentym checks. Each password reaches its tool in its own temporary `.pgpass` file. Grants are restored only with `--in-place`, so a copy under a safe name is not exposed to the application.

**MongoDB.** pentym starts an empty side instance with a random superuser password, then streams the archive into `mongorestore --archive --stopOnError`, adding `--oplogReplay` for a replica-set snapshot so the data is as of the snapshot's consistent point. The password reaches `mongorestore` in a temporary `--config` file. `mongorestore` comes from the same MongoDB Database Tools as `mongodump`.
