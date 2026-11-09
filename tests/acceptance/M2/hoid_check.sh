#!/usr/bin/env bash
# Black-box check of hoid deployment health, permissions, hardening, logs and Slack status (scope 2).
# Never prints env secret values, UUIDs, tokens or emails.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
source "$HERE/lib.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

ssh -o BatchMode=yes hoid bash -s >"$tmp/remote.out" <<'REMOTE'
set -euo pipefail
C=finbot-finbot-1
CF=finbot-cloudflared-1

echo "== finbot compose ps"
docker ps -a --filter label=com.docker.compose.project=finbot --format '{{.Names}} | {{.Status}} | ports=[{{.Ports}}] | {{.Image}}'

echo "== finbot-spike compose ps"
docker ps -a --filter label=com.docker.compose.project=finbot-spike --format '{{.Names}} | {{.Status}} | ports=[{{.Ports}}]'

echo "== finbot inspect"
docker inspect "$C" --format 'user={{.Config.User}}
readonly={{.HostConfig.ReadonlyRootfs}}
privileged={{.HostConfig.Privileged}}
capdrop={{.HostConfig.CapDrop}}
capadd={{.HostConfig.CapAdd}}
secopt={{.HostConfig.SecurityOpt}}
portbindings={{.HostConfig.PortBindings}}
publishall={{.HostConfig.PublishAllPorts}}
restart={{.HostConfig.RestartPolicy.Name}}
restartcount={{.RestartCount}}
started={{.State.StartedAt}}
health={{.State.Health.Status}}
healthfails={{.State.Health.FailingStreak}}'

echo "== finbot env names"
docker inspect "$C" --format '{{range .Config.Env}}{{println .}}{{end}}' | sed 's/=.*//' | sort | tr '\n' ' '; echo

echo "== finbot safe env values"
docker inspect "$C" --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep -E '^(IMPORT_ENABLED|IMPORT_INTERVAL|SYNC_INTERVAL|HTTP_ADDR|MCP_LISTEN_ADDR|MCP_PUBLIC_URL|FINBOT_DB_PATH|TZ)=' | sort

echo "== finbot tunnel token present?"
docker inspect "$C" --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -c '^TUNNEL_TOKEN=' || true

echo "== finbot id & health"
docker exec "$C" id
docker exec "$C" finbot health && echo "finbot_health_exit=0"

echo "== finbot /data permissions"
docker exec "$C" sh -c 'stat -c "%a %u:%g %n" /data /data/finbot.db*'

echo "== cloudflared inspect"
docker inspect "$CF" --format 'user={{.Config.User}}
args={{.Args}}
readonly={{.HostConfig.ReadonlyRootfs}}
privileged={{.HostConfig.Privileged}}
capdrop={{.HostConfig.CapDrop}}
capadd={{.HostConfig.CapAdd}}
secopt={{.HostConfig.SecurityOpt}}
portbindings={{.HostConfig.PortBindings}}
restartcount={{.RestartCount}}
status={{.State.Status}}'

echo "== cloudflared registered count"
docker logs "$CF" 2>&1 | grep -c 'Registered tunnel connection' || true

echo "== host ~/finbot files"
stat -c '%a %n' ~/finbot ~/finbot/finbot.env ~/finbot/tunnel.env ~/finbot/cutover.log ~/finbot/tunnel-active
tail -n 1 ~/finbot/cutover.log
REMOTE

ssh -o BatchMode=yes hoid 'docker logs finbot-finbot-1 2>&1' >"$tmp/finbot.log"
ssh -o BatchMode=yes hoid 'cat ~/finbot/cutover.log' >"$tmp/cutover.log"

echo "== secret check on remote state output"
sc_state=$(secret_count "$tmp/remote.out")
echo "$sc_state"
[[ "$sc_state" == *"exact_value_lines=0 token_shaped_lines=0 uuid_lines=0 email_lines=0" ]] || {
  echo "FAIL: remote.out matched secret/uuid/email pattern; refusing to print" >&2
  exit 1
}
cat "$tmp/remote.out"

echo "== secret check on finbot logs"
sc_logs=$(secret_count "$tmp/finbot.log")
echo "$sc_logs"
[[ "$sc_logs" == *"exact_value_lines=0 token_shaped_lines=0 uuid_lines=0 email_lines=0" ]] || {
  echo "FAIL: finbot.log matched secret/uuid/email pattern" >&2
  exit 1
}

echo "== secret check on cutover.log"
sc_cut=$(secret_count "$tmp/cutover.log")
echo "$sc_cut"
[[ "$sc_cut" == *"exact_value_lines=0 token_shaped_lines=0 uuid_lines=0 email_lines=0" ]] || {
  echo "FAIL: cutover.log matched secret/uuid/email pattern" >&2
  exit 1
}

echo "== finbot log summary"
echo "total_lines=$(wc -l < "$tmp/finbot.log" | tr -d ' ')"
echo "warn_lines=$(grep -c '"level":"WARN"' "$tmp/finbot.log" || true)"
echo "error_lines=$(grep -c '"level":"ERROR"' "$tmp/finbot.log" || true)"
echo "import_enabled_lines=$(grep -c '"msg":"import enabled"' "$tmp/finbot.log" || true)"
echo "mcp_serving_lines=$(grep -c '"msg":"mcp serving"' "$tmp/finbot.log" || true)"
echo "oauth_prune_lines=$(grep -c '"msg":"oauth prune"' "$tmp/finbot.log" || true)"
echo "sync_tick_lines=$(grep -c '"msg":"sync tick"' "$tmp/finbot.log" || true)"
echo "import_tick_lines=$(grep -c '"msg":"import tick"' "$tmp/finbot.log" || true)"

if [[ -f "$ENV_FILE" ]] && grep -q '^SLACK_BOT_TOKEN=.' "$ENV_FILE"; then
  echo "== slack status check"
  SLACK_TOKEN=$(grep '^SLACK_BOT_TOKEN=' "$ENV_FILE" | sed -E 's/^[^=]+=//; s/[[:space:]]+#.*$//; s/^"(.*)"$/\1/')
  SLACK_BOT_TOKEN="$SLACK_TOKEN" "$HERE/../M1/slack_status.py" --text >"$tmp/slack.out"
  sc_slack=$(secret_count "$tmp/slack.out")
  echo "$sc_slack"
  [[ "$sc_slack" == *"exact_value_lines=0 token_shaped_lines=0 uuid_lines=0 email_lines=0" ]] || {
    echo "FAIL: slack.out matched secret/uuid/email pattern" >&2
    exit 1
  }
  echo "slack_import_on=$(grep -c '\*Import:\* on' "$tmp/slack.out" || true)"
fi
