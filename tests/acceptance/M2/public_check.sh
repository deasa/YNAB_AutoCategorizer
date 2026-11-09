#!/usr/bin/env bash
# Black-box check of public https://finance.hoidlabs.site (scope 1).
# Read-only / unauthenticated probes + one ephemeral public DCR client registration.
# Never prints secrets, tokens, client_ids, UUIDs or emails.
set -euo pipefail

BASE="${PUBLIC_URL:-https://finance.hoidlabs.site}"
pass=0
fail=0

ok()  { pass=$((pass + 1)); echo "PASS: $1"; }
bad() { fail=$((fail + 1)); echo "FAIL: $1" >&2; }
assert_eq() {
  if [[ "$1" == "$2" ]]; then ok "$3"; else bad "$3 (got '$1', want '$2')"; fi
}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# 1. Protected resource metadata (root and /mcp path) + Cloudflare header check
code=$(curl -sS -D "$tmp/prm.hdr" -o "$tmp/prm.json" -w '%{http_code}' "$BASE/.well-known/oauth-protected-resource")
assert_eq "$code" "200" "GET /.well-known/oauth-protected-resource returns 200"
if grep -qi '^cf-ray:' "$tmp/prm.hdr" || grep -qi '^server: cloudflare' "$tmp/prm.hdr"; then
  ok "Cloudflare headers present on public response"
else
  bad "Cloudflare headers missing on public response"
fi
python3 - "$tmp/prm.json" "$BASE" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
base = sys.argv[2]
assert d.get("resource") == base + "/mcp", f"resource={d.get('resource')}"
assert d.get("resource_name") == "finbot", f"resource_name={d.get('resource_name')}"
assert d.get("authorization_servers") == [base], f"as={d.get('authorization_servers')}"
assert d.get("scopes_supported") == ["mcp"], f"scopes={d.get('scopes_supported')}"
assert d.get("bearer_methods_supported") == ["header"], f"bearer={d.get('bearer_methods_supported')}"
PY
ok "Protected resource metadata fields (resource_name=finbot, not spike)"

code2=$(curl -sS -o "$tmp/prm2.json" -w '%{http_code}' "$BASE/.well-known/oauth-protected-resource/mcp")
assert_eq "$code2" "200" "GET /.well-known/oauth-protected-resource/mcp returns 200"

# 2. OAuth Authorization Server metadata
code_as=$(curl -sS -o "$tmp/as.json" -w '%{http_code}' "$BASE/.well-known/oauth-authorization-server")
assert_eq "$code_as" "200" "GET /.well-known/oauth-authorization-server returns 200"
python3 - "$tmp/as.json" "$BASE" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
base = sys.argv[2]
assert d.get("issuer") == base
assert d.get("authorization_endpoint") == base + "/oauth/authorize"
assert d.get("token_endpoint") == base + "/oauth/token"
assert d.get("registration_endpoint") == base + "/oauth/register"
assert d.get("revocation_endpoint") == base + "/oauth/revoke"
assert d.get("code_challenge_methods_supported") == ["S256"]
assert "mcp" in d.get("scopes_supported", [])
PY
ok "OAuth AS metadata endpoints and S256-only PKCE"

# 3. /mcp without token -> 401 with WWW-Authenticate resource_metadata
code_mcp=$(curl -sS -D "$tmp/mcp.hdr" -o /dev/null -w '%{http_code}' -X POST "$BASE/mcp")
assert_eq "$code_mcp" "401" "POST /mcp without token returns 401"
if grep -qi "resource_metadata=\"$BASE/.well-known/oauth-protected-resource\"" "$tmp/mcp.hdr"; then
  ok "POST /mcp 401 WWW-Authenticate cites resource_metadata"
else
  bad "POST /mcp 401 WWW-Authenticate missing resource_metadata"
fi

