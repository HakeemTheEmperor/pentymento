# Metrics

pentym exposes Prometheus metrics at `/metrics` on the agent's control socket and, when enabled, on its health port. Labels are per target and job, never per table or collection.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `pentym_capture_lag_seconds` | gauge | `target` | Seconds between the newest change in the database and the newest change captured |
| `pentym_pg_slot_retained_wal_bytes` | gauge | `target` | Bytes of WAL the Postgres replication slot makes the server retain |
| `pentym_mongo_oplog_window_seconds` | gauge | `target` | Seconds of history in the MongoDB oplog: how long capture can stop before entries are lost |
| `pentym_pitr_window_seconds` | gauge | `target` | Length of the actual point-in-time recovery window, gaps included |
| `pentym_last_success_timestamp_seconds` | gauge | `target`, `job` | Unix time of the last successful run of a job |
| `pentym_job_duration_seconds` | histogram | `job` | Duration of jobs |
| `pentym_job_failures_total` | counter | `target`, `job` | Failed job runs; `job="capture"` counts capture failures and gaps |
| `pentym_repo_bytes` | gauge | `kind` | Bytes stored in the repository, by kind |
| `pentym_dedup_ratio` | gauge | — | Logical snapshot bytes per byte stored after deduplication |
| `pentym_backend_request_duration_seconds` | histogram | `op` | Duration of storage backend requests |
| `pentym_backend_request_errors_total` | counter | `op` | Failed storage backend requests |
| `pentym_upload_queue_depth` | gauge | — | Objects waiting to be uploaded |
| `pentym_events_detected_total` | counter | `type` | Destructive events detected, by type |
| `pentym_lease_held` | gauge | `target` | 1 while this agent holds the target's capture lease, otherwise 0 |
| `pentym_verify_last_result` | gauge | `target`, `level` | Result of the last verify at a level: 1 passed, 0 failed |

Histogram buckets:

- `pentym_job_duration_seconds`: 1, 5, 15 and 30 seconds; 1, 5, 15 and 30 minutes; 1, 2, 4, 8 and 24 hours.
- `pentym_backend_request_duration_seconds`: 5 ms to 2 minutes (0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60 and 120 seconds).

## Limitations

- A metric appears in a scrape only once it has a value, so a new agent shows few metrics until its first jobs run.
