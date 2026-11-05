-- 0005: persistent OAuth state for the MCP authorization server (auth/), so
-- sign-ins survive a restart. Token tables are keyed by fosite's HMAC signature;
-- a raw token, code or verifier is never stored. request is the JSON of the
-- sanitized fosite request, including its session.

CREATE TABLE oauth_clients (
    client_id      TEXT NOT NULL PRIMARY KEY,
    name           TEXT NOT NULL,
    redirect_uris  TEXT NOT NULL, -- JSON array
    grant_types    TEXT NOT NULL, -- JSON array
    response_types TEXT NOT NULL, -- JSON array
    scopes         TEXT NOT NULL, -- JSON array
    created_at     TEXT NOT NULL
) STRICT;

CREATE INDEX oauth_clients_created ON oauth_clients (created_at);

-- A used code stays inactive until it expires, so a replay can revoke its grant.
CREATE TABLE oauth_auth_codes (
    signature  TEXT    NOT NULL PRIMARY KEY,
    request_id TEXT    NOT NULL,
    client_id  TEXT    NOT NULL REFERENCES oauth_clients (client_id) ON DELETE CASCADE,
    expires_at TEXT    NOT NULL,
    active     INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    request    TEXT    NOT NULL
) STRICT;

CREATE INDEX oauth_auth_codes_request ON oauth_auth_codes (request_id);
CREATE INDEX oauth_auth_codes_client ON oauth_auth_codes (client_id);
CREATE INDEX oauth_auth_codes_expires ON oauth_auth_codes (expires_at);

CREATE TABLE oauth_access_tokens (
    signature  TEXT NOT NULL PRIMARY KEY,
    request_id TEXT NOT NULL,
    client_id  TEXT NOT NULL REFERENCES oauth_clients (client_id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    request    TEXT NOT NULL
) STRICT;

CREATE INDEX oauth_access_tokens_request ON oauth_access_tokens (request_id);
CREATE INDEX oauth_access_tokens_client ON oauth_access_tokens (client_id);
CREATE INDEX oauth_access_tokens_expires ON oauth_access_tokens (expires_at);

-- A rotated or revoked token stays inactive until it expires, so a replay is
-- recognised as reuse and revokes its grant.
CREATE TABLE oauth_refresh_tokens (
    signature  TEXT    NOT NULL PRIMARY KEY,
    request_id TEXT    NOT NULL,
    client_id  TEXT    NOT NULL REFERENCES oauth_clients (client_id) ON DELETE CASCADE,
    expires_at TEXT    NOT NULL,
    active     INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
    request    TEXT    NOT NULL
) STRICT;

CREATE INDEX oauth_refresh_tokens_request ON oauth_refresh_tokens (request_id);
CREATE INDEX oauth_refresh_tokens_client ON oauth_refresh_tokens (client_id);
CREATE INDEX oauth_refresh_tokens_expires ON oauth_refresh_tokens (expires_at);

CREATE TABLE oauth_pkce_sessions (
    signature  TEXT NOT NULL PRIMARY KEY, -- the authorization code's signature
    request_id TEXT NOT NULL,
    client_id  TEXT NOT NULL REFERENCES oauth_clients (client_id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    request    TEXT NOT NULL
) STRICT;

CREATE INDEX oauth_pkce_sessions_client ON oauth_pkce_sessions (client_id);
CREATE INDEX oauth_pkce_sessions_expires ON oauth_pkce_sessions (expires_at);

-- user '*' applies to every user (revoke --all).
CREATE TABLE token_generations (
    user       TEXT    NOT NULL PRIMARY KEY CHECK (user <> ''),
    generation INTEGER NOT NULL
) STRICT;

CREATE TABLE oauth_hmac_secret (
    id     INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    secret BLOB    NOT NULL CHECK (length(secret) >= 32)
) STRICT;
