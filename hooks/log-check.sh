#!/bin/sh
# For backup tools that only write a log file. Run it from cron after the
# backup window:
#
#   log-check.sh <job_id> <logfile> [--ok REGEX] [--fail REGEX] [--max-age MINUTES]
#
#   fail  if the log is missing, older than --max-age (default 1500 = 25 h),
#         or any line matches --fail (default: error|failed|fatal, case-insensitive)
#   ok    if a line matches --ok (default: success|completed|finished)
#   warn  otherwise (log is fresh but says neither)
#
# Log content is never sent; the message only says which rule decided.
set -u
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=hooks/_common.sh
. "$here/_common.sh"

if [ $# -lt 2 ]; then
	echo "usage: $0 <job_id> <logfile> [--ok REGEX] [--fail REGEX] [--max-age MINUTES]" >&2
	exit 2
fi
job=$1
log=$2
shift 2
ok_re='success|completed|finished'
fail_re='error|failed|fatal'
max_age=1500
while [ $# -gt 0 ]; do
	case "$1" in
	--ok) ok_re=$2; shift 2 ;;
	--fail) fail_re=$2; shift 2 ;;
	--max-age) max_age=$2; shift 2 ;;
	*) echo "log-check: unknown option $1" >&2; exit 2 ;;
	esac
done

if [ ! -r "$log" ]; then
	status=fail
	msg="log file missing or unreadable"
elif [ -z "$(find "$log" -mmin "-$max_age" 2>/dev/null)" ]; then
	status=fail
	msg="log not updated in the last $max_age minutes"
elif grep -Eiq -- "$fail_re" "$log"; then
	status=fail
	msg="log matched the failure pattern"
elif grep -Eiq -- "$ok_re" "$log"; then
	status=ok
	msg="log matched the success pattern"
else
	status=warn
	msg="log is fresh but matched neither pattern"
fi

servatum_load_env
servatum_need_bin || exit 0
"$SERVATUM_BIN" report --job "$job" --status "$status" --message "$msg" --quiet
rc=$?
[ "${SERVATUM_HOOK_STRICT:-0}" = 1 ] && exit "$rc"
exit 0
