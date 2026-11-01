# Shared helpers for the M1 acceptance scripts. Source, don't run.
# secret_count FILE: prints how many lines of FILE contain a secret value from .env, or a
# token-shaped string. Prints a count only, never the matching text.
ENV_FILE="${ENV_FILE:-.env}"
secret_count() {
  local f="$1" pats
  pats=$(mktemp)
  # Values of every .env var long enough to be a real secret or id. Includes the plan id.
  grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$ENV_FILE" \
    | grep -Ev '^(HTTP_ADDR|LOG_LEVEL|SYNC_INTERVAL|IMPORT_INTERVAL|IMPORT_ENABLED|FINBOT_DB_PATH|TZ)=' \
    | sed -E 's/^[^=]+=//; s/^"(.*)"$/\1/; s/^'"'"'(.*)'"'"'$/\1/' \
    | awk 'length($0)>=16' > "$pats"
  local exact shaped uuid
  exact=$(grep -c -F -f "$pats" "$f" || true)
  shaped=$(grep -c -E 'xox[abprs]-[0-9A-Za-z-]{8,}|xapp-[0-9A-Za-z-]{8,}|AGE-SECRET-KEY-|Bearer [A-Za-z0-9._-]{8,}|eyJ[A-Za-z0-9_-]{20,}\.' "$f" || true)
  uuid=$(grep -c -E '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' "$f" || true)
  echo "patterns=$(wc -l < "$pats" | tr -d ' ') exact_value_lines=$exact token_shaped_lines=$shaped uuid_lines=$uuid"
  rm -f "$pats"
}
