#!/usr/bin/env bash
# Deploys finbot to hoid:~/finbot as compose project finbot. Run from the repo root.
# Secrets are streamed over ssh stdin and written 0600; they never touch argv or stdout.
# $dir is a fixed constant expanded locally on purpose:
# shellcheck disable=SC2029
set -euo pipefail
host=${HOST:-hoid}
dir=finbot # relative to the remote home; ~/finbot/.env there is never read
timeout=${HEALTH_TIMEOUT:-120}

[[ -f .env && -f deploy/compose.yaml ]] || { echo "deploy: run from the repo root, with .env" >&2; exit 1; }
# The Google, allowlist, Slack, Jev and claude keys are required too: without them
# MCP, Socket Mode or categorization is silently off while the healthcheck still passes.
for key in YNAB_ACCESS_TOKEN YNAB_BUDGET_ID GOOGLE_OAUTH_CLIENT_ID GOOGLE_OAUTH_CLIENT_SECRET MCP_ALLOWED_EMAILS \
	SLACK_BOT_TOKEN SLACK_APP_TOKEN SLACK_USERS JEV_API_KEY CLAUDE_CODE_OAUTH_TOKEN; do
	grep -E "^${key}=" .env | grep -qvE "^${key}=[[:space:]]*(\"\"|'')?[[:space:]]*\$" ||
		{ echo "deploy: .env has no value for ${key}" >&2; exit 1; }
done
# An API key would take precedence over the claude OAuth token. It is never forwarded,
# but a .env that sets one is a mistake worth stopping on.
if grep -E '^[[:space:]]*(export[[:space:]]+)?ANTHROPIC_API_KEY=' .env |
	grep -qvE "ANTHROPIC_API_KEY=[[:space:]]*(\"\"|'')?[[:space:]]*\$"; then
	echo "deploy: .env sets ANTHROPIC_API_KEY; remove it (finbot uses CLAUDE_CODE_OAUTH_TOKEN)" >&2
	exit 1
fi

# Ship only committed, git-tracked files (never untracked or ignored ones such
# as .claude/ or .env.*), so the deploy is exactly HEAD.
git diff --quiet HEAD -- || { echo "deploy: tracked files have uncommitted changes; commit first" >&2; exit 1; }
sha=$(git rev-parse --short HEAD) # git archive ships no .git, so the build can't work this out itself
ssh "$host" "mkdir -p $dir && chmod 700 $dir"

# The marker decides whether this host runs finbot's connector. Print the answer
# rather than use test's exit code, so an ssh failure can't read as "no marker".
marker=$(ssh "$host" "if [ -e $dir/tunnel-active ]; then echo present; else echo absent; fi")
compose_tunnel="TUNNEL_ENV_FILE=/dev/null"
compose_profile=""
if [[ "$marker" == present ]]; then
	# Checked before anything ships. Only counts and the mode cross the wire, never the value.
	ssh "$host" "test -f $dir/tunnel.env && test \"\$(stat -c %a $dir/tunnel.env)\" = 600 && test \"\$(grep -c '^TUNNEL_TOKEN=' $dir/tunnel.env)\" = 1 && grep -q '^TUNNEL_TOKEN=.' $dir/tunnel.env" ||
		{ echo "deploy: FAILED: ~/$dir/tunnel-active exists but ~/$dir/tunnel.env is missing, not 0600, or lacks exactly one TUNNEL_TOKEN= line" >&2; exit 1; }
	compose_tunnel="TUNNEL_ENV_FILE=\$HOME/$dir/tunnel.env"
	compose_profile="--profile tunnel"
elif [[ "$marker" != absent ]]; then
	echo "deploy: could not read the tunnel marker on $host" >&2
	exit 1
fi

git archive --format=tar HEAD -- . ':!spikes' ':!docs' ':!.env*' ':!*.env' ':!**/*.env' |
	ssh "$host" "rm -rf $dir/src.new && mkdir $dir/src.new && tar -x -C $dir/src.new && rm -rf $dir/src && mv $dir/src.new $dir/src"

# finbot.env: only the keys finbot needs, from the local .env.
grep -E '^(YNAB_ACCESS_TOKEN|YNAB_BUDGET_ID|HEALTHCHECK_PING_URL|BACKUP_AGE_RECIPIENT|BACKUP_TURSO_URL|BACKUP_TURSO_TOKEN|BACKUP_AT|BACKUP_KEEP|SLACK_BOT_TOKEN|SLACK_STATUS_CHANNEL|SLACK_APP_TOKEN|SLACK_USERS|GOOGLE_OAUTH_CLIENT_ID|GOOGLE_OAUTH_CLIENT_SECRET|MCP_ALLOWED_EMAILS|JEV_API_KEY|CLAUDE_CODE_OAUTH_TOKEN)=' .env |
	ssh "$host" "umask 077 && cat > $dir/finbot.env.new && chmod 600 $dir/finbot.env.new && mv $dir/finbot.env.new $dir/finbot.env"
ssh "$host" "test -s $dir/finbot.env && test \"\$(stat -c %a $dir/finbot.env)\" = 600" ||
	{ echo "deploy: ~/$dir/finbot.env is empty or not 0600" >&2; exit 1; }

# cd into the compose file's directory so compose's project dir (and its
# default .env lookup) is src/deploy, never ~/finbot.
ssh "$host" "cd $dir/src/deploy && GIT_SHA=$sha FINBOT_ENV_FILE=\$HOME/$dir/finbot.env $compose_tunnel docker compose -p finbot $compose_profile up -d --build --remove-orphans"

# Wait for the healthcheck (finbot health: /healthz 200 after a successful sync).
ssh "$host" bash -s -- "$timeout" <<'REMOTE'
set -euo pipefail
deadline=$((SECONDS + $1))
cid=$(docker ps -q --filter label=com.docker.compose.project=finbot --filter label=com.docker.compose.service=finbot)
[[ -n "$cid" ]] || { echo "deploy: finbot container not running" >&2; exit 1; }
status=
while ((SECONDS < deadline)); do
	status=$(docker inspect -f '{{.State.Health.Status}}' "$cid")
	[[ "$status" == healthy ]] && { echo "deploy: finbot is healthy"; exit 0; }
	sleep 5
done
echo "deploy: FAILED: finbot not healthy after $1s (status: ${status:-unknown}); last logs:" >&2
docker logs --tail 30 "$cid" >&2
exit 1
REMOTE
