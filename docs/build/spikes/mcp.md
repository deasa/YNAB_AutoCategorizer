# Spike M0-2a: MCP Go SDK + OAuth (local)

Code: `spikes/mcp/` (own module; root module untouched). Run: `cd spikes/mcp && go run .`.
Labels: **VERIFIED** = checked in this spike (by running code, reading the dependency's source at the pinned version, or
an official doc fetched on 2026-10-29). **UNVERIFIED** = from secondary sources or not yet exercised.

## 1. SDK comparison and choice (U8)

| | `github.com/modelcontextprotocol/go-sdk` | `github.com/mark3labs/mcp-go` |
|---|---|---|
| Latest version (`go list -m -versions`, 2026-10-29) | **v1.8.0** (stable v1 API) | v1.4.0 |
| Streamable HTTP server | `mcp.NewStreamableHTTPHandler`: VERIFIED (used here) | `server.NewStreamableHTTPServer`: VERIFIED (source) |
| Bearer-token middleware | `auth.RequireBearerToken(verifier, opts)`: writes 401 + `WWW-Authenticate: Bearer resource_metadata=…`, checks expiry and scopes, and puts `TokenInfo` on the request. Tool handlers read it as `req.Extra.TokenInfo`. VERIFIED (used here) | No packaged verifier. You write your own via `WithHTTPContextFunc`. VERIFIED (source) |
| RFC 9728 protected-resource metadata | `auth.ProtectedResourceMetadataHandler` (with CORS). VERIFIED | `server.NewProtectedResourceMetadataHandler`. VERIFIED (source) |
| Authorization server | None. `oauthex` is client-side only (DCR, AS-metadata discovery, token exchange). VERIFIED | None. VERIFIED (source) |
| Spec revisions | 2024-11-05, 2025-03-26, 2025-06-18, 2025-11-25, **2026-07-28** (latest). VERIFIED (`mcp/shared.go`) | Same set up to 2026-07-28. VERIFIED (`mcp/types.go`) |
| Maintenance | Official, under the MCP org (co-maintained with Google) | Community. Mature and widely used |

**Choice: `github.com/modelcontextprotocol/go-sdk` v1.8.0.** It is official, its v1 API is stable, it supports the latest
spec revision, and its auth package covers the whole resource-server side: bearer verification, the RFC 9728 document and
the 401 challenge. That leaves only a `TokenVerifier` func for us to write. Its typed `mcp.AddTool` generates the input and
output schemas. This matches architecture §4.

## 2. OAuth approach

Neither SDK ships an authorization server, so option (c) alone isn't enough. It covers the resource server only.

**Chosen: (a) + (c).** The authorization server is a minimal **`github.com/ory/fosite` v0.49.0** setup inside the binary.
The resource server uses the go-sdk auth helpers. Login is delegated to Google via `github.com/coreos/go-oidc/v3` +
`golang.org/x/oauth2`.

- fosite does all the protocol and crypto work: authorization-code grant, **PKCE S256 enforced** (plain disabled), opaque
  HMAC-SHA512/256 token generation and validation, refresh-token **rotation** with reuse detection, `invalid_grant` errors,
  RFC 7009 revocation, and introspection.
- go-oidc verifies Google's ID token (signature via Google JWKS, `aud`, `iss`, `exp`). We also check the nonce, and we use
  PKCE on the Google leg too.
- Code we own (~500 lines, no crypto): endpoint wiring, the RFC 8414 metadata JSON, DCR bookkeeping (strict redirect-URI
  validation, store a public client), a pre-Google PKCE check, parking the authorize request while the user is at Google,
  the allowlist check, a CSRF-protected consent page, and the revocation generation counter.
- fosite settings that matter:
  - `EnforcePKCE`, with plain PKCE disabled.
  - `DisableRefreshTokenValidation: true`, so introspection never accepts a refresh token. The `/mcp` verifier also rejects
    any token whose use isn't `access_token`.
  - `ExactScopeStrategy`. Authorize always adds the `mcp` scope, and `/mcp` requires it (403 otherwise).

