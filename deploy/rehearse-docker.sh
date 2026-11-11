#!/usr/bin/env bash
# Real-docker check of the real image, compose file, listeners and cloudflared flags. Compose project
# finbot-rehearsal with dummy credentials. It never reads .env. finbot gets egress
# only so Google's public OIDC discovery works (the MCP routes are 503 without it),
# with api.ynab.com pinned to loopback so it can't reach YNAB. cloudflared runs on
# an internal network, so it can't reach Cloudflare. Usage: deploy/rehearse-docker.sh
set -uo pipefail
cd "$(dirname "$0")" || exit 1
curl_image=curlimages/curl:8.17.0
project=finbot-rehearsal
tmp=$(mktemp -d "${TMPDIR:-/tmp}/finbot-rehearse-docker.XXXXXX")
passed=0 failed=0
ok() { passed=$((passed + 1)); echo "  ok: $1"; }
bad() { failed=$((failed + 1)); echo "  FAIL: $1"; }
expect() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

cat >"$tmp/finbot.env" <<'EOF'
YNAB_ACCESS_TOKEN=rehearsal-dummy
YNAB_BUDGET_ID=00000000-0000-4000-8000-000000000000
GOOGLE_OAUTH_CLIENT_ID=rehearsal-dummy
GOOGLE_OAUTH_CLIENT_SECRET=rehearsal-dummy
MCP_ALLOWED_EMAILS=rehearsal@example.invalid
JEV_API_KEY=rehearsal-dummy
EOF
# A well-formed but dummy token, so cloudflared gets past parsing and fails only on the network.
fake=$(printf '{"a":"00000000000000000000000000000000","t":"11111111-1111-4111-8111-111111111111","s":"c2VjcmV0"}' | base64 | tr -d '\n')
printf 'TUNNEL_TOKEN=%s\n' "$fake" >"$tmp/tunnel.env"
cat >"$tmp/internal.yaml" <<'EOF'
networks:
  default:
    internal: true
EOF
cat >"$tmp/no-ynab.yaml" <<'EOF'
services:
  finbot:
    extra_hosts:
      - "api.ynab.com:127.0.0.1"
EOF
overlay=$tmp/internal.yaml
export FINBOT_ENV_FILE=$tmp/finbot.env TUNNEL_ENV_FILE=$tmp/tunnel.env GIT_SHA=rehearsal
dc() { docker compose -p "$project" --profile tunnel -f compose.yaml -f "$overlay" "$@"; }
cleanup() { dc down -v --remove-orphans >/dev/null 2>&1; rm -rf "$tmp"; }
trap cleanup EXIT

echo "== docker compose config"
cfg=$(dc config)
expect "no published ports" "$(grep -cE '^\s+(ports|published):' <<<"$cfg")" 0
expect "read_only on both" "$(grep -c 'read_only: true' <<<"$cfg")" 2
expect "cap_drop ALL on both" "$(grep -c -- '- ALL' <<<"$cfg")" 2
expect "no-new-privileges on both" "$(grep -c 'no-new-privileges:true' <<<"$cfg")" 2
expect "bounded logs on both" "$(grep -c 'max-size: 10m' <<<"$cfg")" 2
expect "cloudflared runs as nonroot" "$(grep -c 'user: 65532:65532' <<<"$cfg")" 1
expect "finbot runs in dry_run" "$(grep -c 'CLASSIFY_MODE: dry_run' <<<"$cfg")" 1
noprofile=$(docker compose -p "$project" -f compose.yaml config --services)
expect "without the profile only finbot exists" "$noprofile" finbot
absentfile=$(FINBOT_ENV_FILE=$tmp/finbot.env TUNNEL_ENV_FILE=/dev/null docker compose -p "$project" -f compose.yaml config --services 2>&1)
expect "a deploy with no tunnel.env (TUNNEL_ENV_FILE=/dev/null) still loads" "$absentfile" finbot

echo "== build and start"
overlay=$tmp/no-ynab.yaml
dc build finbot 2>&1 | tail -1
dc up -d finbot 2>&1 | tail -1
docker pull -q "$curl_image" >/dev/null
cid=$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=finbot)
[[ -n "$cid" ]] || { bad "finbot did not start"; docker logs --tail 20 "$(dc ps -aq finbot)" 2>&1 | cut -c1-200; exit 1; }
get() { docker run --rm --network "${project}_default" --read-only --cap-drop ALL --security-opt no-new-privileges:true "$curl_image" -sS -m 10 -H 'Host: finance.hoidlabs.site' -w '\n%{http_code}' "http://finbot:8080$1" 2>&1; }
# 503 until Google's discovery has succeeded.
for _ in $(seq 1 60); do r=$(get /mcp); [[ "${r##*$'\n'}" != 503 ]] && break; sleep 1; done

