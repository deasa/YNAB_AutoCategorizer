package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/storage"
	"golang.org/x/oauth2"
)

const (
	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
	pendingLoginTTL = 10 * time.Minute
	maxClients      = 500
	maxPending      = 100 // parked logins + consents awaiting the user

	scopeMCP     = "mcp"
	scopeOffline = "offline_access"
	genKey       = "gen"
)

// AuthServer is finbot's minimal OAuth 2.1 authorization server. fosite does
// all token issuing, PKCE, refresh rotation, introspection and revocation;
// this file only wires endpoints, DCR bookkeeping and the Google hand-off.
type AuthServer struct {
	issuer   string // public base URL, e.g. https://finance.example.com
	provider fosite.OAuth2Provider
	store    *clientStore
	idp      IdentityProvider
	allow    Allowlist
	gens     *Generations

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
// client on finbot's consent page.
type pendingConsent struct {
	ar      fosite.AuthorizeRequester
	id      Identity
	csrf    string
	created time.Time
}

func NewAuthServer(issuer string, idp IdentityProvider, allow Allowlist, gens *Generations) (*AuthServer, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	cfg := &fosite.Config{
		AccessTokenLifespan:            accessTokenTTL,
		RefreshTokenLifespan:           refreshTokenTTL,
		AuthorizeCodeLifespan:          5 * time.Minute,
		GlobalSecret:                   secret, // in memory: a restart invalidates every token
		EnforcePKCE:                    true,
		DisableRefreshTokenValidation:  true, // introspection must never accept a refresh token as a bearer
		EnablePKCEPlainChallengeMethod: false,
		RefreshTokenScopes:             []string{}, // always issue a refresh token
		ScopeStrategy:                  fosite.ExactScopeStrategy,
		TokenURL:                       issuer + "/oauth/token",
	}
	store := &clientStore{MemoryStore: storage.NewMemoryStore(), clients: map[string]fosite.Client{}, names: map[string]string{}}
	provider := compose.Compose(cfg, store,
		&compose.CommonStrategy{CoreStrategy: compose.NewOAuth2HMACStrategy(cfg)},
		compose.OAuth2AuthorizeExplicitFactory,
		compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2PKCEFactory,
		compose.OAuth2TokenIntrospectionFactory,
		compose.OAuth2TokenRevocationFactory,
	)
	return &AuthServer{
		issuer: issuer, provider: provider, store: store, idp: idp, allow: allow, gens: gens,
		pending: map[string]pendingLogin{}, consents: map[string]pendingConsent{},
	}, nil
}

// Register mounts the AS and protected-resource endpoints on mux.
func (s *AuthServer) Register(mux *http.ServeMux, mcpPath string) {
	prm := &oauthex.ProtectedResourceMetadata{
		Resource:               s.issuer + mcpPath,
		AuthorizationServers:   []string{s.issuer},
		ScopesSupported:        []string{scopeMCP},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "finbot (spike)",
	}
	prmHandler := auth.ProtectedResourceMetadataHandler(prm)
	mux.Handle("/.well-known/oauth-protected-resource", prmHandler)
	mux.Handle("/.well-known/oauth-protected-resource"+mcpPath, prmHandler)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleASMetadata)
	mux.HandleFunc("POST /oauth/register", s.handleRegister)
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("GET /oauth/google/callback", s.handleGoogleCallback)
	mux.HandleFunc("POST /oauth/consent", s.handleConsent)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("POST /oauth/revoke", s.handleRevoke)
}

func (s *AuthServer) ResourceMetadataURL() string {
	return s.issuer + "/.well-known/oauth-protected-resource"
}

func (s *AuthServer) handleASMetadata(w http.ResponseWriter, r *http.Request) {
	// RFC 8414. A map rather than oauthex.AuthServerMeta, which would emit an
	// empty jwks_uri (we sign nothing, so there is no JWKS).
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                     s.issuer,
		"authorization_endpoint":                     s.issuer + "/oauth/authorize",
		"token_endpoint":                             s.issuer + "/oauth/token",
		"registration_endpoint":                      s.issuer + "/oauth/register",
		"revocation_endpoint":                        s.issuer + "/oauth/revoke",
		"scopes_supported":                           []string{scopeMCP, scopeOffline},
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
func (s *AuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		registrationError(w, "invalid_client_metadata", "body must be JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		registrationError(w, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := s.validRedirectURI(u); err != nil {
			registrationError(w, "invalid_redirect_uri", err.Error())
			return
		}
	}
	id := rand.Text()
	client := &fosite.DefaultClient{
		ID:            id,
		RedirectURIs:  req.RedirectURIs,
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},
		Scopes:        []string{scopeMCP, scopeOffline},
		Public:        true,
	}
	if err := s.store.add(client, req.ClientName); err != nil {
		registrationError(w, "invalid_client_metadata", err.Error())
		return
	}
	log.Printf("dcr: registered client %s (%q)", id, req.ClientName)
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                req.ClientName,
		"redirect_uris":              req.RedirectURIs,
		"grant_types":                client.GrantTypes,
		"response_types":             client.ResponseTypes,
		"token_endpoint_auth_method": "none",
		"scope":                      strings.Join(client.Scopes, " "),
	})
}

