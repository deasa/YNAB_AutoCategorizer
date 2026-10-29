#!/usr/bin/env bash
# Checks that every architecture.md §5a flag exists in `claude --help` (no auth needed).
# Usage: check_flags.sh            (local)
#        ssh hoid 'bash -s' < check_flags.sh
set -euo pipefail

CLAUDE="${CLAUDE:-claude}"
command -v "$CLAUDE" >/dev/null || CLAUDE="$HOME/.local/bin/claude"

echo "version: $("$CLAUDE" --version)"
help="$("$CLAUDE" --help 2>&1)"

# Prints the full (joined) help entry for an option/command: its 2-space-indented
# line plus the deeper-indented continuation lines that follow it.
entry() {
  printf '%s\n' "$help" | awk -v pat="$1" '
    /^  [^ ]/ { if (found) exit; if ($0 ~ pat) { found = 1; out = $0; next } }
    found && /^   +/ { out = out " " $0; next }
    found { exit }
    END { if (found) print out }
  ' | sed -E 's/[[:space:]]+/ /g; s/^ //; s/\|/\\|/g'
}

rows=(
  "-p|^  -p, --print "
  "--output-format|^  --output-format "
  "--tools|^  --tools "
  "--strict-mcp-config|^  --strict-mcp-config"
  "--mcp-config|^  --mcp-config "
  "--allowedTools|^  --allowedTools"
  "--json-schema|^  --json-schema "
  "--permission-prompts|^  --permission-prompts "
  "--setting-sources|^  --setting-sources "
  "--disable-slash-commands|^  --disable-slash-commands"
  "--append-system-prompt-file|^  --append-system-prompt-file"
  "--resume|^  -r, --resume "
  "--system-prompt-snapshot|^  --system-prompt-snapshot "
  "--bare|^  --bare"
  "--model|^  --model "
  "--fallback-model|^  --fallback-model "
  "setup-token|^  setup-token"
)

echo
echo "| Flag | Present | Help text |"
echo "|---|---|---|"
for r in "${rows[@]}"; do
  name="${r%%|*}"
  text="$(entry "${r#*|}")"
  if [[ -n "$text" ]]; then
    echo "| \`$name\` | yes | $text |"
  else
    echo "| \`$name\` | **NO** (not a listed option) | |"
  fi
done

# Hidden flags still show up in other options' help text; list where they are mentioned.
echo
echo "Mentions of append-system-prompt-file:"
printf '%s\n' "$help" | grep -n -- "append-system-prompt\[-file\]\|append-system-prompt-file" || echo "(none)"
