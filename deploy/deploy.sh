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
for key in YNAB_ACCESS_TOKEN YNAB_BUDGET_ID; do
	grep -qE "^${key}=." .env || { echo "deploy: .env has no ${key}" >&2; exit 1; }
done

ssh "$host" "mkdir -p $dir/src && chmod 700 $dir"
rsync -a --delete \
	--exclude .git --exclude .env --exclude '*.env' --exclude /finbot --exclude '*.test' --exclude '*.db*' \
	--exclude spikes/ --exclude docs/ \
	./ "$host:$dir/src/"

# finbot.env: only the keys finbot needs, from the local .env.
grep -E '^(YNAB_ACCESS_TOKEN|YNAB_BUDGET_ID|HEALTHCHECK_PING_URL)=' .env |
	ssh "$host" "umask 077 && cat > $dir/finbot.env.new && chmod 600 $dir/finbot.env.new && mv $dir/finbot.env.new $dir/finbot.env"
ssh "$host" "test -s $dir/finbot.env && test \"\$(stat -c %a $dir/finbot.env)\" = 600" ||
	{ echo "deploy: ~/$dir/finbot.env is empty or not 0600" >&2; exit 1; }

# cd into the compose file's directory so compose's project dir (and its
# default .env lookup) is src/deploy, never ~/finbot.
ssh "$host" "cd $dir/src/deploy && FINBOT_ENV_FILE=\$HOME/$dir/finbot.env docker compose -p finbot up -d --build --remove-orphans"

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
