#!/bin/sh
# Put in front of any cron backup line:
#
#   0 1 * * * /usr/local/lib/servatum/hooks/cron-wrap.sh job_xxx /usr/local/bin/backup.sh --full
#
# Loads SERVATUM_KEY from /etc/servatum/env, then runs the command through
# 'servatum run'. The command's output and exit code are passed through
# unchanged; if servatum is missing, the command still runs.
set -u
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=hooks/_common.sh
. "$here/_common.sh"

if [ $# -lt 2 ]; then
	echo "usage: $0 <job_id> <command> [args...]" >&2
	exit 2
fi
job=$1
shift

servatum_load_env
if ! servatum_need_bin; then
	exec "$@"
fi
# shellcheck disable=SC2086
exec "$SERVATUM_BIN" run --job "$job" ${SERVATUM_RUN_FLAGS:-} -- "$@"
