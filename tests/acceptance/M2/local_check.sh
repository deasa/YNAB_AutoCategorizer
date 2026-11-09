#!/usr/bin/env bash
# Black-box check of the local finbot CLI and MCP server against a temp SQLite DB
# populated by one read-only `finbot sync` (scope 3).
# SLACK_BOT_TOKEN, HEALTHCHECK_PING_URL and all BACKUP_* are blanked; IMPORT_ENABLED=false.
# Never writes to YNAB and never prints secrets, UUIDs, tokens or emails.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../../.." && pwd)
# shellcheck source=lib.sh
source "$HERE/lib.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "== building finbot and mcp_driver"
(cd "$ROOT" && go build -o "$tmp/finbot" . && go build -o "$tmp/mcp_driver" ./tests/acceptance/M2)

YNAB_TOKEN=$(grep '^YNAB_ACCESS_TOKEN=' "$ENV_FILE" | sed -E 's/^[^=]+=//; s/[[:space:]]+#.*$//; s/^"(.*)"$/\1/')
BUDGET_ID=$(grep '^YNAB_BUDGET_ID=' "$ENV_FILE" | sed -E 's/^[^=]+=//; s/[[:space:]]+#.*$//; s/^"(.*)"$/\1/')

mkdir -p "$tmp/empty"

echo "== running read-only finbot sync into temp DB (initial + delta)"
(
  cd "$tmp/empty"
  env -i PATH="$PATH" HOME="$tmp/empty" \
    FINBOT_DB_PATH="$tmp/finbot.db" \
    YNAB_ACCESS_TOKEN="$YNAB_TOKEN" \
    YNAB_BUDGET_ID="$BUDGET_ID" \
    IMPORT_ENABLED=false \
    SLACK_BOT_TOKEN="" \
    HEALTHCHECK_PING_URL="" \
    BACKUP_AGE_RECIPIENT="" \
    BACKUP_TURSO_URL="" \
    BACKUP_TURSO_TOKEN="" \
    "$tmp/finbot" sync >"$tmp/sync1.out" 2>"$tmp/sync1.err"

  env -i PATH="$PATH" HOME="$tmp/empty" \
    FINBOT_DB_PATH="$tmp/finbot.db" \
    YNAB_ACCESS_TOKEN="$YNAB_TOKEN" \
    YNAB_BUDGET_ID="$BUDGET_ID" \
    IMPORT_ENABLED=false \
    SLACK_BOT_TOKEN="" \
    HEALTHCHECK_PING_URL="" \
    BACKUP_AGE_RECIPIENT="" \
    BACKUP_TURSO_URL="" \
    BACKUP_TURSO_TOKEN="" \
    "$tmp/finbot" sync >"$tmp/sync2.out" 2>"$tmp/sync2.err"
)

cat "$tmp/sync1.out" "$tmp/sync1.err" "$tmp/sync2.out" "$tmp/sync2.err" >"$tmp/sync.all"
sc_sync=$(secret_count "$tmp/sync.all")
echo "sync secret_count: $sc_sync"
[[ "$sc_sync" == *"exact_value_lines=0 token_shaped_lines=0 uuid_lines=0 email_lines=0" ]] || {
  echo "FAIL: sync output matched secret/uuid/email pattern" >&2
  exit 1
}
cat "$tmp/sync1.out"
cat "$tmp/sync2.out"

echo "== running mcp_driver black-box CLI + OAuth + MCP suite"
rc=0
(
  cd "$tmp/empty"
  env -i PATH="$PATH" HOME="$tmp/empty" \
    YNAB_ACCESS_TOKEN="$YNAB_TOKEN" \
    YNAB_BUDGET_ID="$BUDGET_ID" \
    "$tmp/mcp_driver" -bin "$tmp/finbot" -db "$tmp/finbot.db" -workdir "$tmp/empty" >"$tmp/driver.out" 2>&1
) || rc=$?
sc_driver=$(secret_count "$tmp/driver.out")
echo "driver secret_count: $sc_driver"
[[ "$sc_driver" == *"exact_value_lines=0 token_shaped_lines=0 uuid_lines=0 email_lines=0" ]] || {
  echo "FAIL: driver output matched secret/uuid/email pattern" >&2
  exit 1
}
cat "$tmp/driver.out"
exit "$rc"
