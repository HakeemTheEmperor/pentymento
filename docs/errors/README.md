# Message reference

Errors and warnings you can act on carry a message ID such as `PTM-1203`. Run a command with `-v`, or with `--json`, to see the ID; each ID has a page here explaining the cause, the impact and the fix.

IDs are permanent: an ID is never reused and never changes meaning.

| Block | Area |
| --- | --- |
| 1xxx | Configuration and secrets |
| 2xxx | Database connection and privileges |
| 3xxx | Storage and repository |
| 4xxx | Keys |
| 5xxx | Capture and gaps |
| 6xxx | Restore |
| 7xxx | Verify and check |
| 8xxx | Agent and lease |
| 9xxx | Notifications |

| ID | Name |
| --- | --- |
| [PTM-1001](PTM-1001.md) | plaintext-secret-rejected |
| [PTM-1002](PTM-1002.md) | plaintext-secret-file-readable |
| [PTM-1003](PTM-1003.md) | config-invalid |
| [PTM-1004](PTM-1004.md) | secret-env-unset |
| [PTM-1005](PTM-1005.md) | secret-file-unreadable |
| [PTM-1006](PTM-1006.md) | secret-keychain-missing |
| [PTM-1007](PTM-1007.md) | secret-command-failed |
| [PTM-1008](PTM-1008.md) | config-not-found |

## Page template

Each page is `docs/errors/<ID>.md`, published at `https://pentymento.dev/errors/<ID>`:

```markdown
# PTM-NNNN: <short name>

<the message as printed, with placeholders>

## Cause

## Impact

## Fix
```
