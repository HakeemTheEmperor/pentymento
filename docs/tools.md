# How pentym runs database tools

Some work is done by the databases' own tools: `pg_basebackup` (as a fallback), `pg_dump` and `pg_restore` for Postgres, `mongodump` and `mongorestore` for MongoDB. pentym runs them as child processes with these rules (PRD 7.2).

## No secrets on the command line

A password on a command line is visible to every user of the machine (`ps`, `/proc`, Task Manager) and often ends up in shell history and logs. pentym never puts one there:

- **Postgres tools** get a temporary `.pgpass` file, named by `PGPASSFILE`.
- **MongoDB tools** get a temporary `--config` file holding the password.
- `PGPASSWORD` in the child's environment is used only as a fallback when a file cannot be.

pentym checks every command before running it and refuses one whose arguments, or environment outside that fallback, contain a secret.

The temporary files are created in their own directory, readable only by pentym's user (mode `0600` in a `0700` directory; on Windows, an access list granting only that user, without inherited entries). Each file is named after the process that wrote it and deleted as soon as the tool exits, whether it succeeded, failed or was cancelled. Files left by a pentym that crashed are deleted the next time pentym runs.

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
