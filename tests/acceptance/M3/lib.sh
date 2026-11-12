# Shared helpers for the M3 acceptance scripts. Source from the repo root, don't run.
# secret_count FILE (from M2): lines holding a .env secret/id value, a token-shaped string,
# a UUID or an email. slack_id_count FILE: lines holding a Slack user id from SLACK_USERS.
# Both print counts only, never the matching text.
. tests/acceptance/M2/lib.sh
slack_id_count() {
  local ids
  ids=$(grep -E '^SLACK_USERS=' "$ENV_FILE" | sed -E 's/^[^=]+=//; s/[[:space:]]+#.*$//; s/^"(.*)"$/\1/' \
    | tr ',' '\n' | sed -E 's/^[^:]*://; s/[[:space:]]//g' | awk 'length($0)>=6')
  if [ -z "$ids" ]; then echo "slack_id_lines=0"; return; fi
  echo "slack_id_lines=$(printf '%s\n' "$ids" | grep -c -F -f - "$1" || true)"
}
# clean FILE: PASS when every count is 0, else FAIL.
clean() {
  local f="$1" what="$2" a b
  a=$(secret_count "$f"); b=$(slack_id_count "$f")
  if echo "$a $b" | grep -qE '(exact_value_lines|token_shaped_lines|uuid_lines|email_lines|slack_id_lines)=[1-9]'; then
    echo "FAIL secrets in $what: $a $b"; return 1
  fi
  echo "PASS no secrets/ids in $what: $a $b"
}
