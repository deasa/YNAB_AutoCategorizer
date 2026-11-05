package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"golang.org/x/oauth2"
)

const (
	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
	authCodeTTL     = 5 * time.Minute
	pendingLoginTTL = 10 * time.Minute

	// MaxPending caps parked logins plus consents awaiting the user.
	MaxPending = 100

	// maxClientNameRunes keeps a registered name from pushing the redirect host
	// off the consent page.
	maxClientNameRunes = 100

	// ScopeMCP is the scope every access token for the MCP endpoint carries.
	ScopeMCP     = "mcp"
	scopeOffline = "offline_access"
)

// Config is what NewServer needs.
type Config struct {
	Issuer    string // public base URL, e.g. https://finance.example.com
	MCPPath   string // path of the protected resource, e.g. /mcp
	Storage   *Storage
	IDP       IdentityProvider
	Allowlist Allowlist
	Logger    *slog.Logger // nil uses slog.Default()
}

// Server is finbot's minimal OAuth 2.1 authorization server. It is an
// http.Handler for the AS and protected-resource endpoints, and its VerifyToken
// is the go-sdk token verifier for the MCP endpoint. fosite does all token
// issuing, PKCE, refresh rotation, introspection and revocation; this file only
// wires endpoints, DCR bookkeeping and the Google hand-off.
type Server struct {
	issuer   string
	mcpPath  string
	provider fosite.OAuth2Provider
	store    *Storage
	idp      IdentityProvider
	allow    Allowlist
	log      *slog.Logger
	mux      *http.ServeMux
	now      func() time.Time

	mu       sync.Mutex
	pending  map[string]pendingLogin   // keyed by the state sent to Google
	consents map[string]pendingConsent // keyed by consent id, after Google
}

type pendingLogin struct {
	ar           fosite.AuthorizeRequester
	pkceVerifier string
	nonce        string
	created      time.Time
}

// pendingConsent is an allowlisted login waiting for the user to approve the
// client on finbot's consent page. Its session holds the user's generation at
// sign-in, so a revoke before the user approves still applies.
type pendingConsent struct {
	ar      fosite.AuthorizeRequester
	session *fosite.DefaultSession
	csrf    string
	created time.Time
}

// NewServer loads the persisted HMAC secret and builds the server.
func NewServer(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Storage == nil || cfg.IDP == nil {
		return nil, errors.New("auth: new server: storage and identity provider are required")
	}
	if err := validIssuer(cfg.Issuer); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(cfg.MCPPath, "/") {
		return nil, errors.New("auth: new server: MCP path must start with /")
	}
	secret, err := cfg.Storage.HMACSecret(ctx)
	if err != nil {
		return nil, err
	}
	fc := &fosite.Config{
		AccessTokenLifespan:            accessTokenTTL,
		RefreshTokenLifespan:           refreshTokenTTL,
		AuthorizeCodeLifespan:          authCodeTTL,
		GlobalSecret:                   secret,
		EnforcePKCE:                    true,
		DisableRefreshTokenValidation:  true, // introspection must never accept a refresh token as a bearer
		EnablePKCEPlainChallengeMethod: false,
		RefreshTokenScopes:             []string{}, // always issue a refresh token
		ScopeStrategy:                  fosite.ExactScopeStrategy,
		TokenURL:                       cfg.Issuer + "/oauth/token",
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		issuer: cfg.Issuer, mcpPath: cfg.MCPPath, store: cfg.Storage, idp: cfg.IDP, allow: cfg.Allowlist, log: logger,
		now: time.Now, pending: map[string]pendingLogin{}, consents: map[string]pendingConsent{},
		provider: compose.Compose(fc, cfg.Storage,
			&compose.CommonStrategy{CoreStrategy: compose.NewOAuth2HMACStrategy(fc)},
			compose.OAuth2AuthorizeExplicitFactory,
			compose.OAuth2RefreshTokenGrantFactory,
			compose.OAuth2PKCEFactory,
			compose.OAuth2TokenIntrospectionFactory,
			compose.OAuth2TokenRevocationFactory,
		),
	}
	s.routes()
	return s, nil
}

