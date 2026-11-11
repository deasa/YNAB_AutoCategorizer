#!/usr/bin/env bash
# Checks deploy.sh's .env preflight with dummy .env files. git and ssh are stubs that
# fail with "stub ... reached", so a run that gets past the preflight stops at its first
# git call (deploy.sh exits 1 there) and never reaches a host.
# Usage: deploy/test-preflight.sh
set -uo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/finbot-preflight.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/work/deploy"
for c in git ssh; do printf '#!/bin/sh\necho "stub %s reached" >&2\nexit 3\n' "$c" >"$tmp/bin/$c"; chmod +x "$tmp/bin/$c"; done
cp "$repo/deploy/compose.yaml" "$tmp/work/deploy/"
passed=0 failed=0
required=(YNAB_ACCESS_TOKEN YNAB_BUDGET_ID GOOGLE_OAUTH_CLIENT_ID GOOGLE_OAUTH_CLIENT_SECRET MCP_ALLOWED_EMAILS
	SLACK_BOT_TOKEN SLACK_APP_TOKEN SLACK_USERS JEV_API_KEY CLAUDE_CODE_OAUTH_TOKEN)

# run <name> <want rc> <want stderr substring> <.env lines...>
run() {
	local name=$1 want_rc=$2 want_msg=$3; shift 3
	printf '%s\n' "$@" >"$tmp/work/.env"
	local out rc
	out=$(cd "$tmp/work" && PATH="$tmp/bin:$PATH" HOST=preflight.invalid bash "$repo/deploy/deploy.sh" 2>&1); rc=$?
	if [[ $rc == "$want_rc" && "$out" == *"$want_msg"* ]]; then
		passed=$((passed + 1)); echo "  ok: $name"
	else
		failed=$((failed + 1)); echo "  FAIL: $name (rc=$rc, want $want_rc): $(head -c 200 <<<"$out")"
	fi
}
all=(); for k in "${required[@]}"; do all+=("$k=dummy"); done

run "all keys present passes the preflight" 1 "stub git reached" "${all[@]}"
for k in "${required[@]}"; do
	lines=(); for l in "${all[@]}"; do [[ $l == "$k="* ]] || lines+=("$l"); done
	run "missing $k stops it" 1 "no value for $k" "${lines[@]}"
	run "empty $k stops it" 1 "no value for $k" "${lines[@]}" "$k="
done
run "ANTHROPIC_API_KEY stops it" 1 "sets ANTHROPIC_API_KEY" "${all[@]}" "ANTHROPIC_API_KEY=dummy"
run "exported ANTHROPIC_API_KEY stops it" 1 "sets ANTHROPIC_API_KEY" "${all[@]}" "export ANTHROPIC_API_KEY=dummy"
run "quoted ANTHROPIC_API_KEY stops it" 1 "sets ANTHROPIC_API_KEY" "${all[@]}" 'ANTHROPIC_API_KEY="dummy"'
run "empty ANTHROPIC_API_KEY is allowed" 1 "stub git reached" "${all[@]}" "ANTHROPIC_API_KEY="
rm -f "$tmp/work/.env"
out=$(cd "$tmp/work" && PATH="$tmp/bin:$PATH" HOST=preflight.invalid bash "$repo/deploy/deploy.sh" 2>&1); rc=$?
if [[ $rc == 1 && "$out" == *"with .env"* ]]; then passed=$((passed + 1)); echo "  ok: absent .env stops it"; else failed=$((failed + 1)); echo "  FAIL: absent .env (rc=$rc)"; fi

echo "passed=$passed failed=$failed"
[[ $failed == 0 ]]
