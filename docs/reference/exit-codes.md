# Exit codes

Every `pentym` command ends with one of these exit codes. They are stable: new codes may be added, but existing codes are never renumbered or given a new meaning, so scripts and CI jobs can rely on them.

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | The operation failed |
| 2 | Usage error: bad command, flag or argument |
| 3 | Configuration error |
| 4 | Not found: target, snapshot or event |
| 5 | Cannot connect to the database or storage |
| 6 | Key or permission problem: no key slot unlocks, or a privilege is missing |
| 7 | Integrity or verification failure: `check` or `verify` found a problem |
| 8 | Recovery not possible as asked: a gap, or a point outside the recovery window |
| 9 | Declined at a confirmation prompt |
| 10 | Not implemented |
| 130 | Interrupted (Ctrl-C) |

With `--json`, a failed command also writes an error document to stdout:

```json
{
  "error": {
    "target": "prod-pg",
    "message": "cannot connect to 10.0.0.5:5432",
    "cause": "password authentication failed for user \"pentym_backup\"",
    "next": "check the password reference for prod-pg.backup in config, then run: pentym doctor prod-pg"
  }
}
```

`id` is the message ID, when the message has one (see the [message reference](../errors/README.md)). `id`, `target`, `impact`, `cause` and `next` are left out when empty.
