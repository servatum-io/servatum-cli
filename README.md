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

## Hooks (`hooks/`)

Generic POSIX `sh` scripts for Linux servers. Key goes in `/etc/servatum/env` (`SERVATUM_KEY=sv_ing_...`, mode 600). Each exits 0 so a reporting problem never breaks the backup tool (`SERVATUM_HOOK_STRICT=1` to change that).

| Script | Use when | Example |
|---|---|---|
| `post-hook.sh <job> <status\|exit_code> [msg]` | the backup tool can run a script after a job | `post-hook.sh job_x "$EXIT_CODE"` |
| `cron-wrap.sh <job> <cmd...>` | the backup is a cron line | `0 1 * * * cron-wrap.sh job_x /usr/local/bin/backup.sh` |
| `log-check.sh <job> <log> [--ok RE] [--fail RE] [--max-age MIN]` | the tool only writes a log | `30 6 * * * log-check.sh job_x /var/log/backup.log` |

`servatum report --job … --status ok|warn|fail [--exit-code N] [--message …]` is what the hooks call; use it directly from any script.

## cPanel & WHM backups

Registers a [Standardized Hook](https://api.docs.cpanel.net/guides/guide-to-standardized-hooks/) on `System::Backup` (`pre` + `post`, runs as root, cannot block the backup). Run as root on the cPanel server:

```sh
install -m 755 servatum-linux-amd64 /usr/local/servatum/servatum
SERVATUM_KEY=sv_ing_... /usr/local/servatum/servatum install whm --job job_...   # add --dry-run to only log
/usr/local/cpanel/bin/manage_hooks list | grep -A4 whm-backup                     # check
/usr/local/servatum/servatum uninstall whm                                        # remove
```

- `pre` records the start and sends `started`. `post` checks the run and sends `ok` / `warn` / `fail`, logging to `/var/log/servatum-whm.log`.
- What is checked: backup settings and accounts from WHM API 1 (`backup_config_get`, `listaccts`: accounts with backups off, and suspended accounts when those are excluded, are not expected); the run's archives under `<backupdir>/YYYY-MM-DD`, `weekly/…`, `monthly/…` (`accounts/<user>.tar.gz|.tar|<user>/`).
- `fail`: no backup directory for the run, no archives, an expected account missing, or a sampled archive that does not open. `warn`: an archive under 10 KB, or the sample check ran out of time.
- Test-read: 3 random archives (max 5% of them, at least 1), read end to end, 120 s cap (`--sample`, `--sample-max-pct`, `--verify-timeout`). Incremental (directory) backups are not test-read.
- The report carries `kind: whm.backup.v1` and a `details` object; the problem list is capped at 100 entries to stay under the 16 KB body limit.
- Try it without a hook: `servatum whm report --dry-run --day 2026-11-08`.