// validIssuer wants a bare https origin, or http on loopback for local runs.
func validIssuer(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("auth: new server: issuer must be a bare origin with no trailing slash")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return errors.New("auth: new server: issuer must be https, or http on loopback")
	}
	return nil
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func (s *Server) routes() {
	prm := sdkauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               s.issuer + s.mcpPath,
		AuthorizationServers:   []string{s.issuer},
		ScopesSupported:        []string{ScopeMCP},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "finbot",
	})
	mux := http.NewServeMux()
	mux.Handle("/.well-known/oauth-protected-resource", prm)
	mux.Handle("/.well-known/oauth-protected-resource"+s.mcpPath, prm)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleASMetadata)
	mux.HandleFunc("POST /oauth/register", s.handleRegister)
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("GET /oauth/google/callback", s.handleGoogleCallback)
	mux.HandleFunc("POST /oauth/consent", s.handleConsent)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("POST /oauth/revoke", s.handleRevoke)
	s.mux = mux
}

// ServeHTTP serves /oauth/* and /.well-known/oauth-*.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ResourceMetadataURL is the RFC 9728 URL for the MCP endpoint's WWW-Authenticate.
func (s *Server) ResourceMetadataURL() string {
	return s.issuer + "/.well-known/oauth-protected-resource"
}

// RevokeUser invalidates all of a user's access and refresh tokens at once, or
// everyone's for AllUsers. user is a subject from UserID. The next sign-in works.
func (s *Server) RevokeUser(ctx context.Context, user string) error {
	_, err := s.store.BumpGeneration(ctx, user)
	return err
}

func (s *Server) handleASMetadata(w http.ResponseWriter, _ *http.Request) {
	// RFC 8414. A map rather than oauthex.AuthServerMeta, which would emit an
	// empty jwks_uri (we sign nothing, so there is no JWKS).
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                     s.issuer,
		"authorization_endpoint":                     s.issuer + "/oauth/authorize",
		"token_endpoint":                             s.issuer + "/oauth/token",
		"registration_endpoint":                      s.issuer + "/oauth/register",
		"revocation_endpoint":                        s.issuer + "/oauth/revoke",
		"scopes_supported":                           []string{ScopeMCP, scopeOffline},
		"response_types_supported":                   []string{"code"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":      []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":           []string{"S256"},
	})
}

// handleRegister is RFC 7591 Dynamic Client Registration for public clients.
// A registered client gets nothing by itself: every authorization still needs
// an allowlisted Google sign-in.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		registrationError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		registrationError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := s.validRedirectURI(u); err != nil {
			registrationError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	name := clampName(req.ClientName)
	client := &fosite.DefaultClient{
		ID:            rand.Text(),
		RedirectURIs:  req.RedirectURIs,
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},
		Scopes:        []string{ScopeMCP, scopeOffline},
		Public:        true,
	}
	switch err := s.store.RegisterClient(r.Context(), client, name); {
	case errors.Is(err, ErrTooManyClients):
		s.log.Warn("dcr: client cap reached")
		registrationError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "too many registered clients")
		return
	case err != nil:
		s.log.Error("dcr: register client", "err", err)
		registrationError(w, http.StatusInternalServerError, "server_error", "could not register the client")
		return
	}
	s.log.Info("dcr: registered client", "name", name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        s.now().Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                client.GrantTypes,
		"response_types":             client.ResponseTypes,
		"token_endpoint_auth_method": "none",
		"scope":                      strings.Join(client.Scopes, " "),
	})
}

func clampName(name string) string {
	name = strings.ToValidUTF8(name, "")
	if utf8.RuneCountInString(name) <= maxClientNameRunes {
		return name
	}
	return string([]rune(name)[:maxClientNameRunes])
}

// validRedirectURI allows https, or http on a loopback host (native clients),
// with a real host, no userinfo, and never one of finbot's own OAuth endpoints.
func (s *Server) validRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Fragment != "" || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return fmt.Errorf("redirect_uri %q must be an absolute URI with a host and no userinfo or fragment", raw)
	}
	if s.isOwnOAuthEndpoint(u) {
		return fmt.Errorf("redirect_uri %q points at this server's OAuth endpoints", raw)
	}
	if u.Scheme == "https" || (u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil
	}
	return fmt.Errorf("redirect_uri %q must be https or http on loopback", raw)
}

// isOwnOAuthEndpoint reports whether u targets this server's /oauth/ tree,
// comparing the parsed host and the cleaned path (so //oauth and /./oauth are
// caught too).
func (s *Server) isOwnOAuthEndpoint(u *url.URL) bool {
	iss, err := url.Parse(s.issuer)
	if err != nil || !strings.EqualFold(u.Hostname(), iss.Hostname()) {
		return false
	}
	return strings.HasPrefix(path.Clean("/"+u.Path)+"/", "/oauth/")
}

