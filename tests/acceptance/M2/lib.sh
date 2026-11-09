# Shared helpers for the M2 acceptance scripts. Source, don't run.
# secret_count FILE: prints how many lines of FILE contain a secret value from .env, a
# token-shaped string, a UUID, or an email address. Prints counts only, never matching text.
ENV_FILE="${ENV_FILE:-.env}"
secret_count() {
  local f="$1" pats
  pats=$(mktemp)
  # Values of every .env var long enough to be a real secret or id (includes the plan id),
  # with inline comments stripped, plus every comma-separated entry in MCP_ALLOWED_EMAILS.
  {
    grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$ENV_FILE" \
      | grep -Ev '^(HTTP_ADDR|LOG_LEVEL|SYNC_INTERVAL|IMPORT_INTERVAL|IMPORT_ENABLED|FINBOT_DB_PATH|TZ|MCP_LISTEN_ADDR|MCP_PUBLIC_URL|SLACK_STATUS_CHANNEL|BACKUP_AT|BACKUP_KEEP|CONFIDENCE_THRESHOLD|DRY_RUN|RUN_INTERVAL)=' \
      | sed -E 's/^[^=]+=//; s/[[:space:]]+#.*$//; s/^[[:space:]]+//; s/[[:space:]]+$//; s/^"(.*)"$/\1/; s/^'"'"'(.*)'"'"'$/\1/' \
      | awk 'length($0)>=12'
    grep -E '^MCP_ALLOWED_EMAILS=' "$ENV_FILE" \
      | sed -E 's/^[^=]+=//; s/[[:space:]]+#.*$//; s/^"(.*)"$/\1/; s/^'"'"'(.*)'"'"'$/\1/' \
      | tr ',' '\n' | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' | awk 'length($0)>0'
  } | sort -u > "$pats"
  local exact shaped uuid email
  exact=$(grep -c -F -f "$pats" "$f" || true)
  shaped=$(grep -c -E 'xox[abprs]-[0-9A-Za-z-]{8,}|xapp-[0-9A-Za-z-]{8,}|AGE-SECRET-KEY-|Bearer [A-Za-z0-9._-]{8,}|eyJ[A-Za-z0-9_-]{20,}\.' "$f" || true)
  uuid=$(grep -c -E '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' "$f" || true)
  email=$(grep -c -E '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' "$f" || true)
  echo "patterns=$(wc -l < "$pats" | tr -d ' ') exact_value_lines=$exact token_shaped_lines=$shaped uuid_lines=$uuid email_lines=$email"
  rm -f "$pats"
}
