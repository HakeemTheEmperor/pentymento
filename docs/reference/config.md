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
| `targets.<name>.engine` | `postgres` or `mongodb` |

A target name is lowercase letters, digits, `-` and `_`, starts with a letter or digit, and is at most 63 characters. Two names that differ only in `-` and `_` are not allowed, because environment overrides cannot tell them apart.

```yaml
targets:
  prod-pg:
    engine: postgres
  prod-mg:
    engine: mongodb
```

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
