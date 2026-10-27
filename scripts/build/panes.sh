#!/usr/bin/env bash
# Build-orchestration helpers for the herdr supervisor. See docs/build/SUPERVISOR.md.
#
#   panes.sh status         one line per agent in this workspace: name pane kind status ctx% 5h% wk%
#   panes.sh ctx <target>   context % of one Claude agent (blank if its statusline isn't readable)
#   panes.sh ack            record the current snapshot as "handled" (sup runs this at the end of each loop)
#   panes.sh watch [secs]   exit as soon as the snapshot differs from the last ack — immediately if it already
#                           does — and print what changed. Run with run_in_background; its exit wakes sup.
#
# A snapshot is, per agent (excluding the caller): pane=status:ctxband, where ctxband is ok | warn (>= CTX_WARN)
# | unknown (Claude pane whose statusline can't be read, e.g. pane too narrow), plus a fingerprint of the
# signal files in $SIGNALS (builders/agy write DONE/HANDOFF/QUESTION/VERDICT lines there).
set -euo pipefail

WS="${HERDR_WORKSPACE_ID:?run inside herdr}"
CTX_WARN="${CTX_WARN:-35}"
WT_ROOT="${WT_ROOT:-$HOME/dev/github.com/deasa/YNAB_AutoCategorizer.wt}"
SIGNALS="$WT_ROOT/_signals"
ACK="$WT_ROOT/_sup/last_ack"
mkdir -p "$SIGNALS" "$(dirname "$ACK")"

# Claude panes render "<model> │ ctx N% │ 5h N% │ wk N%" via ~/.claude/statusline.sh.
statusline() { herdr agent read "$1" --source visible --lines 6 2>/dev/null | grep -E 'ctx [0-9]+%' | tail -1 || true; }
pct() { sed -nE "s/.*$1 ([0-9]+)%.*/\1/p"; }

agents() {
  herdr agent list | jq -r --arg ws "$WS" '.result.agents[] | select(.workspace_id==$ws)
      | [(.name // "-"), .pane_id, .agent, .agent_status] | @tsv'
}

snapshot() {
  local self="${HERDR_PANE_ID:-}" band c
  agents | while IFS=$'\t' read -r _ pane kind st; do
    [ "$pane" = "$self" ] && continue
    band=ok
    if [ "$kind" = claude ]; then
      c=$(statusline "$pane" | pct ctx)
      if [ -z "$c" ]; then band=unknown; elif [ "$c" -ge "$CTX_WARN" ]; then band=warn; fi
    fi
    echo "$pane=$st:$band"
  done | sort
  echo "signals=$(ls -l "$SIGNALS" 2>/dev/null | md5 -q 2>/dev/null || ls -l "$SIGNALS" | md5sum | cut -d' ' -f1)"
}

status() {
  agents | while IFS=$'\t' read -r name pane kind st; do
    line=$(statusline "$pane")
    printf '%-14s %-8s %-6s %-8s ctx=%-4s 5h=%-4s wk=%s\n' "$name" "$pane" "$kind" "$st" \
      "$(echo "$line" | pct ctx)" "$(echo "$line" | pct 5h)" "$(echo "$line" | pct wk)"
  done
  echo "signals:"; for f in "$SIGNALS"/*; do [ -f "$f" ] && echo "  $(basename "$f"): $(tail -1 "$f")"; done; true
}

watch() {
  local every="${1:-60}" cur
  touch "$ACK"
  while :; do
    cur=$(snapshot)
    if [ "$cur" != "$(cat "$ACK")" ]; then
      echo "CHANGED since last ack:"; diff <(cat "$ACK") <(echo "$cur") | grep -E '^[<>]' || true
      exit 0
    fi
    sleep "$every"
  done
}

case "${1:-status}" in
  status) status ;;
  ctx) statusline "${2:?target}" | pct ctx ;;
  ack) snapshot > "$ACK"; echo "acked $(wc -l < "$ACK" | tr -d ' ') lines" ;;
  watch) watch "${2:-60}" ;;
  *) echo "usage: $0 status|ctx <target>|ack|watch [secs]" >&2; exit 2 ;;
esac
