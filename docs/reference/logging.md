# Logs and progress

## Logs

| Process | Where | Level | Default format |
| --- | --- | --- | --- |
| `pentym agent` (the service) | stdout, for journald and Kubernetes | info | JSON |
| Every other command | stderr, only with `--debug` | debug | text in a terminal, JSON otherwise |

`--log-format text` or `--log-format json` overrides the format. Results and `--json` documents are always on stdout, so logs never mix with them in one-shot commands.

Every line carries four fields, empty when they don't apply:

| Field | Meaning |
| --- | --- |
| `target` | The configured target |
| `job` | The operation, such as `backup` or `verify` |
| `run_id` | A 12-character ID for one run of a job, shared with the output of child processes and the journal |
| `component` | The part of pentym that wrote the line |

Secrets never appear in logs: they are shown as `[redacted]`, or as their reference, such as `env:PGPASSWORD`.

## Progress

Long operations report bytes processed, rate, time remaining and deduplication so far, on stderr:

| Where stderr goes | Progress |
| --- | --- |
| A terminal | One line, redrawn about four times a second and erased when the operation ends |
| Anywhere else | An info log line every 30 seconds, and one when the operation ends |
| Anywhere, with `--json` | One JSON event per line every second, and a final `done` event |

`-q` turns progress off.

A `--json` progress event:

```json
{"time":"2026-11-25T13:00:04Z","event":"progress","bytes":1073741824,"total":53687091200,"bytes_per_second":107374182,"eta_seconds":490,"dedup_ratio":3.14}
```

`total` and `eta_seconds` are left out when the size of the stream isn't known in advance; `dedup_ratio` is left out until it is known. The last event has `"event":"done"`.
