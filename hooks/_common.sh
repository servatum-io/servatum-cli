# shellcheck shell=sh
# Shared helpers for Servatum hook scripts. Sourced, not executed.
# Credentials: put SERVATUM_KEY=sv_ing_... in /etc/servatum/env (mode 600),
# or export it in the environment. The scripts never print the key.

SERVATUM_BIN=${SERVATUM_BIN:-servatum}
SERVATUM_ENV_FILE=${SERVATUM_ENV_FILE:-/etc/servatum/env}

servatum_load_env() {
	if [ -z "${SERVATUM_KEY:-}" ] && [ -r "$SERVATUM_ENV_FILE" ]; then
		# Warn if the env file is readable by group/others.
		perms=$(find "$SERVATUM_ENV_FILE" -maxdepth 0 -perm /044 2>/dev/null)
		if [ -n "$perms" ]; then
			echo "servatum-hook: WARNING $SERVATUM_ENV_FILE is readable by group/others; chmod 600 it" >&2
		fi
		set -a
		# shellcheck disable=SC1090
		. "$SERVATUM_ENV_FILE"
		set +a
	fi
}

servatum_need_bin() {
	if ! command -v "$SERVATUM_BIN" >/dev/null 2>&1; then
		echo "servatum-hook: WARNING '$SERVATUM_BIN' not found in PATH; nothing reported" >&2
		return 1
	fi
}
