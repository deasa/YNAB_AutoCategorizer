#!/usr/bin/env bash
# Scope 1 (hoid, dry_run), read-only: ssh/docker reads, a copy of the live DB read with
# sqlite3 -readonly, and Slack conversations.history. Never clicks, posts, or writes to YNAB.
#   hoid_check.sh snapshot [dbdir]  record before-state of the open dry_run_check questions (run before the
#                            click); dbdir: use an earlier local copy of /data/finbot.db* instead of copying now
#   hoid_check.sh check      dry-run invariants; and, once a snapshotted question is answered (BA-23,
#                            Brendan's click), verify the click recorded without changing YNAB.
# Exit: 0 pass, 1 fail, 3 waiting for the click. The state file holds question numbers and
# sha256 hashes only, never raw ids. Run from the repo root.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
. tests/acceptance/M3/lib.sh
mode="${1:-check}"
STATE="${M3_STATE:-$(cd .. && pwd)/_m3-acceptance-state}"; mkdir -p "$STATE"; chmod 700 "$STATE"
T=$(mktemp -d); chmod 700 "$T"; trap 'rm -rf "$T"' EXIT
rc=0
pass() { echo "PASS $*"; }
fail() { echo "FAIL $*"; rc=1; }
q() { sqlite3 -readonly "$T/db/finbot.db" "$1"; }
h() { printf '%s' "$1" | shasum -a 256 | cut -c1-16; }

mkdir -p "$T/db"
if [ "$mode" = snapshot ] && [ -n "${2:-}" ]; then cp "$2"/finbot.db* "$T/db/" || exit 1; echo "INFO snapshot from an earlier DB copy"; else
# One copy of finbot.db + finbot.db-wal (the -shm is a cache SQLite rebuilds). The container has no
# sqlite3, so the copy isn't atomic: it is accepted only if PRAGMA quick_check is ok, else retried.
# The remote temp dir (it holds the OAuth HMAC secret) is removed by a trap on every exit path.
ok=0
for try in 1 2 3; do
  rm -f "$T"/db/finbot.db*
  ssh hoid 'umask 077; d=$(mktemp -d) || exit 1; trap "rm -rf \"$d\"" EXIT
    docker cp -q finbot-finbot-1:/data/finbot.db-wal "$d/finbot.db-wal" &&
    docker cp -q finbot-finbot-1:/data/finbot.db "$d/finbot.db" && tar -C "$d" -cf - .' \
    | tar -C "$T/db" -xf - 2>/dev/null
  [ "$(sqlite3 -readonly "$T/db/finbot.db" 'PRAGMA quick_check' 2>/dev/null)" = ok ] && { ok=1; break; }
  echo "INFO DB copy $try failed quick_check; retrying"; sleep 2
done
[ "$ok" = 1 ] || { echo "FAIL no consistent DB copy from hoid after 3 tries"; exit 1; }
fi

# --- invariants (always) ---
st=$(ssh hoid 'docker inspect -f "{{.State.Health.Status}}" finbot-finbot-1; docker inspect finbot-finbot-1 -f "{{range .Config.Env}}{{println .}}{{end}}" | grep -E "^CLASSIFY_(MODE|MAX_OPEN_QUESTIONS)="')
echo "$st" | head -1 | grep -qx healthy && pass "finbot-finbot-1 healthy" || fail "container not healthy"
echo "$st" | grep -qx 'CLASSIFY_MODE=dry_run' && pass "CLASSIFY_MODE=dry_run on hoid" || fail "CLASSIFY_MODE is not dry_run"
cap=$(echo "$st" | sed -n 's/^CLASSIFY_MAX_OPEN_QUESTIONS=//p'); cap=${cap:-5}
wl=$(q "SELECT count(*) FROM write_log"); [ "$wl" = 0 ] && pass "write_log is empty (0 rows)" || fail "write_log has $wl rows"
read -r dec auto applied <<<"$(q "SELECT count(*), sum(mode='auto'), sum(outcome='applied') FROM decisions" | tr '|' ' ')"
[ "${dec:-0}" -gt 0 ] && [ "${auto:-0}" = 0 ] && [ "${applied:-0}" = 0 ] \
  && pass "decisions: $dec, all dry_run, none applied" || fail "decisions: $dec total, $auto auto, $applied applied"
