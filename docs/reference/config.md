# Configuration

pentym reads one YAML config file, plus any files it includes, and applies environment overrides on top.

## Where the file is

pentym uses the first of:

1. `--config <path>`
2. the `PENTYM_CONFIG` environment variable
3. the default for the system:

| System | Path |
| --- | --- |
| Linux | `/etc/pentym/config.yaml` |
| macOS | `/usr/local/etc/pentym/config.yaml` |
| Windows | `%ProgramData%\pentym\config.yaml` |

## Keys

Unknown keys are rejected with the file and line, so a misspelt key never passes silently.

| Key | Meaning |
| --- | --- |
| `include` | Files applied before this one (see below) |
| `allow_plaintext_secrets` | Allow secret values written in the file; default `false` |
| `repository.path` | The repository directory on local storage; must be absolute |
| `repository.key` | A [secret reference](#secrets) to what unlocks a key slot when there is no terminal to ask in |
| `targets.<name>.engine` | `postgres` or `mongodb` |
| `targets.<name>.host`, `.port`, `.database` | Where the Postgres server is; `host` may be a Unix socket directory; empty values use the libpq defaults |
| `targets.<name>.sslmode` | `disable`, `allow`, `prefer` (default), `require`, `verify-ca` or `verify-full` |
| `targets.<name>.sslrootcert`, `.sslcert`, `.sslkey` | Certificate and key files, absolute paths |
| `targets.<name>.uri` | Where the MongoDB server or replica set is: a connection string such as `mongodb://db1:27017,db2:27017/?replicaSet=rs0&tls=true`, or `mongodb+srv://…`; default `mongodb://localhost:27017`. It must not hold a user name or password. MongoDB targets only; Postgres targets use `host`, `port` and `database` |
| `targets.<name>.credentials.backup.user` | The role (Postgres) or user (MongoDB) base backups connect as. Postgres needs `REPLICATION`; MongoDB needs the `backup` role and read on `local`. Leave it out for MongoDB x.509 (`authMechanism=MONGODB-X509` in `uri`) |
| `targets.<name>.credentials.backup.password` | A [secret reference](#secrets) to its password; leave it out for certificates, peer authentication or `.pgpass` |
| `instance_provider` | Side-instance providers to try, in order: `docker`, `local`; default `[docker, local]` |
| `work_dir` | Directory for side-instance files and temporary credential files; must be absolute; default `pentym/work` in the user's cache directory |
| `tools.postgres`, `tools.mongodb` | Directories of database binaries, searched before PATH; must be absolute |

A target name is lowercase letters, digits, `-` and `_`, starts with a letter or digit, and is at most 63 characters. Two names that differ only in `-` and `_` are not allowed, because environment overrides cannot tell them apart.

```yaml
targets:
  prod-pg:
    engine: postgres
    host: db.internal
    port: 5432
    sslmode: verify-full
    sslrootcert: /etc/pentym/db-ca.pem
    credentials:
      backup:
        user: pentym_backup
        password: env:PG_BACKUP_PW
  prod-mg:
    engine: mongodb
    uri: mongodb://db1:27017,db2:27017,db3:27017/?replicaSet=rs0&tls=true
    credentials:
      backup:
        user: pentym_backup
        password: env:MG_BACKUP_PW
```

## Repository

```yaml
repository:
  path: /var/lib/pentym/repo
  key: env:PENTYM_KEY
```

- `path` is where `pentym init` creates the repository and where every other command finds it.
- Commands that unlock the repository (such as `pentym key add`) ask for the passphrase in a terminal. Without a terminal (scripts, cron, CI) they read `key` instead; without either they stop with [PTM-4004](../errors/PTM-4004.md).
- A `key` value that starts with `AGE-SECRET-KEY-` is used as an age identity; anything else as a passphrase or paper recovery code. pentym tries it against every slot of that kind; `--slot <id>` limits it to one.

## Side instances

Restores and verification run a temporary database server of the backup's version, a side instance (PRD 7.6).

```yaml
instance_provider: [docker, local]
work_dir: /var/lib/pentym/work
tools:
  postgres: [/usr/lib/postgresql/17/bin, /usr/lib/postgresql/16/bin]
  mongodb: [/opt/mongodb-8.0/bin]
```

- **`docker`** runs the official `postgres:<major>` or `mongo:<version>` image through the `docker` command, with the data directory mounted. On Linux the container runs as pentym's user, so the data files stay pentym's.
- **`local`** runs `postgres` or `mongod` from the first directory in `tools.<engine>`, or on PATH, whose version matches the backup. List several directories to keep several versions.
- Providers are tried in `instance_provider` order; when none can start the version needed, the error says why for each ([PTM-6001](../errors/PTM-6001.md)).
- A side instance listens on `127.0.0.1` only, on a random port, and its superuser password is random, passed through files that only pentym's user can read, and never shown.
- `work_dir` holds each running instance's files under `instances/`, and temporary credential files under `secrets/`. Before starting an instance, pentym removes what a crashed run on the same machine left behind: servers, containers (labelled `pentym.owner-pid` and `pentym.owner-host`) and credential files.
- `pentym doctor` reports what each provider can start.

## Includes

```yaml
include:
  - common.yaml
  - targets/prod.yaml
```

- Paths are relative to the file that includes them.
- Included files are applied first, in order, then the including file on top.
- Mappings merge key by key; single values and lists are replaced.
- A file that includes itself, directly or through others, is an error.

## Environment overrides

Any key that holds a single value can be set with an environment variable: `PENTYM_` followed by the key path, with `__` (two underscores) between levels.

```sh
PENTYM_ALLOW_PLAINTEXT_SECRETS=false
PENTYM_TARGETS__PROD_PG__ENGINE=postgres
```

- Names match keys regardless of case, and `_` matches `-`, so `PROD_PG` matches the target `prod-pg`.
- A target must already exist in the file to be overridden.
- A variable that matches no key, or matches two, is an error.
- `PENTYM_CONFIG` and variables starting `PENTYM_TEST_` are not overrides.

## Secrets

The config holds secrets only as references, resolved each time a secret is used, so a rotated secret takes effect on the next connection without a restart.

| Reference | Reads the secret from |
| --- | --- |
| `env:NAME` | The environment variable `NAME`; an unset or empty variable is an error |
| `file:/path` | A file, such as a Docker or Kubernetes secret mount; one trailing newline is ignored |
| `keychain:service/item` | macOS Keychain, Windows Credential Manager or Linux Secret Service |
| `cmd:<helper>` | The standard output of a helper such as `op read op://vault/db/password`; one trailing newline is ignored |

A `cmd:` helper:

- runs directly, without a shell; quote arguments that contain spaces with `'…'` or `"…"`;
- must finish within 30 seconds;
- inherits pentym's environment and gets empty standard input;
- is shown, with its arguments, in messages and the journal, so its arguments must never contain a secret.

A value written in the file is rejected ([PTM-1001](../errors/PTM-1001.md)) unless `allow_plaintext_secrets: true` is set and only the file's owner can read it ([PTM-1002](../errors/PTM-1002.md)). An environment override of a secret must always be a reference.

pentym never prints a secret: logs, errors, `--json` output, journal entries and notifications show `[redacted]`, or the reference itself, such as `env:PGPASSWORD`.

## Limitations

- Go's garbage collector cannot guarantee that a secret is wiped from memory; pentym clears the buffers it controls.
- The `keychain:` reference needs a keychain for the user pentym runs as; servers often have none, so use `env:` or `file:` there.
- On Windows, the check for files other users can read looks at Everyone, Users and Authenticated Users only.
