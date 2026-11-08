#!/usr/bin/env bash
# Hands finance.hoidlabs.site back to the finbot-spike connector. Safe to re-run.
# Order: start the spike's connector, wait until it's registered, then stop
# finbot's and remove the marker, so there is never a moment with no connector.
# If the spike can't take over, finbot's connector is left running.
set -euo pipefail
host=${HOST:-hoid}
timeout=${TUNNEL_TIMEOUT:-90}

ssh "$host" bash -s -- "$timeout" <<'REMOTE'
set -euo pipefail
fail() { echo "rollback: FAILED: $*" >&2; exit 1; }
connector() {
	docker ps -aq --filter label=com.docker.compose.project="$1" --filter label=com.docker.compose.service=cloudflared
}
is_running() { [[ "$(docker inspect -f '{{.State.Status}}' "$1")" == running ]]; }

spike=$(connector finbot-spike)
[[ -n "$spike" && "$spike" != *$'\n'* ]] || fail "expected one finbot-spike cloudflared container; nothing changed"

if is_running "$spike"; then
	echo "rollback: spike connector already running"
else
	# --since matters: the spike's old logs already contain the registration line.
	since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	docker start "$spike" >/dev/null
	deadline=$((SECONDS + $1))
	registered=
	while ((SECONDS < deadline)); do
		# Held in a variable (a grep -q pipe would SIGPIPE under pipefail) and never printed.
		logs=$(docker logs --since "$since" "$spike" 2>&1 || true)
		if [[ "$logs" == *"Registered tunnel connection"* ]]; then registered=1; break; fi
		sleep 2
	done
	[[ -n "$registered" ]] || fail "spike connector did not register within $1s; finbot's connector left as it was"
	echo "rollback: spike connector registered"
fi

finbot=$(connector finbot)
if [[ -n "$finbot" ]]; then
	# shellcheck disable=SC2086 # one id per line
	docker stop $finbot >/dev/null
	for id in $finbot; do
		! is_running "$id" || fail "finbot's connector is still running"
	done
fi
rm -f finbot/tunnel-active
is_running "$spike" || fail "spike connector is not running"
echo "rollback: done; the spike's connector is the only one running"
REMOTE