read -r open pend unposted cat <<<"$(q "SELECT sum(status='open'), sum(status='pending'), sum(status='open' AND message_ts IS NULL), sum(kind='categorize') FROM questions" | tr '|' ' ')"
ans=$(q "SELECT count(*) FROM questions WHERE status IN ('open','answered')")
[ "$ans" -gt 0 ] && pass "questions appear: $ans posted ($open open now), $pend pending" || fail "no posted questions"
[ "${open:-0}" -le "$cap" ] && [ "${unposted:-0}" = 0 ] && pass "cap holds: $open open <= $cap with $pend waiting" || fail "cap: $open open, cap $cap, $unposted unposted"
[ "${cat:-0}" = 0 ] && pass "no categorize questions in dry_run" || fail "$cat categorize questions"
sl=$(q "SELECT count(*) FROM human_locks WHERE source='slack'"); [ "$sl" = 0 ] && pass "no Slack-sourced locks" || fail "$sl Slack locks"
last=$(ssh hoid 'docker exec finbot-finbot-1 finbot dry-run-report' 2>&1 | tail -1)
[ "$last" = "writes: 0" ] && pass "dry-run-report: writes: 0" || fail "dry-run-report last line: $last"

# --- scope 1 click ---
if [ "$mode" = snapshot ]; then
  : > "$STATE/before.tsv"; chmod 600 "$STATE/before.tsv"
  q "SELECT q.id, coalesce(t.category_id,''), t.approved FROM questions q JOIN transactions t ON t.plan_id=q.plan_id AND t.id=q.subject_id
     WHERE q.status='open' AND q.kind='dry_run_check'" | while IFS='|' read -r id c a; do
    printf '%s\t%s\t%s\n' "$id" "$(h "$c")" "$a" >> "$STATE/before.tsv"
  done
  printf 'write_log\t%s\n' "$wl" >> "$STATE/before.tsv"
  pass "snapshot: $(($(wc -l < "$STATE/before.tsv")-1)) open dry_run_check questions recorded in $STATE/before.tsv"
