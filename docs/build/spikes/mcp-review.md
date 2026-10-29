# agy review M0-2a r1

VERDICT: FIX

## Findings
1. [severity: high] [VERIFIED] `spikes/mcp/authserver.go:63-73,319-334` and `spikes/mcp/mcpserver.go:45-47`: `VerifyToken` accepts refresh tokens (`ory_rt_…`) and tokens lacking the `mcp` scope on `/mcp` (violates N7, §4). In `fosite@v0.49.0` (`handler/oauth2/introspector.go:27-53`), `fosite.AccessToken` is only a lookup-order hint unless `Config.DisableRefreshTokenValidation` is `true`; `VerifyToken` discards the returned `TokenUse` (`_`) and reads `sess.GetExpiresAt(fosite.AccessToken)` from the shared session, so `Authorization: Bearer <refresh_token>` gets `200 OK` on `POST /mcp` without rotation, as does an `offline_access`-only token because `RequireBearerTokenOptions.Scopes` is unset → set `DisableRefreshTokenValidation: true` in `fosite.Config`, reject `tu != fosite.AccessToken` in `VerifyToken`, set `Scopes: []string{scopeMCP}` in `RequireBearerTokenOptions` (and require `scopeMCP` at authorize/introspect), and add tests asserting `401` for a refresh token and `403` without `mcp` scope.
2. [severity: high] [VERIFIED] `spikes/mcp/authserver.go:134-195,199-247` and `docs/build/spikes/mcp.md:206-207`: Open `https://` DCR, permissive URL parsing, and zero local consent enable confused-deputy phishing once M0-2b exposes this spike (`M0-2a.md:9,14`, N6, N7). `validRedirectURI` (`authserver.go:180-195`) checks only `u.IsAbs()`, accepting `https:///cb` (empty host), `https:opaque` (`u.Opaque != ""`), `https://claude.ai@evil.example/cb` (`u.User != nil`), and finbot's own `/oauth/*` paths (`201` on each). Worse, an attacker can DCR-register `https://evil.example/cb` and send an `/oauth/authorize` link: Google shows only *"Sign in to finbot"*, and `handleGoogleCallback` (`authserver.go:241-246`) immediately 303-redirects `?code=…` to `evil.example` with no finbot consent step (`mcp.md:206` defers this past M0-2b) → in `validRedirectURI`, reject `u.Opaque != ""`, `u.Hostname() == ""`, `u.User != nil`, and `s.issuer+"/oauth/"` prefixes (with tests); before M0-2b, add a CSRF-protected `POST` consent page after Google callback showing `client_name` and the `redirect_uri` host before `completeLogin` (or allowlist non-loopback DCR origins).
3. [severity: med] [VERIFIED] `spikes/mcp/authserver.go:48,199-216`, `spikes/mcp/server_test.go:189`, and `docs/build/spikes/mcp.md:75,85`: `handleAuthorize` skips PKCE checks before redirecting to Google, leaves `s.pending` uncapped, and lacks PKCE tests. `fosite.NewAuthorizeRequest` (`authorize_request_handler.go:333-432`) defers `pkce.Handler` until `NewAuthorizeResponse`, so `GET /oauth/authorize` without `code_challenge` `302`s to `accounts.google.com` (contradicting `mcp.md:75,85`) and parks an uncapped `pendingLogin` for 10 min (memory DoS) → require `code_challenge != ""` and `code_challenge_method == "S256"` in `handleAuthorize` before parking, cap `len(s.pending)` (e.g. `maxPending = 100`), and test missing/plain `code_challenge` and wrong `code_verifier`.
4. [severity: low] [VERIFIED] `spikes/mcp/authserver.go:307`, `spikes/mcp/main.go:80`, `spikes/mcp/mcpserver.go:44`, and `spikes/mcp/.gitignore:1-2`: `issued, _ := ds.Extra[genKey].(int)` ignores `!ok` (defaulting to `0` and breaking on JSON `float64` when persisted); `http.ListenAndServe` omits `ReadHeaderTimeout`/`IdleTimeout`; `mcpHandler` allocates `newMCPServer()` per request with `SessionTimeout: 0`; and `go build` emits binary `mcp`, omitted from `.gitignore` → fail closed on missing/non-numeric `gen` or empty `Subject`, use `&http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}`, reuse one `*mcp.Server` with `SessionTimeout`, and add `/mcp` to `spikes/mcp/.gitignore`.