Why not (b), an external AS: Cloudflare Access for MCP requires the domain to be proxied through Cloudflare, but hoid is
exposed via nginx-proxy-manager (Q16). A self-hosted AS such as Ory Hydra or Keycloak is another container, another DB and
another login/consent integration. That is more moving parts than the code above, and we'd still have to write the Google
login/consent app for Hydra.

How Claude's requirements map onto this design. Source: [claude.com/docs/connectors/building/authentication](https://claude.com/docs/connectors/building/authentication),
fetched 2026-10-29. Each item is VERIFIED against that doc:
- The server must return 401 with `WWW-Authenticate: Bearer resource_metadata="…"`, and Claude ignores the header on a 200.
  ✔ (go-sdk middleware)
- `resource` must equal the MCP URL exactly. Claude uses only the **first** `authorization_servers` entry. ✔
- RFC 8414 AS metadata at `/.well-known/oauth-authorization-server`. ✔
- Client registration: **DCR (RFC 7591)** or CIMD are both "supported by default". Claude picks CIMD only if the server
  advertises `client_id_metadata_document_supported: true`; otherwise it uses DCR. We implement DCR and leave CIMD out. ✔
- DCR and CIMD clients are public clients, so the server needs `token_endpoint_auth_methods_supported: ["none"]`. ✔
- PKCE S256 on every request, advertised in `code_challenge_methods_supported`. ✔
- Refresh happens on a 401 or within 5 minutes of expiry. The server must rotate refresh tokens for public clients and return
  `invalid_grant` for a dead refresh token. ✔ (fosite, covered by tests)
- `/token` takes form-urlencoded and `/register` takes JSON. ✔
- Redirect URIs: `https://claude.ai/api/mcp/auth_callback` (web, Desktop, mobile, Cowork). Claude Code uses a loopback
  `http://localhost:<port>/callback`. With DCR, the client registers its exact URI, so an exact match works. Port-agnostic
  matching is only needed for CIMD.
- Claude appends `offline_access` when the AS lists it in `scopes_supported`. We list it, and we issue a refresh token
  regardless.
- Endpoints have to answer within 10 s (30 s for refresh). Egress comes from `160.79.104.0/21`.

## 3. Endpoints and flow

| Path | Purpose |
|---|---|
| `POST /mcp` (+ GET/DELETE) | Streamable HTTP MCP endpoint, behind the bearer middleware |
| `GET /.well-known/oauth-protected-resource[/mcp]` | RFC 9728 metadata (`resource`, `authorization_servers`, `scopes_supported: [mcp]`) |
| `GET /.well-known/oauth-authorization-server` | RFC 8414 metadata |
| `POST /oauth/register` | RFC 7591 DCR. Public clients only. Each redirect URI must be https, or http on loopback, and must have a host. It must have no userinfo, no fragment, must not be opaque, and must not point at our own `/oauth/*` (parsed host plus `path.Clean`ed path). Capped at 500 clients |
| `GET /oauth/authorize` | fosite validates the request (client, redirect, scope, state). We require an S256 `code_challenge` **before** parking anything (otherwise a local 400 page), add the `mcp` scope, park the request (cap: 100 pending logins plus consents, else 503), and redirect to Google |
| `GET /oauth/google/callback` | Google redirect URI (registered in GCP). It checks the allowlist, then renders the consent page |
| `POST /oauth/consent` | Consent form (CSRF token in the form and in a `SameSite=Strict` HttpOnly cookie). Allow issues the code and redirects to the client; Deny shows a local 403 page |
| `POST /oauth/token` | `authorization_code` and `refresh_token` grants |
| `POST /oauth/revoke` | RFC 7009 single-token revocation |

