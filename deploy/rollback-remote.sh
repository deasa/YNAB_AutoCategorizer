#!/usr/bin/env bash
# Runs on hoid. Hands finance.hoidlabs.site back to the finbot-spike connector.
# Order: start the spike's connector, wait until it's registered, then stop
# finbot's and remove the marker, so there is never a moment with no connector.
# If the spike can't take over, finbot's connector is left running.
# Usage: rollback-remote.sh <timeout-seconds> [--from-cutover]
set -euo pipefail
cd "$HOME"
timeout=$1
from_cutover=${2:-}
pidfile=finbot/cutover.pid
fail() { echo "rollback: FAILED: $*" >&2; exit 1; }
connector() {
	docker ps -aq --filter label=com.docker.compose.project="$1" --filter label=com.docker.compose.service=cloudflared
}
is_running() { [[ "$(docker inspect -f '{{.State.Status}}' "$1")" == running ]]; }

# A detached cutover has its own rollback trap; two rollers could race its step 4.
if [[ "$from_cutover" != --from-cutover && -f "$pidfile" ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
	fail "a cutover is running on this host; kill -TERM \$(cat ~/$pidfile) makes it roll back itself. Nothing changed"
fi

spike=$(connector finbot-spike)
[[ -n "$spike" && "$spike" != *$'\n'* ]] || fail "expected one finbot-spike cloudflared container; nothing changed"

if is_running "$spike"; then
	echo "rollback: spike connector already running"
else
	docker start "$spike" >/dev/null
	# StartedAt has nanosecond precision, so the spike's old registration lines can't match.
	since=$(docker inspect -f '{{.State.StartedAt}}' "$spike")
	deadline=$((SECONDS + timeout))
	registered=
	while ((SECONDS < deadline)); do
		# Held in a variable (a grep -q pipe would SIGPIPE under pipefail) and never printed.
		logs=$(docker logs --since "$since" "$spike" 2>&1 || true)
		if [[ "$logs" == *"Registered tunnel connection"* ]]; then registered=1; break; fi
		sleep 2
	done
	[[ -n "$registered" ]] || fail "spike connector did not register within ${timeout}s; finbot's connector left as it was"
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