echo "== step 2 probes against the real server, on the compose network"
r=$(get /mcp); expect "/mcp status" "${r##*$'\n'}" 401
r=$(get /healthz); expect "/healthz on the public port" "${r##*$'\n'}" 404
r=$(get /.well-known/oauth-protected-resource); expect "metadata status" "${r##*$'\n'}" 200
if [[ "$r" =~ \"resource_name\"[[:space:]]*:[[:space:]]*\"finbot\" ]]; then ok "resource_name is finbot"; else bad "resource_name"; fi

echo "== healthcheck reaches /healthz on 127.0.0.1:8081"
msg=$(docker exec "$cid" finbot health 2>&1); rc=$?
echo "  finbot health: rc=$rc: $(cut -c1-120 <<<"$msg")"
# YNAB is unreachable, so no sync and 503 (exit 1) is the expected answer. Connection refused would mean a wrong HTTP_ADDR.
if grep -qiE 'refused|no such host' <<<"$msg"; then bad "health can't reach the listener"; else ok "health reached the listener"; fi
if grep -q 503 <<<"$msg"; then ok "health got the server's 503 (no sync possible here)"; else bad "health did not report 503"; fi
expect "tunnel token absent from finbot's env" "$(docker exec "$cid" env | grep -c TUNNEL_TOKEN)" 0
expect "MCP keys present in finbot's env" "$(docker exec "$cid" env | grep -cE '^(GOOGLE_OAUTH_CLIENT_ID|GOOGLE_OAUTH_CLIENT_SECRET|MCP_ALLOWED_EMAILS)=')" 3
expect "healthz not bound on the container interface" "$(docker run --rm --network "${project}_default" "$curl_image" -s -o /dev/null -m 5 -w '%{http_code}' http://finbot:8081/healthz 2>/dev/null)" 000

echo "== pinned claude CLI, with the finbot-data volume over /data and a read-only root"
expect "classify mode in finbot's env" "$(docker exec "$cid" printenv CLASSIFY_MODE)" dry_run
expect "finbot logged categorize enabled in dry_run" "$(docker logs "$cid" 2>&1 | grep '"categorize enabled"' | grep -c '"mode":"dry_run"')" 1
expect "ANTHROPIC_API_KEY absent from finbot's env" "$(docker exec "$cid" env | grep -c '^ANTHROPIC_API_KEY=')" 0
expect "claude --version in the running container" "$(docker exec "$cid" claude --version 2>&1 | cut -d' ' -f1)" 2.1.312
expect "CLAUDE_BIN is the pinned binary" "$(docker exec "$cid" sh -c 'readlink -f "$CLAUDE_BIN"')" /opt/claude/versions/2.1.312
expect "updates disabled in the image" "$(docker exec "$cid" printenv DISABLE_UPDATES)" 1
docker volume create "${project}-claude-check" >/dev/null
v=$(docker run --rm -v "${project}-claude-check:/data" --read-only --cap-drop ALL --security-opt no-new-privileges:true --entrypoint claude finbot:latest --version 2>&1)
docker volume rm "${project}-claude-check" >/dev/null
expect "claude --version with a fresh named volume at /data" "$(cut -d' ' -f1 <<<"$v")" 2.1.312

echo "== real cloudflared with the real flags and hardening"
dc down -v --remove-orphans >/dev/null 2>&1
overlay=$tmp/internal.yaml
dc up -d --no-deps cloudflared 2>&1 | tail -1
sleep 8
cf=$(docker ps -aq --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=cloudflared)
logs=$(docker logs "$cf" 2>&1)
if grep -qiE 'incorrect usage|flag provided but not defined' <<<"$logs"; then bad "cloudflared rejected its flags"; else ok "cloudflared accepted its flags"; fi
if grep -q '127.0.0.1:20241' <<<"$logs"; then ok "metrics on container loopback"; else bad "metrics address not in logs"; fi
if grep -qiE 'read-only file system|permission denied' <<<"$logs"; then bad "cloudflared hit the hardening"; else ok "no read-only or permission errors"; fi
expect "cloudflared uid" "$(docker inspect -f '{{.Config.User}}' "$cf")" 65532:65532
if grep -q "$fake" <<<"$logs"; then bad "token appeared in cloudflared logs"; else ok "token not in cloudflared logs"; fi

echo "passed=$passed failed=$failed"
[[ $failed == 0 ]]