Sequence:
1. The client calls `POST /mcp` with no token and gets 401 with `WWW-Authenticate: Bearer resource_metadata="<issuer>/.well-known/oauth-protected-resource", scope="mcp"`.
2. The client GETs the protected-resource metadata, then the AS metadata at `<issuer>/.well-known/oauth-authorization-server`.
3. The client calls `POST /oauth/register` with its `redirect_uris` and gets back a `client_id`. The client is public, with
   `token_endpoint_auth_method: none`.
4. The browser opens `/oauth/authorize?response_type=code&client_id…&redirect_uri…&code_challenge…&state…&scope=mcp[ offline_access]&resource…`.
5. finbot validates that with fosite and parks the request under a fresh random `state`. It then redirects to Google with
   `scope=openid email profile`, its own PKCE S256 challenge, a nonce, and `prompt=select_account`.
6. The user signs in at Google. Google redirects to `/oauth/google/callback?code&state`.
7. finbot exchanges the code with Google and verifies the ID token and nonce. It requires `email_verified=true` and the email
   to be in `MCP_ALLOWED_EMAILS`.
   - If either check fails, the browser gets a local 403 page ("google account is not allowed to use this server"). No code
     is issued.
   - If both pass, finbot shows its **consent page**: the signed-in user's name, the client's `client_name`, and the
     `redirect_uri` host. Loopback hosts get an extra warning. The page sets `X-Frame-Options: DENY` and a CSP with
     `frame-ancestors 'none'` and `form-action 'self' <redirect origin>`. The redirect origin has to be listed because
     browsers apply `form-action` to the redirect that follows the POST. The user's Allow/Deny goes as a CSRF-checked,
     single-use `POST /oauth/consent`.
   - **Error rule:** until the user clicks Allow, the DCR-registered `redirect_uri` is untrusted. Every error before
     approval, and Deny, is answered on finbot's own origin with a local 400/403/503 page and never redirected to the
     client. That avoids an open redirect to attacker-registered URIs. The trade-off is that a client waiting on its
     callback sees no error and the user closes the window. Only Allow redirects (303 with `code` and `state`).
   - On Allow, a session is created with `subject=u_<sha256(email)[:12 hex]>`, the display name, and the user's current
     revocation generation. fosite then redirects to the client's `redirect_uri` with `code` and `state`.
8. The client calls `POST /oauth/token` with `grant_type=authorization_code`, the `code_verifier` and its `client_id`, and
   gets back an `access_token`, a `refresh_token` and `expires_in`.
9. The client calls `POST /mcp` with `Authorization: Bearer …`. The verifier (fosite introspection) checks the token, then the
   generation, and returns `TokenInfo{UserID, Extra.name}`. The `hello` tool returns `{user_id, display_name}`.

Google's access and refresh tokens are discarded right after the ID token is verified. Nothing from Google is stored.

## 4. Token format and lifetime

- Access and refresh tokens are fosite's opaque HMAC tokens (`ory_at_…` / `ory_rt_…`): a random key plus an HMAC signature
  under a 32-byte global secret. They are not JWTs, so they can only be validated by looking them up in the store. That is
  what makes revocation immediate.
- The access token lives **1 h**. The refresh token lives **30 days** and **rotates on every use**. Reusing an old refresh
  token returns `invalid_grant` and revokes that token family. The authorization code lives 5 min. A parked Google login
  lives 10 min.
- Spike storage is all in memory: the fosite `MemoryStore`, the HMAC secret and the DCR clients. A restart invalidates
  everything, and clients then re-register and re-auth. Production (M3+) should keep fosite storage in SQLite (fosite's
  storage interfaces) and keep the HMAC secret in the 0600 secrets file, so restarts don't force a re-login.

## 5. Revocation

- **One step:** `go run . revoke <email>` (or `revoke --all`) bumps that user's generation counter in
  `$MCP_DATA_DIR/generations.json` (gitignored `.data/`, keyed by the hashed user id, so no emails are stored).
- Every session records the generation it was issued under. The `/mcp` verifier and the `/token` endpoint (both code and
  refresh grants) re-read the file and reject a mismatch. This is effective on the very next request, with no restart.
  Access tokens get a 401, and refresh tokens get `invalid_grant`, so the client has to sign in with Google again.
