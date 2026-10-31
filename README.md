# servatum-cli

Report backup job results to [Servatum](https://servatum.io).

```sh
export SERVATUM_KEY=sv_ing_...        # ingest key (prefer env over --key: flags show in ps)
servatum run --job job_... -- restic backup /home /etc
servatum ping pt_...                   # or: ping --start / ping --fail [--log FILE|-]
```

- The wrapped command's output passes through untouched and its exit code is returned as-is.
- If Servatum can't be reached, the backup still runs and keeps its exit code. The report is retried 3 times (~10 s), then spooled to `/var/spool/servatum` (or `~/.local/state/servatum/spool`; `SERVATUM_SPOOL` overrides) and sent on the next run or by `servatum flush`. Spooled reports expire after 72 h; the spool is capped at 1,000 files / 10 MB. Rejected reports (bad key, bad job) and pings are never spooled.
- Recommended cron: `*/15 * * * * SERVATUM_KEY=sv_ing_... servatum flush --quiet`
- A `started` report is sent before each run (`--no-start` to skip).
- Command output is **not** sent unless you pass `--send-output` (last 8 KB).
- `--warn-exit-codes 24` reports those codes as `warn` instead of `fail`.
- `--dry-run` prints the reports instead of sending them.

Env: `SERVATUM_KEY`, `SERVATUM_JOB`, `SERVATUM_ENDPOINT` (default `https://api.servatum.io`).

## Develop

```sh
make test
go run ./cmd/mockapi            # local stand-in API on :8787, GET /_dump to inspect
make build                      # static linux amd64/arm64 binaries in dist/
```