// authorizeError answers on finbot's own origin. Until the user approves on the
// consent page, a DCR-registered redirect_uri is untrusted, so errors (and Deny)
// never redirect there.
func authorizeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, msg+"\nClose this window and start again from your MCP client.", status)
}

// handleAuthorize validates the client's request with fosite, parks it, and
// sends the browser to Google.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	ar, err := s.provider.NewAuthorizeRequest(r.Context(), r)
	if err != nil {
		e := fosite.ErrorToRFC6749Error(err)
		authorizeError(w, http.StatusBadRequest, "Invalid authorization request: "+e.ErrorField+": "+e.GetDescription())
		return
	}
	// fosite checks PKCE only when issuing the code; check it before parking
	// anything or sending the user to Google.
	form := ar.GetRequestForm()
	if form.Get("code_challenge") == "" || form.Get("code_challenge_method") != "S256" {
		authorizeError(w, http.StatusBadRequest, "Invalid authorization request: PKCE with code_challenge_method=S256 is required.")
		return
	}
	// Every token is for the MCP endpoint, so always grant the mcp scope.
	ar.SetRequestedScopes(withMCPScope(ar.GetRequestedScopes()))
	state, verifier, nonce := rand.Text(), oauth2.GenerateVerifier(), rand.Text()
	if !s.park(func() {
		s.pending[state] = pendingLogin{ar: ar, pkceVerifier: verifier, nonce: nonce, created: s.now()}
	}) {
		http.Error(w, "too many sign-ins in progress; try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, s.idp.AuthCodeURL(state, verifier, nonce), http.StatusFound)
}

func withMCPScope(scopes fosite.Arguments) fosite.Arguments {
	if scopes.Has(ScopeMCP) {
		return scopes
	}
	return append(scopes, ScopeMCP)
}

// handleGoogleCallback finishes the Google login, applies the allowlist, and
// shows the consent page for the parked authorization request.
func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	s.mu.Lock()
	p, ok := s.pending[q.Get("state")]
	delete(s.pending, q.Get("state"))
	s.mu.Unlock()
	if !ok || s.expired(p.created) {
		http.Error(w, "unknown or expired sign-in; start again from your MCP client", http.StatusBadRequest)
		return
	}
	if q.Get("error") != "" {
		authorizeError(w, http.StatusForbidden, "Google sign-in failed.")
		return
	}
	id, err := s.idp.Exchange(ctx, q.Get("code"), p.pkceVerifier, p.nonce)
	if err != nil {
		s.log.Warn("google exchange failed", "err", err)
		authorizeError(w, http.StatusForbidden, "Google sign-in could not be verified.")
		return
	}
	uid := UserID(id.Email)
	if err := s.allow.Check(id.Email, id.EmailVerified); err != nil {
		s.log.Warn("login rejected", "user", uid, "reason", err)
		authorizeError(w, http.StatusForbidden, "Access denied: "+err.Error()+".")
		return
	}
	// The client may have been evicted since the request was parked.
	name, err := s.store.ClientName(ctx, p.ar.GetClient().GetID())
	if errors.Is(err, fosite.ErrNotFound) {
		authorizeError(w, http.StatusBadRequest, "This app's registration has expired.")
		return
	}
	gen, genErr := s.store.CurrentGeneration(ctx, uid)
	if err != nil || genErr != nil {
		s.log.Error("prepare consent", "err", errors.Join(err, genErr))
		authorizeError(w, http.StatusInternalServerError, "Could not complete sign-in.")
		return
	}
	consentID, csrf := rand.Text(), rand.Text()
	sess := NewSession(uid, id.Name, gen)
	if !s.park(func() { s.consents[consentID] = pendingConsent{ar: p.ar, session: sess, csrf: csrf, created: s.now()} }) {
		http.Error(w, "too many sign-ins in progress; try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	user := id.Name
	if user == "" {
		user = uid
	}
	s.renderConsent(w, p.ar.GetRedirectURI(), name, consentID, csrf, user)
}

func (s *Server) expired(created time.Time) bool { return s.now().Sub(created) > pendingLoginTTL }

// park stores a pending login or consent unless the server is at capacity,
// dropping expired entries first.
func (s *Server) park(store func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.pending {
		if s.expired(p.created) {
			delete(s.pending, k)
		}
	}
	for k, c := range s.consents {
		if s.expired(c.created) {
			delete(s.consents, k)
		}
	}
	if len(s.pending)+len(s.consents) >= MaxPending {
		return false
	}
	store()
	return true
}

// handleConsent is the CSRF-protected POST from the consent page. The form's
// csrf value must match both the parked consent and the SameSite=Strict cookie
// set when the page was rendered. A consent is single use, whatever the outcome.
func (s *Server) handleConsent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PostFormValue("consent_id")
	s.mu.Lock()
	c, ok := s.consents[id]
	delete(s.consents, id)
	s.mu.Unlock()
	cookie, err := r.Cookie(consentCookie)
	if !ok || s.expired(c.created) || err != nil ||
		!constantEq(r.PostFormValue("csrf"), c.csrf) || !constantEq(cookie.Value, c.csrf) {
		http.Error(w, "invalid or expired consent; start again from your MCP client", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: consentCookie, Path: "/oauth/consent", MaxAge: -1})
	if r.PostFormValue("decision") != "approve" {
		authorizeError(w, http.StatusForbidden, "You declined access. Nothing was sent to the app.")
		return
	}
	switch err := s.store.CheckSession(ctx, c.session); {
	case errors.Is(err, ErrRevoked):
		authorizeError(w, http.StatusForbidden, "Access was revoked during this sign-in.")
		return
	case err != nil:
		s.log.Error("check session", "err", err)
		authorizeError(w, http.StatusInternalServerError, "Could not complete sign-in.")
		return
	}
	for _, sc := range c.ar.GetRequestedScopes() {
		c.ar.GrantScope(sc)
	}
	resp, err := s.provider.NewAuthorizeResponse(ctx, c.ar, c.session)
	if err != nil {
		s.log.Error("issue code", "err", fosite.ErrorToRFC6749Error(err).ErrorField)
		authorizeError(w, http.StatusInternalServerError, "Could not complete sign-in.")
		return
	}
	s.log.Info("login ok", "user", c.session.Subject)
	s.provider.WriteAuthorizeResponse(ctx, w, c.ar, resp)
}

// handleToken serves the authorization_code and refresh_token grants. Storage
// already treats a revoked user's code or refresh token as not found, which
// ends in invalid_grant; the check here is a second layer.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar, err := s.provider.NewAccessRequest(ctx, r, &fosite.DefaultSession{})
	if err != nil {
		s.provider.WriteAccessError(ctx, w, ar, err)
		return
	}
	if err := s.checkRequester(ctx, ar); err != nil {
		s.provider.WriteAccessError(ctx, w, ar, fosite.ErrInvalidGrant.WithHint("Access was revoked; sign in again."))
		return
	}
	resp, err := s.provider.NewAccessResponse(ctx, ar)
	if err != nil {
		s.provider.WriteAccessError(ctx, w, ar, err)
		return
	}
	s.provider.WriteAccessResponse(ctx, w, ar, resp)
}

