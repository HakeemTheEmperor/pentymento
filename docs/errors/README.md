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

No message IDs are registered yet.

## Page template

Each page is `docs/errors/<ID>.md`, published at `https://pentymento.dev/errors/<ID>`:

```markdown
# PTM-NNNN: <short name>

<the message as printed, with placeholders>

## Cause

## Impact

## Fix
```