// validRedirectURI allows https, or http on a loopback host (native clients),
// with a real host, no userinfo, and never one of finbot's own OAuth endpoints.
func (s *AuthServer) validRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Fragment != "" || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return fmt.Errorf("redirect_uri %q must be an absolute URI with a host and no userinfo or fragment", raw)
	}
	if strings.HasPrefix(strings.ToLower(raw), strings.ToLower(s.issuer)+"/oauth/") {
		return fmt.Errorf("redirect_uri %q points at this server's OAuth endpoints", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("redirect_uri %q must be https or http on loopback", raw)
}

// handleAuthorize validates the client's request with fosite, parks it, and
// sends the browser to Google.
func (s *AuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar, err := s.provider.NewAuthorizeRequest(ctx, r)
	if err != nil {
		s.provider.WriteAuthorizeError(ctx, w, ar, err)
		return
	}
	// fosite checks PKCE only when issuing the code; check it before parking
	// anything or sending the user to Google.
	form := ar.GetRequestForm()
	if form.Get("code_challenge") == "" || form.Get("code_challenge_method") != "S256" {
		s.provider.WriteAuthorizeError(ctx, w, ar, fosite.ErrInvalidRequest.WithHint("PKCE with code_challenge_method=S256 is required."))
		return
	}
	// Every token is for /mcp, so always grant the mcp scope.
	if !ar.GetRequestedScopes().Has(scopeMCP) {
		ar.(*fosite.AuthorizeRequest).AppendRequestedScope(scopeMCP)
	}
	state, verifier, nonce := rand.Text(), oauth2.GenerateVerifier(), rand.Text()
	if !s.park(func() {
		s.pending[state] = pendingLogin{ar: ar, pkceVerifier: verifier, nonce: nonce, created: time.Now()}
	}) {
		http.Error(w, "too many sign-ins in progress; try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, s.idp.AuthCodeURL(state, verifier, nonce), http.StatusFound)
}

// handleGoogleCallback finishes the Google login, applies the allowlist, and
// completes the parked authorization request with a finbot-issued code.
func (s *AuthServer) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	s.mu.Lock()
	p, ok := s.pending[q.Get("state")]
	delete(s.pending, q.Get("state"))
	s.mu.Unlock()
	if !ok || time.Since(p.created) > pendingLoginTTL {
		http.Error(w, "unknown or expired sign-in; start again from your MCP client", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		s.provider.WriteAuthorizeError(ctx, w, p.ar, fosite.ErrAccessDenied.WithHintf("Google sign-in failed: %s", e))
		return
	}
	id, err := s.idp.Exchange(ctx, q.Get("code"), p.pkceVerifier, p.nonce)
	if err != nil {
		log.Printf("google exchange failed: %v", err)
		s.provider.WriteAuthorizeError(ctx, w, p.ar, fosite.ErrAccessDenied.WithHint("Google sign-in could not be verified."))
		return
	}
	uid := UserID(id.Email)
	if err := s.allow.Check(id.Email, id.EmailVerified); err != nil {
		log.Printf("login rejected for %s: %v", uid, err)
		s.provider.WriteAuthorizeError(ctx, w, p.ar, fosite.ErrAccessDenied.WithHint(err.Error()))
		return
	}
	consentID, csrf := rand.Text(), rand.Text()
	if !s.park(func() { s.consents[consentID] = pendingConsent{ar: p.ar, id: id, csrf: csrf, created: time.Now()} }) {
		http.Error(w, "too many sign-ins in progress; try again in a few minutes", http.StatusServiceUnavailable)
		return
	}
	s.renderConsent(w, p.ar, consentID, csrf, id)
}

// park stores a pending login or consent unless the server is at capacity,
// dropping expired entries first.
func (s *AuthServer) park(store func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.pending {
		if time.Since(p.created) > pendingLoginTTL {
			delete(s.pending, k)
		}
	}
	for k, c := range s.consents {
		if time.Since(c.created) > pendingLoginTTL {
			delete(s.consents, k)
		}
	}
	if len(s.pending)+len(s.consents) >= maxPending {
		return false
	}
	store()
	return true
}

// handleConsent is the CSRF-protected POST from the consent page. The form's
// csrf value must match both the parked consent and the SameSite=Strict
// cookie set when the page was rendered.
func (s *AuthServer) handleConsent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PostFormValue("consent_id")
	s.mu.Lock()
	c, ok := s.consents[id]
	delete(s.consents, id)
	s.mu.Unlock()
	cookie, err := r.Cookie(consentCookie)
	if !ok || time.Since(c.created) > pendingLoginTTL || err != nil ||
		!constantEq(r.PostFormValue("csrf"), c.csrf) || !constantEq(cookie.Value, c.csrf) {
		http.Error(w, "invalid or expired consent; start again from your MCP client", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: consentCookie, Path: "/oauth/consent", MaxAge: -1})
	if r.PostFormValue("decision") != "approve" {
		s.provider.WriteAuthorizeError(ctx, w, c.ar, fosite.ErrAccessDenied.WithHint("You declined access."))
		return
	}
	resp, err := s.issueCode(ctx, c.ar, c.id)
	if err != nil {
		s.provider.WriteAuthorizeError(ctx, w, c.ar, err)
		return
	}
	s.provider.WriteAuthorizeResponse(ctx, w, c.ar, resp)
}

// issueCode maps an allowlisted, consented Google identity to a finbot
// session and authorization code.
func (s *AuthServer) issueCode(ctx context.Context, ar fosite.AuthorizeRequester, id Identity) (fosite.AuthorizeResponder, error) {
	uid := UserID(id.Email)
	gen, err := s.gens.Current(uid)
	if err != nil {
		return nil, fosite.ErrServerError.WithWrap(err)
	}
	for _, sc := range ar.GetRequestedScopes() {
		ar.GrantScope(sc)
	}
	log.Printf("login ok for %s (gen %d)", uid, gen)
	return s.provider.NewAuthorizeResponse(ctx, ar, newSession(uid, id.Name, gen))
}

func newSession(uid, name string, gen int) *fosite.DefaultSession {
	return &fosite.DefaultSession{Subject: uid, Username: name, Extra: map[string]any{genKey: gen}}
}

// handleToken serves the authorization_code and refresh_token grants. A
// refresh by a revoked user fails with invalid_grant.
func (s *AuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ar, err := s.provider.NewAccessRequest(ctx, r, &fosite.DefaultSession{})
	if err != nil {
		s.provider.WriteAccessError(ctx, w, ar, err)
		return
	}
	if err := s.checkGeneration(ar.GetSession()); err != nil {
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

// handleRevoke is RFC 7009 single-token revocation (used by clients).
func (s *AuthServer) handleRevoke(w http.ResponseWriter, r *http.Request) {
	err := s.provider.NewRevocationRequest(r.Context(), r)
	s.provider.WriteRevocationResponse(r.Context(), w, err)
}

var errRevoked = errors.New("token revoked")

// checkGeneration rejects sessions issued before the user's last revoke.
func (s *AuthServer) checkGeneration(sess fosite.Session) error {
	ds, ok := sess.(*fosite.DefaultSession)
	if !ok {
		return errRevoked
	}
	if ds.Subject == "" {
		return errRevoked
	}
	var issued int
	switch v := ds.Extra[genKey].(type) {
	case int:
		issued = v
	case float64: // after a JSON round trip (persistent storage)
		if v != float64(int(v)) {
			return errRevoked
		}
		issued = int(v)
	default: // missing or non-numeric: fail closed
		return errRevoked
	}
	cur, err := s.gens.Current(ds.Subject)
	if err != nil {
		return err
	}
	if issued != cur {
		return errRevoked
	}
	return nil
}

// VerifyToken is the go-sdk auth.TokenVerifier for /mcp.
func (s *AuthServer) VerifyToken(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	tu, ar, err := s.provider.IntrospectToken(ctx, token, fosite.AccessToken, &fosite.DefaultSession{})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, fosite.ErrorToRFC6749Error(err).ErrorField)
	}
	if tu != fosite.AccessToken {
		return nil, fmt.Errorf("%w: not an access token", auth.ErrInvalidToken)
	}
	sess, ok := ar.GetSession().(*fosite.DefaultSession)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected session", auth.ErrInvalidToken)
	}
	if err := s.checkGeneration(sess); err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
	}
	return &auth.TokenInfo{
		Scopes:     ar.GetGrantedScopes(),
		Expiration: sess.GetExpiresAt(fosite.AccessToken),
		UserID:     sess.Subject,
		Extra:      map[string]any{"name": sess.Username},
	}, nil
}

// clientStore adds concurrency-safe dynamic client registration on top of
// fosite's in-memory token storage.
type clientStore struct {
	*storage.MemoryStore
	mu      sync.RWMutex
	clients map[string]fosite.Client
	names   map[string]string // client_name from DCR, for the consent page
}

func (c *clientStore) GetClient(_ context.Context, id string) (fosite.Client, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cl, ok := c.clients[id]
	if !ok {
		return nil, fosite.ErrNotFound
	}
	return cl, nil
}

func (c *clientStore) add(cl fosite.Client, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.clients) >= maxClients {
		return errors.New("too many registered clients")
	}
	c.clients[cl.GetID()] = cl
	c.names[cl.GetID()] = name
	return nil
}

func (c *clientStore) name(id string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.names[id]
}

func registrationError(w http.ResponseWriter, code, desc string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
