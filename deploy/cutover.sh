#!/usr/bin/env bash
# Moves finance.hoidlabs.site from the finbot-spike connector to finbot's own.
# Run from the repo root. Before step 3 a failure leaves the spike serving and
# changes nothing public; from step 3 on, any failure or exit runs rollback.sh.
# The tunnel token stays on hoid: it's copied there and only counts are reported.
# $dir and friends are fixed constants expanded locally on purpose:
# shellcheck disable=SC2029
set -euo pipefail
host=${HOST:-hoid}
timeout=${TUNNEL_TIMEOUT:-90}
dir=finbot
public_host=finance.hoidlabs.site
curl_image=curlimages/curl:8.17.0
rounds=20
interval=3 # 20 rounds over a minute

[[ -f .env && -x deploy/deploy.sh && -x deploy/rollback.sh ]] ||
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

# Prints the response body, then the status code on the last line.
internal_get() {
	ssh "$host" "docker run --rm --network ${dir}_default --read-only --cap-drop ALL --security-opt no-new-privileges:true $curl_image -sS -m 10 -H 'Host: $public_host' -w '\n%{http_code}' http://finbot:8080$1"
}
public_get() {
	curl -sS -m 10 -w '\n%{http_code}' "https://$public_host$1"
}

# check_site <fetcher>: /mcp is 401, the metadata names finbot exactly, /healthz is 404.
# The spike also answers 401 with the same issuer, so resource_name is what proves
# which stack replied. Bodies are never printed.
check_site() {
	local fetch=$1 resp
	resp=$("$fetch" /mcp) || { echo "/mcp request failed" >&2; return 1; }
	[[ "${resp##*$'\n'}" == 401 ]] || { echo "/mcp returned ${resp##*$'\n'}, want 401" >&2; return 1; }
	resp=$("$fetch" /.well-known/oauth-protected-resource) || { echo "metadata request failed" >&2; return 1; }
	[[ "${resp##*$'\n'}" == 200 ]] || { echo "metadata returned ${resp##*$'\n'}, want 200" >&2; return 1; }
	[[ "$resp" =~ \"resource_name\"[[:space:]]*:[[:space:]]*\"finbot\" ]] ||
		{ echo "metadata resource_name is not finbot" >&2; return 1; }
	resp=$("$fetch" /healthz) || { echo "/healthz request failed" >&2; return 1; }
	[[ "${resp##*$'\n'}" == 404 ]] || { echo "/healthz returned ${resp##*$'\n'}, want 404" >&2; return 1; }
}

step=2
say "check finbot from inside its network"
ssh "$host" "docker pull -q $curl_image >/dev/null" || die "could not pull the probe image; the spike is still serving"
check_site internal_get || die "finbot answered wrongly; the spike is still serving"

# From here on, any failure or exit hands the site back to the spike.
on_exit() {
	local status=$?
	trap - EXIT INT TERM HUP
	((status)) || status=1
	echo "cutover: step $step did not finish; rolling back" >&2
	if ! HOST="$host" TUNNEL_TIMEOUT="$timeout" deploy/rollback.sh; then
		echo "cutover: ROLLBACK FAILED: check hoid and re-run deploy/rollback.sh" >&2
	fi
	exit "$status"
}
trap on_exit EXIT
trap 'exit 130' INT TERM HUP

step=3
say "start finbot's connector"
ssh "$host" bash -s -- "$timeout" "$dir" <<'REMOTE' || die "finbot's connector did not register"
set -euo pipefail
dir=$2
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
(cd "$dir/src/deploy" && FINBOT_ENV_FILE="$HOME/$dir/finbot.env" TUNNEL_ENV_FILE="$HOME/$dir/tunnel.env" \
	docker compose -p finbot --profile tunnel up -d --no-deps cloudflared)
id=$(docker ps -aq --filter label=com.docker.compose.project=finbot --filter label=com.docker.compose.service=cloudflared)
[[ -n "$id" ]] || { echo "cutover: no finbot cloudflared container" >&2; exit 1; }
deadline=$((SECONDS + $1))
while ((SECONDS < deadline)); do
	# Held in a variable (a grep -q pipe would SIGPIPE under pipefail) and never printed.
	logs=$(docker logs --since "$since" "$id" 2>&1 || true)
	[[ "$logs" == *"Registered tunnel connection"* ]] && exit 0
	sleep 2
done
echo "cutover: no registered connection after $1s" >&2
exit 1
REMOTE

step=4
say "stop the spike's connector (its finbot and volume stay)"
ssh "$host" bash -s <<'REMOTE' || die "could not stop the spike's connector"
set -euo pipefail
id=$(docker ps -q --filter label=com.docker.compose.project=finbot-spike --filter label=com.docker.compose.service=cloudflared)
[[ -n "$id" && "$id" != *$'\n'* ]] || { echo "cutover: expected one running spike connector" >&2; exit 1; }
docker stop "$id" >/dev/null
REMOTE

step=5
say "check the public site $rounds times over a minute"
for ((i = 1; i <= rounds; i++)); do
	check_site public_get || die "public check $i of $rounds failed"
	((i == rounds)) || sleep "$interval"
done

step=6
say "create the marker"
ssh "$host" "touch $dir/tunnel-active" || die "could not create the marker"
trap - EXIT INT TERM HUP
echo "cutover: done; finbot's connector serves https://$public_host. Connectors must reconnect."