## Checks run
- `git -C .../M0-2a diff main...HEAD --stat && git status --short` → VERIFIED: clean worktree; only `spikes/mcp/*`, `docs/build/spikes/mcp.md`, and `docs/build/tasks/M0-2a.md` touched; root module, `CLAUDE.md`, and `docs/plan/*` untouched.
- `git -C .../M0-2a log -p main..HEAD -- ':!spikes/mcp/go.sum' | grep -nE '(@|GOCSPX|googleusercontent|ory_at_|ory_rt_|ya29\.|1//|eyJ…)'` → VERIFIED: 0 secrets, client IDs, tokens, or allowlist emails in commits/diff (only git headers and `@example.com` fixtures).
- Root `go build ./... && go vet ./... && go test -count=1 ./...` and `spikes/mcp` `go vet ./... && go test -v -race ./...` → VERIFIED: all green (11/11 spike tests pass under `-race`).
- Live local server check (`GOOGLE_OAUTH_CLIENT_ID=dummy … go run .` on `localhost:8080`, `.env` untouched/unprinted): `curl -si localhost:8080/mcp`, well-known `GET`s, `go run . revoke`, DCR `POST /oauth/register` (`https:///cb`, `https:opaque`, `https://claude.ai@evil.example/cb`), and `GET /oauth/authorize` without `code_challenge` → VERIFIED: `/mcp` returns `401` + `WWW-Authenticate`, metadata valid, `revoke` bumps hashed ID in `0600` `generations.json`, malformed DCR URIs return `201`, and missing-PKCE authorize `302`s to Google (Brendan's live Google sign-in in `mcp.md:149-164` UNVERIFIED directly).
- Scratch PoC test against unmodified `spikes/mcp` (`POST /mcp` with `Bearer <ory_rt_…>` and `scope=offline_access` token) → VERIFIED: both return `HTTP 200` instead of `401`/`403`.

## Builder response
1. **Fixed.**
   - `fosite.Config.DisableRefreshTokenValidation: true`, and `VerifyToken` rejects `tu != fosite.AccessToken` and
     non-`DefaultSession` sessions.
   - `RequireBearerTokenOptions.Scopes: []string{"mcp"}`, so the 401 now advertises `scope="mcp"` and a token without
     `mcp` gets 403. Authorize always adds `mcp` to the requested scopes, so normal clients never get a token without it.
   - Tests: `TestRefreshTokenIsNotABearerToken` (401), `TestTokenWithoutMCPScopeForbidden` (403), `TestMCPScopeAlwaysGranted`.
2. **Fixed.**
   - `validRedirectURI` (now a method) rejects opaque URIs, an empty host, userinfo, fragments, and the issuer's own
     `/oauth/*` (case-insensitive).
   - Added a CSRF-protected `POST /oauth/consent` page after the Google callback and allowlist check, before any code is
     issued. It shows the user, the `client_name` (HTML-escaped) and the `redirect_uri` host, with a loopback warning.
   - The CSRF token must match both the form field and a `SameSite=Strict` HttpOnly cookie (`Secure` on https). Consent is
     single-use with a 10-minute TTL, and the page sets `X-Frame-Options: DENY` and a CSP with `frame-ancestors 'none'`.
   - No origin allowlist, as sup directed.
   - Tests: `TestRegisterRedirectURIValidation` (11 cases), `TestConsentPage`, `TestConsentCSRFAndDecision` (no cookie,
     wrong csrf, unknown id, deny, replay).
3. **Fixed.**
   - `handleAuthorize` requires `code_challenge` and `code_challenge_method=S256` before parking or redirecting. Otherwise
     it sends `invalid_request` to the client.
   - Pending logins plus consents are capped at `maxPending=100`, with expired entries swept first, and return 503 when full.
   - Tests: `TestAuthorizeRequiresS256PKCE` (missing, plain, or no method; nothing parked), `TestTokenRejectsWrongVerifier`
     (`invalid_grant`), `TestPendingLoginsCapped`.
4. **Fixed.**
   - `checkGeneration` fails closed on an empty subject, or a missing, non-numeric or fractional `gen`, and accepts
     `float64` after a JSON round trip (`TestCheckGenerationFailsClosed`).
   - `http.Server` now sets `ReadHeaderTimeout: 10s` and `IdleTimeout: 2m`.
   - One shared `*mcp.Server` with `SessionTimeout: 30m`.
   - `/mcp` added to `spikes/mcp/.gitignore`.
   - Note: your `go run .` review server (dummy client id) was still bound to `localhost:8080`, orphaned under PID 1. I
     stopped it so I could re-run the live checks.

Re-checked:
- `go test -race ./...` in spikes/mcp: 20/20 pass.
- Root build, vet and test: green.
- Live: the curl checks in mcp.md §7. The Google leg is unchanged, so no new sign-in was needed.

# agy review M0-2a r2

VERDICT: FIX

## Findings
1. [severity: high] [VERIFIED] `spikes/mcp/consent.go:58` and `spikes/mcp/authserver.go:329`: `renderConsent` sets `Content-Security-Policy: … form-action 'self'; …`, while `POST /oauth/consent` responds with a cross-origin `303 See Other` redirect to the client's `redirect_uri` (e.g. `https://claude.ai/api/mcp/auth_callback?code=…` or `http://127.0.0.1:<port>/callback?code=…`). In W3C CSP3, Chrome and Safari enforce `form-action` across the entire `302`/`303` redirect chain of a form submission, blocking the OAuth callback redirect whenever the client origin differs from `'self'` (missed in round 2 because Go `net/http` tests ignore CSP and no browser sign-in was re-run) → either include the validated target origin in `form-action` (`fmt.Sprintf("… form-action 'self' %s://%s; …", ru.Scheme, ru.Host)`) or omit `form-action` (`default-src 'none'` disables scripts and `html/template` escapes all fields), and assert in `TestConsentPage` that any `form-action` directive allows `ru.Scheme + "://" + ru.Host`.
2. [severity: med] [VERIFIED] `spikes/mcp/authserver.go:220,227,258,264,270,320-322` and `spikes/mcp/consent.go:30`: Pre-approval error paths and the consent **Deny** button call `s.provider.WriteAuthorizeError`, which `303`-redirects the browser to any DCR-registered `redirect_uri` once `ar.IsRedirectURIValid()` is true (N6, N7). Live check: unauthenticated `POST /oauth/register` (`https://evil.example/cb`) + `GET /oauth/authorize` without `code_challenge` immediately returns `303 Location: https://evil.example/cb?error=invalid_request…` (zero-interaction open redirect); likewise, clicking **Deny** for an unrecognised `{{.Host}}` (`consent.go:30`) still `303`-redirects the user's browser to `evil.example` → never redirect to an unconsented DCR `redirect_uri`: write a local `400`/`403` error response in `handleAuthorize`, `handleGoogleCallback`, and when `decision != "approve"` in `handleConsent` (redirecting to `c.ar.GetRedirectURI()` only on `decision == "approve"`), and update the corresponding error tests.
3. [severity: low] [VERIFIED] `spikes/mcp/authserver.go:199` and `docs/build/spikes/mcp.md:86`: `validRedirectURI` checks `strings.HasPrefix(strings.ToLower(raw), strings.ToLower(s.issuer)+"/oauth/")` on the raw string rather than the parsed URL, so `http://localhost:18923//oauth/google/callback` and `http://localhost:18923/./oauth/google/callback` return `201 Created`; also `mcp.md:86` omits `, scope="mcp"` from the sequence's `WWW-Authenticate` header → check parsed `u.Hostname()` against `s.issuer`'s host plus `strings.HasPrefix(path.Clean("/"+u.Path)+"/", "/oauth/")`, and update `mcp.md:86` (plus §3/§7 error-handling prose for finding 2).

## Checks run
- Round-1 findings 1–4 verification (`spikes/mcp/{authserver.go,consent.go,main.go,mcpserver.go,.gitignore,server_test.go}`) → VERIFIED fixed for token handling (`DisableRefreshTokenValidation: true`, `tu == fosite.AccessToken`, `Scopes: ["mcp"]`), PKCE pre-check + `maxPending=100` cap, consent CSRF (`128`-bit token in form + `HttpOnly` `SameSite=Strict` cookie, single-use, `html/template` escaping of `ClientName`/`User`/`Host`, raw email hidden, `X-Frame-Options: DENY`), fail-closed `checkGeneration`, HTTP/MCP timeouts, and `/mcp` in `.gitignore`, subject to findings 1–3 above.
- `git -C .../M0-2a diff main...HEAD --stat && git status --short` and secret/email grep across `git log -p main..HEAD` → VERIFIED: clean worktree, root module/`CLAUDE.md`/`docs/plan/*` untouched, 0 secrets, tokens, client IDs, or allowlist emails in diff/history.
- Root `go build ./... && go vet ./... && go test -count=1 ./...` and `spikes/mcp` `go vet ./... && go test -v -race -count=1 ./...` → VERIFIED: all green (20/20 spike tests pass under `-race`).
- Live compiled binary check (`MCP_LISTEN_ADDR=localhost:18923`, dummy env, `.env` untouched/unprinted, process terminated and port verified clean via `lsof`): `curl -si /mcp` → `401` with `Bearer resource_metadata="…", scope="mcp"`; DCR rejects `https:///cb`, `https:opaque`, `https://claude.ai@evil.example/cb`, and `/oauth/token` (`400`), but accepts `//oauth/google/callback` and `/./oauth/google/callback` (`201`); `GET /oauth/authorize` without `code_challenge` does not go to Google, but `303`-redirects to `https://evil.example/cb` → VERIFIED.
- Scratch PoC test against unmodified `spikes/mcp` → VERIFIED: refresh token on `/mcp` now returns `401`, `TestTokenWithoutMCPScopeForbidden` returns `403`, and `POST /oauth/consent` emits `form-action 'self'` alongside a cross-origin `303 Location` (Brendan's round-1 live Google sign-in in `mcp.md:158-173` UNVERIFIED directly per instructions).

## Builder response
1. **Fixed.**
   - The CSP is now `form-action 'self' <scheme>://<host>` of the validated redirect URI.
   - `TestConsentPage` asserts the form-action allows the client origin.
   - Live real-browser check: Brendan's Google → consent → Allow got `POST /oauth/consent -> 303` to Claude Code's loopback
     callback, then token 200, then `hello` OK.
2. **Fixed.**
   - `handleAuthorize`, `handleGoogleCallback` and `handleConsent` use a local `authorizeError` instead of
     `WriteAuthorizeError`: 400 for an invalid request or missing PKCE, 403 for a failed or unverified Google sign-in, a
     non-allowlisted account or Deny, and 500 when issuing the code fails.
   - Only `decision == "approve"` redirects to the client.
   - Tests assert a local status with no `Location` header: `assertLocalError` in the PKCE, non-allowlisted, unverified,
     Deny and empty-decision cases. `TestConsentApproveRedirectsToClient` covers the Allow path.
3. **Fixed.**
   - `isOwnOAuthEndpoint` compares the parsed `Hostname()` (case-insensitive) with the issuer host, and checks
     `path.Clean("/"+u.Path)+"/"` for the `/oauth/` prefix.
   - Tests cover `//oauth/…`, `/./oauth/…`, `/x/../oauth/…` and `/%6Fauth/…` (all 400), and `/oauthx/…` (201).
   - `mcp.md:86` now includes `scope="mcp"`, and the §3/§7 error prose is updated.

# agy review M0-2a r3

VERDICT: PASS

## Findings
1. [severity: low] [VERIFIED] `spikes/mcp/main.go:59`, `spikes/mcp/mcpserver.go:48-49`, and `docs/build/spikes/mcp.md:219`: `go-sdk@v1.8.0` (`mcp/streamable.go:318-326`) auto-enables loopback DNS-rebinding protection unless `DisableLocalhostProtection` is set, rejecting non-loopback `Host` headers (`403 Forbidden: invalid Host header "finance.<domain>"`) whenever the listener's local address is loopback (`127.0.0.1`/`[::1]`) → in M0-2b on `hoid`, set container `MCP_LISTEN_ADDR=:8080` on the internal Docker network with no host `ports:` mapping (never proxy to `127.0.0.1:8080` on the host unless `DisableLocalhostProtection` is enabled behind NPM).

## Checks run
- `git -C .../M0-2a diff main...HEAD --stat && git status --short --ignored` and secret/email grep across `git log -p main..HEAD -- ':!spikes/mcp/go.sum'` → **VERIFIED**: clean worktree (`.env`, `spikes/mcp/.data/`, `spikes/mcp/mcp-spike` gitignored; `.env` untouched/unprinted); only `spikes/mcp/*`, `docs/build/spikes/mcp.md`, and `docs/build/tasks/M0-2a.md` touched; 0 secrets, client IDs, tokens, or allowlisted emails in commits or diff.
- Root `go build ./... && go vet ./... && go test -count=1 ./...` and `spikes/mcp` `go vet ./... && go test -v -race -count=1 ./...` → **VERIFIED**: all green (21/21 spike tests pass under `-race`).
- R1 + R2 findings & full auth-flow audit (`spikes/mcp/{authserver.go,consent.go,google.go,mcpserver.go,revocation.go,allowlist.go,main.go}` and `docs/build/spikes/mcp.md`) → **VERIFIED**:
  - R1-1 (`authserver.go:81,434-441`, `mcpserver.go:50-53`): `DisableRefreshTokenValidation: true`, `tu == fosite.AccessToken`, and `Scopes: ["mcp"]` enforced.
  - R1-2 / R2-3 (`authserver.go:195-224`): strict `validRedirectURI` + `isOwnOAuthEndpoint` comparing parsed `Hostname()` (`EqualFold`) and `path.Clean("/"+u.Path)+"/"` against `/oauth/`.
  - R1-3 / R2-2 (`authserver.go:229-352`): S256 PKCE checked before parking (`maxPending=100`); all pre-approval errors (`handleAuthorize`, `handleGoogleCallback`) and consent **Deny** return local `text/plain` (`X-Content-Type-Options: nosniff`, `Cache-Control: no-store`) `400`/`403` responses with no `Location` header; only `decision == "approve"` redirects (`303`).
  - R1-4 (`authserver.go:403-431`, `main.go:81-86`, `mcpserver.go:47-49`, `.gitignore:1-3`): fail-closed `checkGeneration`, HTTP/MCP timeouts, shared `*mcp.Server`, `/mcp` gitignored.
  - R2-1 (`consent.go:61-62`, `mcp.md:86,99-106`): CSP sets `form-action 'self' <scheme>://<host>` (and `net/url` host parsing blocks whitespace/CRLF directive injection); `mcp.md` sequence and error prose match implementation.
- Live compiled binary check (`MCP_LISTEN_ADDR=localhost:18929`, dummy Google env in temp dir, terminated and verified clean via `lsof -iTCP:18929 -sTCP:LISTEN`) → **VERIFIED**: `GET /mcp` → `401` with `WWW-Authenticate: Bearer resource_metadata="http://localhost:18929/.well-known/oauth-protected-resource", scope="mcp"`; well-known RFC 9728/8414 JSON valid; DCR returns `400` on `https:///cb`, `https:opaque`, `https://claude.ai@evil.example/cb`, `https://claude.ai/cb#frag`, `http://evil.example/cb`, `http://0177.0.0.1:8080/cb`, `/oauth/token`, uppercase `/oauth/consent`, `//oauth/google/callback`, `/./oauth/google/callback`, `/x/../oauth/token`, `/%6Fauth/token`, and `/%2foauth/token` (`201` on `https://claude.ai/api/mcp/auth_callback` and `http://127.0.0.1:9999/callback`); `GET /oauth/authorize` without `code_challenge`, with `plain`, or with bad `scope` for `https://evil.example/cb` returns local `400` with empty `Location` (while valid S256 `302`s to `accounts.google.com`); CLI `revoke` writes `0600` hashed-ID `generations.json`.
- Scratch Go PoC suite (`go test -v -race` against unmodified `spikes/mcp/*.go` copy) → **VERIFIED**: auth code and refresh token (`ory_rt_…`) as `/mcp` bearer → `401`; real fosite access token with `offline_access`-only scope on `/mcp` → `403`; consent CSP `form-action` permits both `https://claude.ai` and loopback origins matching `303` `Location`; **Deny** and `?error=access_denied` Google callbacks return local `403` with empty `Location`; multi-hop refresh preserves `hello` identity/generation, reuse revokes the token family, and generation bump invalidates rotated access (`401`) and refresh (`400 invalid_grant`) tokens; cross-user `Mcp-Session-Id` hijack → `403`; cross-client RFC 7009 `/oauth/revoke` blocked while owner client revokes immediately. (Brendan's post-r2 live browser sign-in in `M0-2a.md:133-135` / `mcp.md:193-197` **UNVERIFIED** directly per instructions.)
