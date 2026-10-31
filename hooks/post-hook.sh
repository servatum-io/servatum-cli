#!/bin/sh
# Generic post-backup hook: call it from any backup tool that can run a
# script when a job finishes.
#
#   post-hook.sh <job_id> <status|exit_code> [message]
#
# status: ok | warn | fail (also accepts success/succeeded/completed,
# warning/partial, failed/failure/error). A number is an exit code:
# 0 = ok, anything else = fail. Extra codes can be mapped to warn with
# SERVATUM_WARN_CODES="24 25".
#
# Always exits 0 so a reporting problem never breaks the backup tool's
# hook chain; set SERVATUM_HOOK_STRICT=1 to pass servatum's exit code on.
set -u
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=hooks/_common.sh
. "$here/_common.sh"

if [ $# -lt 2 ]; then
	echo "usage: $0 <job_id> <status|exit_code> [message]" >&2
	exit 2
fi
job=$1
raw=$2
msg=${3:-}

code=""
case "$raw" in
'' | *[!0-9]*)
	lc=$(printf '%s' "$raw" | tr '[:upper:]' '[:lower:]')
	case "$lc" in
	ok | success | succeeded | completed | done) status=ok ;;
	warn | warning | partial) status=warn ;;
	fail | failed | failure | error | errored) status=fail ;;
	*)
		echo "servatum-hook: unknown status '$raw', reporting fail" >&2
		status=fail
		;;
	esac
	;;
*)
	code=$raw
	status=fail
	[ "$code" -eq 0 ] && status=ok
	for w in ${SERVATUM_WARN_CODES:-}; do
		[ "$code" = "$w" ] && status=warn
	done
	;;
esac

servatum_load_env
servatum_need_bin || exit 0

set -- report --job "$job" --status "$status" --quiet
[ -n "$code" ] && set -- "$@" --exit-code "$code"
[ -n "$msg" ] && set -- "$@" --message "$msg"
"$SERVATUM_BIN" "$@"
rc=$?
[ "${SERVATUM_HOOK_STRICT:-0}" = 1 ] && exit "$rc"
exit 0
