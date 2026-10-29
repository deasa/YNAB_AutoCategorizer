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

	mu      sync.Mutex
	pending map[string]pendingLogin // keyed by the state sent to Google
}

type pendingLogin struct {
	ar           fosite.AuthorizeRequester
	pkceVerifier string
	nonce        string
	created      time.Time
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
		EnablePKCEPlainChallengeMethod: false,
		RefreshTokenScopes:             []string{}, // always issue a refresh token
		ScopeStrategy:                  fosite.ExactScopeStrategy,
		TokenURL:                       issuer + "/oauth/token",
	}
	store := &clientStore{MemoryStore: storage.NewMemoryStore(), clients: map[string]fosite.Client{}}
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
		pending: map[string]pendingLogin{},
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
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("POST /oauth/revoke", s.handleRevoke)
}

func (s *AuthServer) ResourceMetadataURL() string {
	return s.issuer + "/.well-known/oauth-protected-resource"
}

func (s *AuthServer) handleASMetadata(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, &oauthex.AuthServerMeta{
		Issuer:                                 s.issuer,
		AuthorizationEndpoint:                  s.issuer + "/oauth/authorize",
		TokenEndpoint:                          s.issuer + "/oauth/token",
		RegistrationEndpoint:                   s.issuer + "/oauth/register",
		RevocationEndpoint:                     s.issuer + "/oauth/revoke",
		ScopesSupported:                        []string{scopeMCP, scopeOffline},
		ResponseTypesSupported:                 []string{"code"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:      []string{"none"},
		RevocationEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:          []string{"S256"},
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
		if err := validRedirectURI(u); err != nil {
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
	if err := s.store.add(client); err != nil {
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

// validRedirectURI allows https, or http on a loopback host (native clients).
func validRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Fragment != "" {
		return fmt.Errorf("redirect_uri %q is not an absolute URI without fragment", raw)
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
	state, verifier, nonce := rand.Text(), oauth2.GenerateVerifier(), rand.Text()
	s.mu.Lock()
	for k, p := range s.pending {
		if time.Since(p.created) > pendingLoginTTL {
			delete(s.pending, k)
		}
	}
	s.pending[state] = pendingLogin{ar: ar, pkceVerifier: verifier, nonce: nonce, created: time.Now()}
	s.mu.Unlock()
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
	resp, err := s.completeLogin(ctx, p.ar, id)
	if err != nil {
		s.provider.WriteAuthorizeError(ctx, w, p.ar, err)
		return
	}
	s.provider.WriteAuthorizeResponse(ctx, w, p.ar, resp)
}

// completeLogin maps a verified Google identity to a finbot session, or
// rejects it with access_denied.
func (s *AuthServer) completeLogin(ctx context.Context, ar fosite.AuthorizeRequester, id Identity) (fosite.AuthorizeResponder, error) {
	uid := UserID(id.Email)
	if err := s.allow.Check(id.Email, id.EmailVerified); err != nil {
		log.Printf("login rejected for %s: %v", uid, err)
		return nil, fosite.ErrAccessDenied.WithHint(err.Error())
	}
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
	issued, _ := ds.Extra[genKey].(int)
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
	_, ar, err := s.provider.IntrospectToken(ctx, token, fosite.AccessToken, &fosite.DefaultSession{})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, fosite.ErrorToRFC6749Error(err).ErrorField)
	}
	sess := ar.GetSession().(*fosite.DefaultSession)
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

func (c *clientStore) add(cl fosite.Client) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.clients) >= maxClients {
		return errors.New("too many registered clients")
	}
	c.clients[cl.GetID()] = cl
	return nil
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