- To lock someone out permanently, also remove them from `MCP_ALLOWED_EMAILS` and restart.
- Clients can also revoke a single token themselves through RFC 7009 `POST /oauth/revoke`.

## 6. Client support (U9)

| Client | Remote MCP + OAuth | DCR | Status | Source |
|---|---|---|---|---|
| Claude web (claude.ai) | Yes, custom connector by URL | Yes (default), or CIMD | **VERIFIED (docs)** | [Claude connector auth](https://claude.com/docs/connectors/building/authentication): "The same authentication infrastructure backs claude.ai, Claude Desktop, Claude mobile, Claude Code, and Cowork" |
| Claude Desktop | Yes (same infrastructure, same callback) | Yes | **VERIFIED (docs)** | same |
| Claude mobile | Yes (same infrastructure). Connectors are added on web or desktop and then sync | Yes | **VERIFIED (docs)** for auth. Adding a connector from mobile is UNVERIFIED | same |
| Claude Code | Yes, `claude mcp add --transport http` | Yes, with a loopback redirect | **VERIFIED (ran it locally, §7)** | same + this spike |
| ChatGPT | Yes, via apps/connectors in developer mode | Yes when `registration_endpoint` is advertised. Prefers CIMD | **VERIFIED (docs)** for the protocol. Plan gating (Plus vs Pro/Team) is **UNVERIFIED** | [OpenAI Apps SDK auth](https://developers.openai.com/apps-sdk/build/auth). Redirect: `https://chatgpt.com/connector_platform_oauth_redirect` (or `https://chatgpt.com/connector/oauth/{callback_id}`) |
| Gemini app (consumer) | Reported for Google AI Ultra only | Reported | **UNVERIFIED**: no Google doc found, and third-party reports conflict | e.g. [harvv.com/install/gemini](https://harvv.com/install/gemini) |
| Gemini Enterprise | Admin-configured custom MCP connector, with a manually created client and no DCR | No | **UNVERIFIED** (doc not fetched) | [Google Cloud doc](https://docs.cloud.google.com/gemini/enterprise/docs/connectors/custom-mcp-server/set-up-custom-mcp-server) |
| Gemini CLI | Yes, Streamable HTTP + OAuth auto-discovery | Yes | **VERIFIED (docs)** | [gemini-cli MCP docs](https://github.com/google-gemini/gemini-cli/blob/main/docs/tools/mcp-server.md) |

Since we support DCR, no per-client setup is needed for Claude, ChatGPT or Gemini CLI. Our DCR accepts any https redirect,
so neither claude.ai's nor ChatGPT's callback needs to be pre-registered.

## 7. Local end-to-end results

All on 2026-10-29 against `go run .` on `localhost:8080`. The client was Claude Code (scratch-dir `--scope local`), because
running MCP Inspector via `npx` was blocked as external code in this environment.

- `curl -i localhost:8080/mcp` → `401 Unauthorized`,
  `Www-Authenticate: Bearer resource_metadata="http://localhost:8080/.well-known/oauth-protected-resource"`. VERIFIED
- Protected-resource metadata → `{"resource":"http://localhost:8080/mcp","authorization_servers":["http://localhost:8080"],"scopes_supported":["mcp"],"bearer_methods_supported":["header"],"resource_name":"finbot (spike)"}`.
  AS metadata → issuer plus authorize, token, register and revoke endpoints, `code_challenge_methods_supported:["S256"]`,
  `token_endpoint_auth_methods_supported:["none"]`. VERIFIED
- `claude mcp add --transport http --scope local finbot-spike http://localhost:8080/mcp` showed "Needs authentication",
  which means discovery worked. Brendan then ran `/mcp` → Authenticate → Google. Server log:
  ```
  POST /oauth/register -> 201   (client "Claude Code (finbot-spike)")
  GET /oauth/authorize -> 302
  login ok for u_9f51ae389a5c (gen 0)
  GET /oauth/google/callback -> 303
  POST /oauth/token -> 200
  POST /mcp -> 200 …
  ```
  VERIFIED
- `claude -p "Call the hello tool…" --allowedTools mcp__finbot-spike__hello` → `{"display_name":"Brendan Ashton","user_id":"u_9f51ae389a5c"}`.
  The raw email is never returned. VERIFIED
- Revocation: `./mcp-spike revoke <email>` → `revoked tokens for u_9f51ae389a5c (generation now 1)`. On Claude Code's next
  call with the same token, the log shows `POST /mcp -> 401`. Its refresh attempt then shows `POST /oauth/token -> 400`
  (`invalid_grant`), and Claude Code drops the tools until the user re-authenticates. VERIFIED
- Review fixes (agy, 4 findings), re-checked live on a fresh `go run .`. Brendan's sign-in wasn't repeated because the
  Google leg is unchanged. VERIFIED:
  - 401 now reads `Bearer resource_metadata="…", scope="mcp"`.
  - DCR now returns 400 for `https:///cb`, `https:opaque`, `https://claude.ai@evil.example/cb` and `http://localhost:8080/oauth/token`.
    `https://claude.ai/api/mcp/auth_callback` still returns 201.
  - `/oauth/authorize` without PKCE returns a local 400 page (no `Location`), never the client or Google. With S256 it
    returns 302 to Google. (Round 2 changed this. Round 1 redirected `invalid_request` to the client.)
  - Unit tests: refresh token as bearer → 401; token without `mcp` → 403; the `mcp` scope is always granted; missing,
    plain or method-less PKCE → local 400 with nothing parked; wrong `code_verifier` → `invalid_grant`; pending cap
    → 503; consent page escaping, cookie flags and no-framing; consent with no cookie, wrong CSRF, unknown id or a replay
    → 403; Deny → local 403, no redirect; Allow → 303 to the client with a code; the CSP `form-action` allows the redirect
    origin; DCR rejects `//oauth/…`, `/./oauth/…`, `/x/../oauth/…` and `/%6Fauth/…` on our host; generation check fails
    closed.
- Round 2, real browser (live check of agy r2 finding 1). Brendan signed in again via Claude Code. Server log:
  `GET /oauth/google/callback -> 200` (consent page), `login ok for u_9f51ae389a5c (gen 1)`,
  `POST /oauth/consent -> 303` (the CSP `form-action` let the cross-origin redirect to Claude Code's loopback callback
  through), `POST /oauth/token -> 200`, `POST /mcp -> 200`. `hello` → `{"display_name":"Brendan Ashton","user_id":"u_9f51ae389a5c"}`.
  `gen 1` is from the earlier live revoke, which shows a fresh sign-in after a revoke works. VERIFIED
- Non-allowlisted, unverified-email and empty-allowlist cases: these are covered by unit tests through the full DCR,
  authorize and callback path, using a fake IdP (`TestNonAllowlistedAccountRejected`, `TestUnverifiedEmailRejected`).
  Each test expects a local 403 page ("not allowed" / "not verified"), with no redirect and no code. A real second Google
  account was not tried. VERIFIED (tests)
- Unit tests for the same-token-after-revoke path: `TestHelloThenRevoke` (hello via the go-sdk client, then 401, then
  `invalid_grant`, then a fresh login that works), `TestRefreshRotates` and `TestOtherUserRevokeDoesNotAffect`.

## 8. What M0-2b must verify on hoid

1. **Public issuer:** set `MCP_PUBLIC_URL=https://finance.<domain>`. The issuer, `resource` (`…/mcp`), all endpoint URLs and
   the Google redirect are built from it, never from request `Host` or `X-Forwarded-*` headers, so the proxy can't spoof
   them. Check that `curl https://finance.<domain>/.well-known/oauth-protected-resource` shows `resource` exactly equal to the
   URL entered in Claude.
2. **Second redirect URI:** Brendan adds `https://finance.<domain>/oauth/google/callback` to the GCP web client. The Google
   consent screen is in **Testing** mode with 2 test users. Testing-mode refresh tokens expire after 7 days, but we don't use
   Google refresh tokens, so this doesn't affect us. Confirm that the wife's account is a test user.
3. **nginx-proxy-manager:**
   - Pass `Authorization` through untouched.
   - Don't buffer SSE: `proxy_buffering off` on `/mcp`, and long `proxy_read_timeout`.
   - Allow GET/POST/DELETE/OPTIONS.
   - Serve `/.well-known/*` from finbot, not NPM's ACME location. Only `/.well-known/acme-challenge/` should go to NPM.
   - Bind `MCP_LISTEN_ADDR` to the docker network, not 0.0.0.0 on the host.
4. **Reachability:** Anthropic egress `160.79.104.0/21` must reach `/mcp`, `/.well-known/*` and `/oauth/*`. Rate limits and
   fail2ban must not block it, and the endpoints must answer within 10 s.
5. **Real connectors:** claude.ai web connector (callback `https://claude.ai/api/mcp/auth_callback`), then Claude mobile
   using the same connector, then ChatGPT developer mode, then Gemini if available. ChatGPT mentions "issuer identification"
   for its stable redirect. Check whether it needs RFC 9207 `iss` in the authorization response, which fosite doesn't add by
   default. If it does, add `authorization_response_iss_parameter_supported` and the `iss` parameter.
6. **Revoke on hoid:** run `docker exec … finbot revoke <email>` and confirm that the next tool call 401s and the client
   prompts for re-auth.
7. **Persistence decision:** with in-memory storage, every deploy logs both users out. Decide whether M0-2b can accept that or
   whether fosite SQL storage is needed first.

## 9. Recommendation

Adopt **go-sdk v1.8.0 + fosite v0.49.0 + go-oidc (Google)** as built in `spikes/mcp/`, with DCR (not CIMD), opaque
introspected tokens, and per-user generation revocation. Move it into an `mcpserver/` and `oauth/` package pair in M3, and
swap `storage.MemoryStore` for a SQLite-backed fosite store.

Hardening before going public:
- Add CIMD only if DCR client sprawl becomes a problem. It won't at 2 users, and the 500-client cap is fine.
- Expire unused DCR clients.

## 10. Impact on architecture (§4)

- §4 "Auth (Q17)": resolved as option **(a) fosite AS + (c) go-sdk resource-server helpers**. (b) was rejected because
  exposure goes via NPM, not Cloudflare. No hand-rolled crypto.
- §4 "Go SDK … version to verify (U8)": **v1.8.0**, spec 2026-07-28. U8 is closed.
- U9: Claude (all surfaces) and Gemini CLI are verified from docs. ChatGPT is verified in its docs, but its plan gating is
  unverified. The consumer Gemini app is unverified. M0-2b tests the real connectors.
- New config: `MCP_PUBLIC_URL`, `MCP_LISTEN_ADDR`, `MCP_ALLOWED_EMAILS`, `MCP_DATA_DIR`, `GOOGLE_OAUTH_CLIENT_ID`,
  `GOOGLE_OAUTH_CLIENT_SECRET`.
- New persistent state, for when it moves from memory to SQLite: OAuth clients, tokens and sessions, and the per-user token
  generation. N9 attribution: `TokenInfo.UserID` is the stable hashed user id that tool handlers use for the write log. Map
  it to a display name ("Brendan" / his wife) in config.
- N7 "revoke quickly": the `revoke` subcommand works without a restart. Add a Slack `/finbot revoke` later as an admin path.

## 11. Deployed on hoid (M0-2b)

Deployed 2026-10-29. It stays running until sup takes it down. Sign-ins are lost on every redeploy because the fosite store
is in memory (sup's decision for the spike). Persistent storage is M3 work.

### Compose layout

The router doesn't forward 80/443 to hoid, so the server is exposed through a **Cloudflare Tunnel** (`hoid`). Its public
hostname `finance.hoidlabs.site` routes to `http://finbot:8080`. Cloudflare terminates TLS. Nothing else on hoid is involved:
no nginx, no nginx-proxy-manager, no certbot, no sudo.

```
~/finbot-spike/mcp/          (0700)
  compose.yaml               spikes/mcp/deploy/compose.yaml   (project name: finbot-spike)
  src/                       rsync of spikes/mcp (built on hoid, no registry)
  mcp.env       (0600)       GOOGLE_OAUTH_CLIENT_ID/SECRET, MCP_ALLOWED_EMAILS: streamed from local .env over ssh stdin
  tunnel.env    (0600)       TUNNEL_TOKEN only: sed-extracted on hoid from ~/finbot/.env (never printed)
services:
  finbot       image finbot-spike-mcp (multi-stage: golang:1.26-alpine -> distroless/static:nonroot)
               MCP_LISTEN_ADDR=:8080, MCP_PUBLIC_URL=https://finance.hoidlabs.site, MCP_DATA_DIR=/data
               volume finbot-spike-data:/data, env_file mcp.env, NO ports:, restart unless-stopped
  cloudflared  cloudflare/cloudflared:2026.10.6, `tunnel --no-autoupdate run`, env_file tunnel.env,
               restart unless-stopped
```

- Deploy: run `spikes/mcp/deploy/deploy.sh` from the repo root (it runs rsync, writes the env files, then
  `docker compose up -d --build`).
- Revoke: `docker compose exec -T finbot /mcp-spike revoke --all`. The image has no shell, so `revoke <email>` would need
  the email on argv; `--all` avoids that.
- Secret handling: `docker inspect` `Args`/`Cmd` hold no token. finbot's environment has only its own keys. cloudflared's
  has only `TUNNEL_TOKEN`. Neither sees `CLAUDE_CODE_OAUTH_TOKEN`.
- Two hoid facts that matter here:
  - `ledger-web` already publishes host port 8080. That's harmless, since finbot publishes nothing and cloudflared reaches it
    over the compose network.
  - The go-sdk loopback DNS-rebinding guard (agy r3) doesn't trigger, because finbot listens on the container interface.

### §8 checklist

| # | Item | Status | Evidence |
|---|---|---|---|
| 1 | Public issuer | **VERIFIED** | `/.well-known/oauth-protected-resource` (and `/mcp` variant) → `"resource":"https://finance.hoidlabs.site/mcp","authorization_servers":["https://finance.hoidlabs.site"]`. AS metadata: `"issuer":"https://finance.hoidlabs.site"`, with every endpoint under it. `curl -i …/mcp` → `401`, `www-authenticate: Bearer resource_metadata="https://finance.hoidlabs.site/.well-known/oauth-protected-resource", scope="mcp"` |
| 2 | Second Google redirect URI | **VERIFIED** | `GET /oauth/google/callback -> 200` after a real Google sign-in through the public URL. The wife's test-user status was not exercised (**UNVERIFIED**) |
| 3 | Proxy, adapted for Cloudflare | **VERIFIED** | `Authorization` passes through: bearer `POST /mcp -> 200` after sign-in. Streaming works: go-sdk answers POSTs as `text/event-stream` (JSONResponse off), and those 200s reached claude.ai. GET/POST/DELETE/OPTIONS reach finbot (`OPTIONS /mcp -> 204`). `/.well-known/*` is served by finbot (the bodies above come from finbot, not Cloudflare). Nothing to bind on the host: no `ports:` |
| 4 | Reachability, no blocking, < 10 s | **VERIFIED** | From the Mac, every endpoint answered in 0.12–0.30 s. There was no challenge or bot-fight page for an empty user agent, `python-httpx`, `Claude-User`, `openai-mcp` or `Go-http-client`: each got finbot's own 401 or 200. Anthropic's real egress got through (see 5). No Cloudflare dashboard change was needed |
| 5 | Real connectors | **VERIFIED** for Claude web and mobile; ChatGPT and Gemini **UNVERIFIED** (skipped) | See the connector results below |
| 6 | Revoke on hoid | **VERIFIED** | See the revoke section below |
| 7 | Persistence decision | Decided | In memory for the spike (sup). M3 moves to a SQLite fosite store |

### Connector results (U9)

| Client | Result |
|---|---|
| claude.ai web | **VERIFIED**: Settings → Connectors → custom connector, URL `https://finance.hoidlabs.site/mcp`. DCR registered as "Claude", then Google sign-in, consent, Allow, and `hello` succeeded |
| Claude mobile | **VERIFIED**: the connector synced from web, and `hello` worked with no new DCR or sign-in (it reuses the web connector's tokens) |
| ChatGPT | **UNVERIFIED** (skipped: Brendan has no plan with developer mode). The RFC 9207 `iss` question stays open |
| Gemini | **UNVERIFIED** (skipped: not trivially available) |

Server log for the claude.ai web connection (client id redacted):
```
POST /mcp -> 401
GET /.well-known/oauth-protected-resource -> 200
GET /.well-known/oauth-authorization-server -> 200
dcr: registered client <id> ("Claude")
POST /oauth/register -> 201
GET /oauth/authorize -> 302
GET /oauth/google/callback -> 200
login ok for u_9f51ae389a5c (gen 0)
POST /oauth/consent -> 303
POST /oauth/token -> 200
POST /mcp -> 200 …
```

### Revoke against claude.ai

At 15:06:58 UTC, `docker compose exec -T finbot /mcp-spike revoke --all` printed
`revoked tokens for * (generation now 1)`. On claude.ai's next call:
```
15:07:37 POST /mcp -> 401            old access token rejected
15:07:38 POST /oauth/token -> 400    refresh attempt → invalid_grant
15:07:39 POST /mcp -> 401
15:07:47 POST /oauth/register -> 201 claude.ai re-registers
15:07:47 GET /oauth/authorize -> 302
15:07:53 login ok for u_9f51ae389a5c (gen 1)
15:07:53 POST /oauth/consent -> 303
15:07:55 POST /oauth/token -> 200
15:07:56 POST /mcp -> 200
```
What the client does: claude.ai tries a silent refresh first. When that fails, it shows the connector as needing to
reconnect, and Brendan has to re-authenticate. Reconnecting runs a fresh DCR, Google sign-in and consent, after which
`hello` succeeds again. **VERIFIED**

### Impact on architecture

- §4 exposure is a **Cloudflare Tunnel** (a `cloudflared` sidecar in the finbot compose project), not nginx-proxy-manager.
  finbot publishes no host port, and hoid needs no inbound ports, TLS certificates or sudo.
- "Rate-limit + fail2ban at the proxy" doesn't fit any more. fail2ban can't see client IPs on hoid: every request arrives
  from cloudflared. The replacements are:
  - Cloudflare **rate-limiting rules** on `/oauth/*` and `/mcp`. They must not challenge Anthropic's egress
    (`160.79.104.0/21`): either skip that range or use block-only rules (never managed challenge), because API callers can't
    solve challenges.
  - Optionally, a WAF custom rule that limits `/mcp` to known client egress ranges.
  - In-app limits, which already exist: DCR cap 500, pending-login cap 100.
  
  If finbot ever logs client IPs, read `CF-Connecting-IP` and trust it only from the cloudflared container.
- Bot Fight Mode is currently not challenging API callers. If Brendan ever turns on "Bot Fight Mode" or "Under Attack", add
  a WAF skip rule for `finance.hoidlabs.site` or recheck the connector.
- §10's "(b) rejected because exposure goes via NPM, not Cloudflare" no longer holds as a reason. Cloudflare Access (b) is
  still a poor fit, because MCP clients need standard OAuth discovery, so the fosite AS stays.
- Persistence: every redeploy logs everyone out until M3 moves to a SQLite fosite store plus a persisted HMAC secret.