else
  [ -f "$STATE/before.tsv" ] || { echo "WAIT no snapshot; run '$0 snapshot' first"; exit 3; }
  answered=0; : > "$T/asked"
  while IFS=$'\t' read -r id ch ap; do
    [ "$id" = write_log ] && { [ "$wl" = "$ch" ] && pass "write_log unchanged since snapshot ($wl)" || fail "write_log $ch -> $wl"; continue; }
    row=$(q "SELECT q.status, coalesce(q.answered_by,''), coalesce(q.answered_at,''), coalesce(t.category_id,''), t.approved, q.channel_id, q.message_ts
             FROM questions q JOIN transactions t ON t.plan_id=q.plan_id AND t.id=q.subject_id WHERE q.id=$id")
    IFS='|' read -r qs by at c a chn ts <<<"$row"
    [ "$qs" = answered ] || continue
    answered=$((answered+1))
    synced=$(q "SELECT s.synced_at FROM sync_state s JOIN questions q ON q.plan_id=s.plan_id WHERE q.id=$id")
    [ -n "$by" ] && pass "question $id answered by $by" || fail "question $id answered with no person"
    if [[ "$synced" > "$at" ]]; then
      [ "$(h "$c")" = "$ch" ] && pass "question $id: row's category unchanged in the mirror (synced after the answer)" || fail "question $id: category changed"
      [ "$a" = "$ap" ] && [ "$a" = 0 ] && pass "question $id: row still unapproved" || fail "question $id: approved $ap -> $a"
    else
      echo "WAIT question $id: no sync since the answer yet; rerun after the next sync (<=15 min)"; [ "$rc" = 0 ] && rc=3
    fi
    echo "$id $chn $ts" >> "$T/asked"
  done < "$STATE/before.tsv"
  # Every answered dry_run_check, snapshotted or not (later ones were posted after the snapshot).
  read -r all unapp nobody differs <<<"$(q "SELECT count(*), sum(t.approved=0), sum(q.answered_by IS NULL),
      sum(t.category_id IS NULL OR t.category_id <> q.answer)
    FROM questions q JOIN transactions t ON t.plan_id=q.plan_id AND t.id=q.subject_id
    WHERE q.kind='dry_run_check' AND q.status='answered'" | tr '|' ' ')"
  [ "${all:-0}" -gt 0 ] && [ "${nobody:-0}" = 0 ] && [ "$unapp" = "$all" ] \
    && pass "all $all answered dry_run_checks: answered by a person, rows still unapproved; $differs answers differ from the row's YNAB category (so none was written)" \
    || fail "answered dry_run_checks: $all total, $unapp unapproved, $nobody with no person"
  : > "$T/asked"
  q "SELECT id, channel_id, message_ts FROM questions WHERE kind='dry_run_check' AND status='answered'" | tr '|' ' ' > "$T/asked"
  if [ "$answered" = 0 ]; then
    echo "WAIT none of the snapshotted questions is answered yet (BA-23: Brendan's click)"; [ "$rc" = 0 ] && rc=3
  else
    python3 tests/acceptance/M3/slack_read.py "$T/qmsgs.jsonl" "$T/slack.jsonl" < "$T/asked" || fail "Slack read"
    while read -r id _ _; do
      jq -se --argjson id "$id" 'map(select(.qid==$id))[0].text | test("YNAB wasn.t changed")' "$T/qmsgs.jsonl" >/dev/null \
        && pass "question $id: Slack message says YNAB wasn't changed" || fail "question $id: Slack text lacks \"YNAB wasn't changed\""
    done < "$T/asked"
    # "by <@U…>" (who answered) is a Slack mention, rendered as a name in Slack: expected in
    # Slack text, so it's counted, then stripped before the id grep.
    for f in qmsgs slack; do
      echo "INFO $f: $(grep -oE '<@[UW][A-Z0-9]+>' "$T/$f.jsonl" | wc -l | tr -d ' ') answerer mentions (expected)"
      sed -E 's/<@[UW][A-Z0-9]+>/<@mention>/g' "$T/$f.jsonl" > "$T/$f.stripped"
    done
    clean "$T/qmsgs.stripped" "the answered question messages" || rc=1
    clean "$T/slack.stripped" "the last 200 channel messages" || rc=1
  fi
fi

# --- secret and id grep over what this check used ---
ssh hoid 'docker logs finbot-finbot-1 2>&1' > "$T/logs.txt"
echo "INFO container log: $(grep -c '"level":"WARN"' "$T/logs.txt") WARN, $(grep -c '"level":"ERROR"' "$T/logs.txt") ERROR lines"
clean "$T/logs.txt" "finbot container logs" || rc=1
q "SELECT 'write_log', count(*) FROM write_log; SELECT 'questions', kind, status, count(*) FROM questions GROUP BY 2,3;
   SELECT 'decisions', mode, outcome, count(*) FROM decisions GROUP BY 2,3; SELECT 'locks', source, count(*) FROM human_locks GROUP BY 2;" > "$T/export.txt"
cat "$T/export.txt"; clean "$T/export.txt" "the DB export" || rc=1
case $rc in 0) echo "HOID RESULT PASS";; 3) echo "HOID RESULT WAITING";; *) echo "HOID RESULT FAIL";; esac
exit "$rc"
