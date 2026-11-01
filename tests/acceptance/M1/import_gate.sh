#!/usr/bin/env bash
# Item 2: the import job stays off unless enabled. Runs `serve` with a bogus token, a bogus plan id
# and a dead proxy, so no request can reach or change YNAB, with a 1s import interval, once with
# the flag false (expect no import activity) and once true (positive control).
# Usage: import_gate.sh FINBOT_BIN WORKDIR
set -u
BIN=$1; W=$(mktemp -d "$2/gate.XXXX"); trap 'rm -rf "$W"' EXIT
run() {
  ( cd "$W" && env -i PATH="$PATH" HOME="$W" TZ=UTC \
      YNAB_ACCESS_TOKEN=bogus-not-a-token YNAB_BUDGET_ID=00000000-0000-0000-0000-000000000000 \
      FINBOT_DB_PATH="$W/$1.db" HTTP_ADDR=127.0.0.1:0 IMPORT_INTERVAL=1s IMPORT_ENABLED="$1" \
      HTTPS_PROXY=http://127.0.0.1:9 HTTP_PROXY=http://127.0.0.1:9 \
      "$BIN" serve > "$W/$1.log" 2>&1 ) &
  local p=$!; sleep 6; pkill -P "$p" 2>/dev/null; kill "$p" 2>/dev/null; wait "$p" 2>/dev/null
  echo "IMPORT_ENABLED=$1: lines=$(wc -l < "$W/$1.log") import_lines=$(grep -ci import "$W/$1.log")"
  grep -i import "$W/$1.log" | cut -c1-220 | sed -E 's/"time":"[^"]*",//' | sort | uniq -c | head -5
}
run false
run true