# 4. /mcp with invalid bearer token -> 401
code_bad=$(curl -sS -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer invalid-token-probe" "$BASE/mcp")
assert_eq "$code_bad" "401" "POST /mcp with invalid bearer token returns 401"

# 5. CORS preflight on /mcp
code_cors=$(curl -sS -D "$tmp/cors.hdr" -o /dev/null -w '%{http_code}' -X OPTIONS \
  -H "Origin: https://claude.ai" \
  -H "Access-Control-Request-Method: POST" "$BASE/mcp")
assert_eq "$code_cors" "204" "OPTIONS /mcp preflight returns 204"
if grep -qi '^access-control-allow-origin: \*' "$tmp/cors.hdr"; then
  ok "OPTIONS /mcp includes Access-Control-Allow-Origin: *"
else
  bad "OPTIONS /mcp missing Access-Control-Allow-Origin"
fi

# 6. /healthz is NOT public (404)
code_hz=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/healthz")
assert_eq "$code_hz" "404" "GET /healthz on public hostname returns 404"

# 7. DCR validation and registration
code_dcr_bad1=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/oauth/register" \
  -H "Content-Type: application/json" \
  -d '{"client_name":"bad","redirect_uris":["http://evil.example.com/cb"]}')
assert_eq "$code_dcr_bad1" "400" "POST /oauth/register rejects non-https non-loopback redirect_uri (400)"

code_dcr_bad2=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/oauth/register" \
  -H "Content-Type: application/json" \
  -d "{\"client_name\":\"bad\",\"redirect_uris\":[\"$BASE/oauth/google/callback\"]}")
assert_eq "$code_dcr_bad2" "400" "POST /oauth/register rejects own /oauth/ redirect_uri (400)"

code_dcr_ok=$(curl -sS -o "$tmp/dcr.json" -w '%{http_code}' -X POST "$BASE/oauth/register" \
  -H "Content-Type: application/json" \
  -d '{"client_name":"m2-acceptance-probe","redirect_uris":["http://127.0.0.1:54321/cb"]}')
assert_eq "$code_dcr_ok" "201" "POST /oauth/register accepts valid loopback client (201)"

cid=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert d["client_id"] and d["token_endpoint_auth_method"]=="none"; print(d["client_id"])' "$tmp/dcr.json")
ok "DCR response has non-empty client_id and token_endpoint_auth_method=none"

# 8. /oauth/authorize rejects missing/plain PKCE locally (400) and redirects (302) to Google on S256
code_auth_nopkce=$(curl -sS -o /dev/null -w '%{http_code}' \
  "$BASE/oauth/authorize?response_type=code&client_id=$cid&redirect_uri=http://127.0.0.1:54321/cb&state=s12345678")
assert_eq "$code_auth_nopkce" "400" "GET /oauth/authorize without PKCE returns 400 locally"

code_auth_plain=$(curl -sS -o /dev/null -w '%{http_code}' \
  "$BASE/oauth/authorize?response_type=code&client_id=$cid&redirect_uri=http://127.0.0.1:54321/cb&state=s12345678&code_challenge=abcdefgh1234567890&code_challenge_method=plain")
assert_eq "$code_auth_plain" "400" "GET /oauth/authorize with plain PKCE returns 400 locally"

code_auth_s256=$(curl -sS -D "$tmp/auth.hdr" -o /dev/null -w '%{http_code}' \
  "$BASE/oauth/authorize?response_type=code&client_id=$cid&redirect_uri=http://127.0.0.1:54321/cb&state=s12345678&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM&code_challenge_method=S256")
assert_eq "$code_auth_s256" "302" "GET /oauth/authorize with S256 PKCE returns 302 redirect"
if grep -qi '^location: https://accounts\.google\.com/' "$tmp/auth.hdr"; then
  ok "GET /oauth/authorize redirects to accounts.google.com"
else
  bad "GET /oauth/authorize did not redirect to accounts.google.com"
fi

echo "SUMMARY: passed=$pass failed=$fail"
[[ "$fail" -eq 0 ]]