// handleRevoke is RFC 7009 single-token revocation, used by clients.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	err := s.provider.NewRevocationRequest(r.Context(), r)
	s.provider.WriteRevocationResponse(r.Context(), w, err)
}

func (s *Server) checkRequester(ctx context.Context, ar fosite.Requester) error {
	sess, ok := ar.GetSession().(*fosite.DefaultSession)
	if !ok {
		return ErrRevoked
	}
	return s.store.CheckSession(ctx, sess)
}

// VerifyToken is the go-sdk auth.TokenVerifier for the MCP endpoint. Only a live
// access token passes; refresh tokens never do. TokenInfo.UserID is the subject,
// and Extra["name"] is the display name for attribution (N9). Requiring ScopeMCP
// is the resource server's job, through RequireBearerTokenOptions.Scopes.
func (s *Server) VerifyToken(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
	tu, ar, err := s.provider.IntrospectToken(ctx, token, fosite.AccessToken, &fosite.DefaultSession{})
	if err != nil {
		return nil, fmt.Errorf("%w: %s", sdkauth.ErrInvalidToken, fosite.ErrorToRFC6749Error(err).ErrorField)
	}
	if tu != fosite.AccessToken {
		return nil, fmt.Errorf("%w: not an access token", sdkauth.ErrInvalidToken)
	}
	sess, ok := ar.GetSession().(*fosite.DefaultSession)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected session", sdkauth.ErrInvalidToken)
	}
	if err := s.store.CheckSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("%w: revoked or unreadable session", sdkauth.ErrInvalidToken)
	}
	return &sdkauth.TokenInfo{
		Scopes:     ar.GetGrantedScopes(),
		Expiration: sess.GetExpiresAt(fosite.AccessToken),
		UserID:     sess.Subject,
		Extra:      map[string]any{"name": sess.Username},
	}, nil
}

func registrationError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
