#!/usr/bin/env bash
# Moves finance.hoidlabs.site from the finbot-spike connector to finbot's own.
# Run from the repo root. Before step 3 a failure leaves the spike serving and
# changes nothing public. Steps 3 to 6 and their rollback trap run on hoid,
# detached (cutover-remote.sh); this side only starts them and polls the log.
# The tunnel token stays on hoid: it's copied there and only counts are reported.
#
# Usage: deploy/cutover.sh            run the cutover
#        deploy/cutover.sh --watch    re-attach to the log, such as after the Mac slept
# Exit:  0 cut over, 1 failed and rolled back (or stopped before step 3),
#        2 rollback failed, 3 outcome unknown (hoid carries on; use --watch)
# $dir and friends are fixed constants expanded locally on purpose:
# shellcheck disable=SC2029
set -euo pipefail
host=${HOST:-hoid}
timeout=${TUNNEL_TIMEOUT:-90}
poll_timeout=${POLL_TIMEOUT:-600}
dir=finbot
# Short timeouts so a dead link fails the poll quickly instead of hanging it.
poll_opts=(-o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=2)

# watch_log prints new lines of ~/finbot/cutover.log until its RESULT line, and
# exits with the outcome. Failed polls are expected and only retried: hoid doesn't
# need this side to finish.
watch_log() {
	local seen=0 out deadline=$((SECONDS + poll_timeout))
	while ((SECONDS < deadline)); do
		if out=$(ssh "${poll_opts[@]}" "$host" "tail -n +$((seen + 1)) $dir/cutover.log" 2>/dev/null) && [[ -n "$out" ]]; then
			printf '%s\n' "$out"
			seen=$((seen + $(printf '%s\n' "$out" | wc -l)))
			case "$out" in
			*"RESULT ok"*)
				echo "cutover: done; finbot's connector serves https://finance.hoidlabs.site. Connectors must reconnect."
				exit 0 ;;
			*"RESULT rolled-back"*)
				echo "cutover: FAILED and rolled back; the spike's connector is serving" >&2
				exit 1 ;;
			*"RESULT rollback-failed"*)
				echo "cutover: ROLLBACK FAILED: check hoid and run deploy/rollback.sh" >&2
				exit 2 ;;
			esac
		fi
		sleep 3
	done
	echo "cutover: outcome unknown after ${poll_timeout}s without a RESULT line. hoid carries on by itself; run deploy/cutover.sh --watch" >&2
	exit 3
}

if [[ "${1:-}" == --watch ]]; then watch_log; fi

[[ -f .env && -x deploy/deploy.sh ]] ||
	{ echo "cutover: run from the repo root, with .env" >&2; exit 1; }

step=0
say() { echo "cutover: step $step: $*"; }
die() { echo "cutover: FAILED at step $step: $*" >&2; exit 1; }

# Step 0: nothing outside ~/finbot/tunnel.env changes, and that only after it validates.
say "preflight and copy tunnel.env"
ssh "$host" bash -s <<'REMOTE' || die "preflight failed; nothing changed"
set -euo pipefail
fail() { echo "cutover: $*" >&2; exit 1; }
connector() {
	docker ps -aq --filter label=com.docker.compose.project="$1" --filter label=com.docker.compose.service=cloudflared
}
[[ ! -e finbot/tunnel-active ]] || fail "~/finbot/tunnel-active exists: already cut over"
spike=$(connector finbot-spike)
[[ -n "$spike" && "$spike" != *$'\n'* ]] || fail "expected one finbot-spike cloudflared container"
[[ "$(docker inspect -f '{{.State.Status}}' "$spike")" == running ]] || fail "the spike's connector is not running"
for id in $(connector finbot); do
	[[ "$(docker inspect -f '{{.State.Status}}' "$id")" != running ]] || fail "finbot's connector is already running"
done
src=finbot-spike/mcp/tunnel.env
[[ -f "$src" ]] || fail "~/$src is missing"
mkdir -p finbot && chmod 700 finbot
tmp=finbot/tunnel.env.new
trap 'rm -f "$tmp"' EXIT
(umask 077 && cp "$src" "$tmp")
chmod 600 "$tmp"
[[ "$(stat -c %a "$tmp")" == 600 ]] || fail "tunnel.env copy is not 0600"
[[ "$(grep -c '^TUNNEL_TOKEN=' "$tmp" || true)" == 1 ]] || fail "tunnel.env must have exactly one TUNNEL_TOKEN= line"
grep -q '^TUNNEL_TOKEN=.' "$tmp" || fail "tunnel.env has an empty TUNNEL_TOKEN"
mv "$tmp" finbot/tunnel.env
trap - EXIT
REMOTE

step=1
say "deploy finbot with MCP on and no connector"
HOST="$host" deploy/deploy.sh || die "deploy failed; the spike is still serving"

step=2
say "check finbot from inside its network, and that hoid can reach the public site"
ssh "$host" "bash $dir/src/deploy/cutover-remote.sh precheck" || die "finbot answered wrongly; the spike is still serving"

# From here on hoid works alone, so a dropped ssh or a sleeping Mac can't strand it.
step=3
say "start steps 3 to 6 on hoid, detached"
start_rc=0
ssh "$host" "bash $dir/src/deploy/cutover-remote.sh start $timeout" || start_rc=$?
# 255 is ssh itself: the start may or may not have landed, and the log will say.
if ((start_rc != 0 && start_rc != 255)); then
	die "could not start on hoid; the spike is still serving"
fi
watch_log
