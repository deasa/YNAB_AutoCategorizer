package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	asRedirect = "http://127.0.0.1:9999/callback"
	asMCPPath  = "/mcp"
)

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// syncBuffer is the log sink; handlers may log while a test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// asEnv is the authorization server on a temp database, served over HTTP with a
// small MCP endpoint, signing in through a fake OIDC provider.
type asEnv struct {
	t       *testing.T
	path    string
	allowed string
	db      *store.DB
	as      *Server
	idp     *fakeOIDC
	srv     *httptest.Server
	logs    *syncBuffer
	handler atomic.Pointer[http.Handler]
}

func newASEnv(t *testing.T, allowed string) *asEnv {
	t.Helper()
	e := &asEnv{t: t, path: filepath.Join(t.TempDir(), "finbot.db"), allowed: allowed, idp: newFakeOIDC(t), logs: &syncBuffer{}}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*e.handler.Load()).ServeHTTP(w, r) }))
	t.Cleanup(e.srv.Close)
	e.start()
	t.Cleanup(func() { e.db.Close() })
	return e
}

// start opens the database and builds a brand-new Server on it.
func (e *asEnv) start() {
	e.t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	idp, err := newOIDCProvider(ctx, e.idp.srv.URL, fakeClientID, fakeClientSecret, e.srv.URL+"/oauth/google/callback")
	if err != nil {
		e.t.Fatal(err)
	}
	as, err := NewServer(ctx, Config{
		Issuer: e.srv.URL, MCPPath: asMCPPath, Storage: New(db), IDP: idp, Allowlist: ParseAllowlist(e.allowed),
		Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.db, e.as = db, as
	mux := http.NewServeMux()
	mux.Handle("/oauth/", as)
	mux.Handle("/.well-known/", as)
	mux.Handle(asMCPPath, testMCPHandler(as.VerifyToken, as.ResourceMetadataURL()))
	var h http.Handler = mux
	e.handler.Store(&h)
}

// restart drops the handler and all in-memory state, and closes the database.
func (e *asEnv) restart() {
	e.t.Helper()
	if err := e.db.Close(); err != nil {
		e.t.Fatal(err)
	}
	e.start()
}

type helloArgs struct{}

type helloResult struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
}

func hello(_ context.Context, req *mcp.CallToolRequest, _ helloArgs) (*mcp.CallToolResult, helloResult, error) {
	ti := req.Extra.TokenInfo
	if ti == nil {
		return nil, helloResult{}, errors.New("no auth context")
	}
	name, _ := ti.Extra["name"].(string)
	return nil, helloResult{UserID: ti.UserID, DisplayName: name}, nil
}

func testMCPHandler(verify sdkauth.TokenVerifier, metadataURL string) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "finbot-test", Version: "0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "hello", Description: "who am I"}, hello)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	return sdkauth.RequireBearerToken(verify, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL, Scopes: []string{ScopeMCP},
	})(h)
}

func (e *asEnv) register(name string, redirectURIs ...string) (int, string) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"redirect_uris": redirectURIs, "client_name": name})
	resp, err := http.Post(e.srv.URL+"/oauth/register", "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	return resp.StatusCode, reg.ClientID
}

func (e *asEnv) mustRegister() string {
	e.t.Helper()
	status, id := e.register("Test <App>", asRedirect)
	if status != http.StatusCreated || id == "" {
		e.t.Fatalf("register: status %d", status)
	}
	return id
}

// startAuthorize sends GET /oauth/authorize, not followed. mutate may change the query.
func (e *asEnv) startAuthorize(clientID string, mutate func(url.Values)) (*http.Response, string) {
	e.t.Helper()
	verifier := oauth2.GenerateVerifier()
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {asRedirect},
		"scope":                 {"mcp offline_access"},
		"state":                 {"client-state-123"},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {e.srv.URL + asMCPPath},
	}
	if mutate != nil {
		mutate(q)
	}
	resp, err := noFollow.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp, verifier
}

func (e *asEnv) get(u string) (*http.Response, string) {
	e.t.Helper()
	resp, err := noFollow.Get(u)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// callback runs authorize and the provider's redirect, stopping at the response
// from finbot's Google callback: the consent page or a rejection.
func (e *asEnv) callback(clientID string) (*http.Response, string, string) {
	e.t.Helper()
	resp, verifier := e.startAuthorize(clientID, nil)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), e.idp.srv.URL+"/auth") {
		e.t.Fatalf("authorize: status %d, want redirect to the provider", resp.StatusCode)
	}
	idp, _ := e.get(resp.Header.Get("Location"))
	if idp.StatusCode != http.StatusFound || !strings.HasPrefix(idp.Header.Get("Location"), e.srv.URL+"/oauth/google/callback") {
		e.t.Fatalf("provider: status %d, want redirect to finbot", idp.StatusCode)
	}
	page, body := e.get(idp.Header.Get("Location"))
	return page, body, verifier
}

var formValue = regexp.MustCompile(`name="(consent_id|csrf)" value="([^"]+)"`)

// submitConsent posts the consent form. cookie=false omits the CSRF cookie.
func (e *asEnv) submitConsent(page *http.Response, body string, fields map[string]string, cookie bool) *http.Response {
	e.t.Helper()
	form := url.Values{}
	for _, m := range formValue.FindAllStringSubmatch(body, -1) {
		form.Set(m[1], m[2])
	}
	for k, v := range fields {
		form.Set(k, v)
	}
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/oauth/consent", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie {
		for _, c := range page.Cookies() {
			req.AddCookie(c)
		}
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// assertLocalError checks an error answered on finbot's origin, never a redirect
// to the (unconsented) client redirect_uri.
func assertLocalError(t *testing.T, resp *http.Response, status int, body, want string) {
	t.Helper()
	if resp.StatusCode != status || resp.Header.Get("Location") != "" {
		t.Fatalf("got %d Location=%q, want local %d", resp.StatusCode, resp.Header.Get("Location"), status)
	}
	if !strings.Contains(body, want) {
		t.Errorf("body %q lacks %q", body, want)
	}
}

// authorize runs everything up to and including an approved consent and returns
// the client's code.
func (e *asEnv) authorize(clientID string) (code, verifier string) {
	e.t.Helper()
	page, body, verifier := e.callback(clientID)
	if page.StatusCode != http.StatusOK {
		e.t.Fatalf("callback: status %d: %s", page.StatusCode, body)
	}
	resp := e.submitConsent(page, body, map[string]string{"decision": "approve"}, true)
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("consent: status %d", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	if u.Query().Get("state") != "client-state-123" || u.Query().Get("code") == "" {
		e.t.Fatalf("bad client redirect: error=%q", u.Query().Get("error"))
	}
	return u.Query().Get("code"), verifier
}

func (e *asEnv) token(form url.Values) (int, map[string]any) {
	e.t.Helper()
	resp, err := http.PostForm(e.srv.URL+"/oauth/token", form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func (e *asEnv) exchangeCode(clientID, code, verifier string) (int, map[string]any) {
	e.t.Helper()
	return e.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {asRedirect},
		"client_id": {clientID}, "code_verifier": {verifier},
	})
}

func (e *asEnv) refresh(clientID, refresh string) (int, map[string]any) {
	e.t.Helper()
	return e.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
}

// login is a full sign-in through to tokens.
func (e *asEnv) login() (access, refresh, clientID string) {
	e.t.Helper()
	clientID = e.mustRegister()
	code, verifier := e.authorize(clientID)
	status, body := e.exchangeCode(clientID, code, verifier)
	if status != http.StatusOK {
		e.t.Fatalf("token: %d %v", status, body["error"])
	}
	access, _ = body["access_token"].(string)
	refresh, _ = body["refresh_token"].(string)
	if access == "" || refresh == "" {
		e.t.Fatal("token response missing tokens")
	}
	return access, refresh, clientID
}

func (e *asEnv) mcpStatus(token string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+asMCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

func (e *asEnv) wantMCPAccepted(token, what string) {
	e.t.Helper()
	if status, _ := e.mcpStatus(token); status == http.StatusUnauthorized || status == http.StatusForbidden {
		e.t.Errorf("%s: /mcp status %d, want accepted", what, status)
	}
}

func (e *asEnv) wantMCPUnauthorized(token, what string) {
	e.t.Helper()
	if status, _ := e.mcpStatus(token); status != http.StatusUnauthorized {
		e.t.Errorf("%s: /mcp status %d, want 401", what, status)
	}
}

func (e *asEnv) wantInvalidGrant(status int, body map[string]any, what string) {
	e.t.Helper()
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		e.t.Errorf("%s: %d %v, want 400 invalid_grant", what, status, body["error"])
	}
}

func (e *asEnv) revokeToken(clientID, token string) {
	e.t.Helper()
	resp, err := http.PostForm(e.srv.URL+"/oauth/revoke", url.Values{"token": {token}, "client_id": {clientID}})
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("revoke: status %d", resp.StatusCode)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// helloAs calls the MCP tool with a real client and returns its JSON result.
func (e *asEnv) helloAs(token string) string {
	e.t.Helper()
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: e.srv.URL + asMCPPath, HTTPClient: &http.Client{Transport: bearer{token}},
	}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "hello"})
	if err != nil || res.IsError {
		e.t.Fatalf("hello: %v %+v", err, res)
	}
	got, _ := json.Marshal(res.StructuredContent)
	return string(got)
}

// TestEndToEnd is the acceptance flow: DCR, authorize, fake Google, consent,
// code, token, refresh, revoke.
func TestEndToEnd(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	access, refresh, clientID := e.login()

	want := `{"display_name":"Alice","user_id":"` + UserID("alice@example.com") + `"}`
	if got := e.helloAs(access); got != want {
		t.Fatalf("hello = %s, want %s", got, want)
	}

	status, body := e.refresh(clientID, refresh)
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %v", status, body["error"])
	}
	access2, refresh2 := body["access_token"].(string), body["refresh_token"].(string)
	if access2 == access || refresh2 == refresh || access2 == "" || refresh2 == "" {
		t.Fatal("refresh did not rotate both tokens")
	}
	if got := e.helloAs(access2); got != want {
		t.Fatalf("hello after refresh = %s: identity changed across rotation", got)
	}
	st, b := e.refresh(clientID, refresh)
	e.wantInvalidGrant(st, b, "replayed refresh token")

	// A fresh grant, revoked by its client under RFC 7009.
	access3, refresh3, client3 := e.login()
	e.revokeToken(client3, refresh3)
	e.wantMCPUnauthorized(access3, "access token of a revoked grant")
	st, b = e.refresh(client3, refresh3)
	e.wantInvalidGrant(st, b, "revoked refresh token")
}

func TestRevokeUser(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	access, refresh, clientID := e.login()
	code, verifier := e.authorize(clientID) // an unredeemed code, too
	e.wantMCPAccepted(access, "before revoke")

	if err := e.as.RevokeUser(context.Background(), UserID("alice@example.com")); err != nil {
		t.Fatal(err)
	}
	e.wantMCPUnauthorized(access, "revoked access token")
	st, b := e.refresh(clientID, refresh)
	e.wantInvalidGrant(st, b, "revoked refresh token")
	st, b = e.exchangeCode(clientID, code, verifier)
	e.wantInvalidGrant(st, b, "code issued before the revoke")

	access2, refresh2, client2 := e.login()
	e.wantMCPAccepted(access2, "fresh sign-in after revoke")
	if st, b := e.refresh(client2, refresh2); st != http.StatusOK {
		t.Errorf("fresh refresh after revoke: %d %v", st, b["error"])
	}
}

func TestRevokeUserScope(t *testing.T) {
	e := newASEnv(t, "alice@example.com,bob@example.com")
	alice, _, _ := e.login()
	e.idp.signInAs(Identity{Email: "bob@example.com", EmailVerified: true, Name: "Bob"})
	bob, _, _ := e.login()
	ctx := context.Background()

	if err := e.as.RevokeUser(ctx, UserID("bob@example.com")); err != nil {
		t.Fatal(err)
	}
	e.wantMCPUnauthorized(bob, "bob after revoking bob")
	e.wantMCPAccepted(alice, "alice after revoking bob")

	if err := e.as.RevokeUser(ctx, AllUsers); err != nil {
		t.Fatal(err)
	}
	e.wantMCPUnauthorized(alice, "alice after revoke --all")
	if err := e.as.RevokeUser(ctx, ""); err == nil {
		t.Error("RevokeUser with no user succeeded")
	}
}

// A revoke while the consent page is open applies to that sign-in as well.
func TestRevokeDuringConsent(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	page, body, _ := e.callback(e.mustRegister())
	if err := e.as.RevokeUser(context.Background(), UserID("alice@example.com")); err != nil {
		t.Fatal(err)
	}
	resp := e.submitConsent(page, body, map[string]string{"decision": "approve"}, true)
	assertLocalError(t, resp, http.StatusForbidden, "", "")
}

func TestRestartKeepsTokensAndClient(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	access, refresh, clientID := e.login()

	e.restart()

	e.wantMCPAccepted(access, "access token after restart")
	status, body := e.refresh(clientID, refresh)
	if status != http.StatusOK {
		t.Fatalf("refresh after restart: %d %v", status, body["error"])
	}
	e.wantMCPAccepted(body["access_token"].(string), "refreshed access token after restart")
	// The DCR client still works for a new sign-in, without registering again.
	code, verifier := e.authorize(clientID)
	if st, b := e.exchangeCode(clientID, code, verifier); st != http.StatusOK {
		t.Fatalf("new sign-in with the old client after restart: %d %v", st, b["error"])
	}
	// A revoke survives a restart too.
	if err := e.as.RevokeUser(context.Background(), UserID("alice@example.com")); err != nil {
		t.Fatal(err)
	}
	e.restart()
	e.wantMCPUnauthorized(access, "revoked token after restart")
}

func TestUnauthenticatedMCPReturns401WithResourceMetadata(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	want := `Bearer resource_metadata="` + e.srv.URL + `/.well-known/oauth-protected-resource", scope="mcp"`
	for _, tok := range []string{"", "not-a-real-token"} {
		status, wwwAuth := e.mcpStatus(tok)
		if status != http.StatusUnauthorized || wwwAuth != want {
			t.Errorf("token %q: got %d %q, want 401 %q", tok, status, wwwAuth, want)
		}
	}
}

func TestDiscoveryMetadata(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	var prm, asm map[string]any
	for path, dst := range map[string]*map[string]any{
		"/.well-known/oauth-protected-resource":     &prm,
		"/.well-known/oauth-protected-resource/mcp": &prm,
		"/.well-known/oauth-authorization-server":   &asm,
	} {
		resp, err := http.Get(e.srv.URL + path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v %v", path, err, resp.Status)
		}
		json.NewDecoder(resp.Body).Decode(dst)
		resp.Body.Close()
	}
	if prm["resource"] != e.srv.URL+asMCPPath || prm["authorization_servers"].([]any)[0] != e.srv.URL || prm["resource_name"] != "finbot" {
		t.Errorf("bad protected resource metadata: %v", prm)
	}
	if asm["issuer"] != e.srv.URL || asm["registration_endpoint"] != e.srv.URL+"/oauth/register" {
		t.Errorf("bad AS metadata: %v", asm)
	}
	if m := asm["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v", m)
	}
}

func TestRegisterRedirectURIValidation(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	tests := map[string]int{
		"https://claude.ai/api/mcp/auth_callback":     http.StatusCreated,
		"http://localhost:3118/callback":              http.StatusCreated,
		"http://127.0.0.1:9999/callback":              http.StatusCreated,
		"http://evil.example.com/cb":                  http.StatusBadRequest,
		"https:///cb":                                 http.StatusBadRequest,
		"https:opaque":                                http.StatusBadRequest,
		"https://claude.ai@evil.example/cb":           http.StatusBadRequest,
		"https://claude.ai/cb#frag":                   http.StatusBadRequest,
		"/relative/cb":                                http.StatusBadRequest,
		e.srv.URL + "/oauth/token":                    http.StatusBadRequest,
		strings.ToUpper(e.srv.URL) + "/oauth/consent": http.StatusBadRequest,
		e.srv.URL + "//oauth/google/callback":         http.StatusBadRequest,
		e.srv.URL + "/./oauth/google/callback":        http.StatusBadRequest,
		e.srv.URL + "/x/../oauth/token":               http.StatusBadRequest,
		e.srv.URL + "/%6Fauth/token":                  http.StatusBadRequest,
		e.srv.URL + "/oauthx/cb":                      http.StatusCreated,
	}
	for uri, want := range tests {
		if got, _ := e.register("x", uri); got != want {
			t.Errorf("register %q: status %d, want %d", uri, got, want)
		}
	}
	if got, _ := e.register("x"); got != http.StatusBadRequest {
		t.Errorf("register with no redirect_uris: status %d, want 400", got)
	}
}

func TestRegisterClampsClientName(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	status, id := e.register(strings.Repeat("é", 5000), asRedirect)
	if status != http.StatusCreated {
		t.Fatalf("register: status %d", status)
	}
	name, err := e.as.store.ClientName(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if name != strings.Repeat("é", maxClientNameRunes) {
		t.Errorf("stored name has %d runes, want %d", len([]rune(name)), maxClientNameRunes)
	}
}

func TestRegisterAtCapRefused(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	ctx := context.Background()
	future := formatTime(time.Now().Add(time.Hour))
	for i := range MaxClients {
		id := "c" + string(rune('a'+i%26)) + rand.Text()
		if err := e.as.store.RegisterClient(ctx, testClient(id), "x"); err != nil {
			t.Fatal(err)
		}
		// A live grant makes the client ineligible for eviction.
		if _, err := e.db.Writer().ExecContext(ctx,
			`INSERT INTO oauth_access_tokens (signature, request_id, client_id, expires_at, request) VALUES (?, ?, ?, ?, '{}')`,
			id, id, id, future); err != nil {
			t.Fatal(err)
		}
	}
	status, id := e.register("late", asRedirect)
	if status != http.StatusServiceUnavailable || id != "" {
		t.Fatalf("register at cap: status %d, want 503", status)
	}
}

func TestRejectedSignIns(t *testing.T) {
	for name, tc := range map[string]struct {
		allowed string
		id      Identity
		want    string
	}{
		"not allowlisted": {"bob@example.com", Identity{Email: "alice@example.com", EmailVerified: true}, "not allowed"},
		"empty allowlist": {"", Identity{Email: "alice@example.com", EmailVerified: true}, "not allowed"},
		"unverified":      {"alice@example.com", Identity{Email: "alice@example.com"}, "not verified"},
		"no email":        {"alice@example.com", Identity{EmailVerified: true}, "not allowed"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newASEnv(t, tc.allowed)
			e.idp.signInAs(tc.id)
			resp, body, _ := e.callback(e.mustRegister())
			assertLocalError(t, resp, http.StatusForbidden, body, tc.want)
			e.as.mu.Lock()
			defer e.as.mu.Unlock()
			if len(e.as.consents) != 0 {
				t.Error("a rejected sign-in reached the consent stage")
			}
		})
	}
}

// Each of these would be accepted but for the verifier in GoogleProvider.
func TestBadIDTokensRejected(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	claim := func(k string, v any) func(*fakeOIDC) {
		return func(f *fakeOIDC) { f.tamper = func(c map[string]any) { c[k] = v } }
	}
	for name, setup := range map[string]func(*fakeOIDC){
		"wrong nonce":     claim("nonce", "other"),
		"wrong audience":  claim("aud", "other-client"),
		"wrong issuer":    claim("iss", "https://evil.example"),
		"expired":         claim("exp", time.Now().Add(-time.Hour).Unix()),
		"string verified": func(f *fakeOIDC) { f.tamper = func(c map[string]any) { c["email_verified"] = "true" } },
		"bad signature":   func(f *fakeOIDC) { f.signKey = other },
		"no id_token":     func(f *fakeOIDC) { f.noIDTok = true },
	} {
		t.Run(name, func(t *testing.T) {
			e := newASEnv(t, "alice@example.com")
			setup(e.idp)
			resp, body, _ := e.callback(e.mustRegister())
			assertLocalError(t, resp, http.StatusForbidden, body, "could not be verified")
		})
	}
}

func TestGoogleCallbackErrors(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	resp, body := e.get(e.srv.URL + "/oauth/google/callback?code=x&state=unknown")
	assertLocalError(t, resp, http.StatusBadRequest, body, "unknown or expired")

	// A provider error is reported without reflecting the query.
	auth, _ := e.startAuthorize(e.mustRegister(), nil)
	u, _ := url.Parse(auth.Header.Get("Location"))
	q := url.Values{"state": {u.Query().Get("state")}, "error": {"call-this-number"}}
	resp, body = e.get(e.srv.URL + "/oauth/google/callback?" + q.Encode())
	assertLocalError(t, resp, http.StatusForbidden, body, "sign-in failed")
	if strings.Contains(body, "call-this-number") {
		t.Error("callback reflected the error parameter")
	}
	// The state is single use.
	resp, body = e.get(e.srv.URL + "/oauth/google/callback?" + q.Encode())
	assertLocalError(t, resp, http.StatusBadRequest, body, "unknown or expired")
}

func TestClientEvictedDuringSignIn(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	clientID := e.mustRegister()
	resp, _ := e.startAuthorize(clientID, nil)
	if _, err := e.db.Writer().ExecContext(context.Background(), `DELETE FROM oauth_clients WHERE client_id = ?`, clientID); err != nil {
		t.Fatal(err)
	}
	idp, _ := e.get(resp.Header.Get("Location"))
	page, body := e.get(idp.Header.Get("Location"))
	assertLocalError(t, page, http.StatusBadRequest, body, "registration has expired")
}

func TestRefreshReuseRevokesGrant(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	_, refresh, clientID := e.login()
	status, body := e.refresh(clientID, refresh)
	if status != http.StatusOK || body["refresh_token"] == refresh {
		t.Fatalf("refresh: %d, rotated=%v", status, body["refresh_token"] != refresh)
	}
	st, b := e.refresh(clientID, refresh)
	e.wantInvalidGrant(st, b, "reused refresh token")
	st, b = e.refresh(clientID, body["refresh_token"].(string))
	e.wantInvalidGrant(st, b, "descendant of a reused refresh token")
}

func TestOnlyAccessTokensAreBearerTokens(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	_, refresh, clientID := e.login()
	e.wantMCPUnauthorized(refresh, "refresh token as bearer")
	code, _ := e.authorize(clientID)
	e.wantMCPUnauthorized(code, "auth code as bearer")
}

func TestTokenWithoutMCPScopeForbidden(t *testing.T) {
	verify := func(context.Context, string, *http.Request) (*sdkauth.TokenInfo, error) {
		return &sdkauth.TokenInfo{Scopes: []string{scopeOffline}, UserID: "u_x", Expiration: time.Now().Add(time.Hour)}, nil
	}
	srv := httptest.NewServer(testMCPHandler(verify, "http://example/prm"))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("token without mcp scope: status %d, want 403", resp.StatusCode)
	}
}

// A client asking only for offline_access still ends up with a usable mcp token.
func TestMCPScopeAlwaysGranted(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	clientID := e.mustRegister()
	verifier := oauth2.GenerateVerifier()
	resp, _ := e.startAuthorize(clientID, func(q url.Values) {
		q.Set("scope", "offline_access")
		q.Set("code_challenge", oauth2.S256ChallengeFromVerifier(verifier))
	})
	idp, _ := e.get(resp.Header.Get("Location"))
	page, body := e.get(idp.Header.Get("Location"))
	consent := e.submitConsent(page, body, map[string]string{"decision": "approve"}, true)
	u, _ := url.Parse(consent.Header.Get("Location"))
	status, tok := e.exchangeCode(clientID, u.Query().Get("code"), verifier)
	if status != http.StatusOK {
		t.Fatalf("token: %d %v", status, tok["error"])
	}
	if !slices.Contains(strings.Fields(tok["scope"].(string)), ScopeMCP) {
		t.Errorf("granted scope %q lacks mcp", tok["scope"])
	}
	e.wantMCPAccepted(tok["access_token"].(string), "mcp token")
}

func TestAuthorizeRejectsBadRequestsLocally(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	clientID := e.mustRegister()
	for name, mutate := range map[string]func(url.Values){
		"missing challenge": func(q url.Values) { q.Del("code_challenge"); q.Del("code_challenge_method") },
		"plain method":      func(q url.Values) { q.Set("code_challenge_method", "plain") },
		"missing method":    func(q url.Values) { q.Del("code_challenge_method") },
		"unknown scope":     func(q url.Values) { q.Set("scope", "admin") },
		"unregistered URI":  func(q url.Values) { q.Set("redirect_uri", "https://evil.example/cb") },
		"unknown client":    func(q url.Values) { q.Set("client_id", "nope") },
	} {
		t.Run(name, func(t *testing.T) {
			resp, _ := e.startAuthorize(clientID, mutate)
			assertLocalError(t, resp, http.StatusBadRequest, "", "")
		})
	}
	e.as.mu.Lock()
	defer e.as.mu.Unlock()
	if len(e.as.pending) != 0 {
		t.Fatalf("%d logins parked for bad requests", len(e.as.pending))
	}
}

func TestTokenRejectsWrongVerifier(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	clientID := e.mustRegister()
	code, _ := e.authorize(clientID)
	st, b := e.exchangeCode(clientID, code, oauth2.GenerateVerifier())
	e.wantInvalidGrant(st, b, "wrong verifier")

	code, _ = e.authorize(clientID)
	st, _ = e.exchangeCode(clientID, code, "")
	if st != http.StatusBadRequest {
		t.Errorf("missing verifier: status %d, want 400", st)
	}
}

func TestPendingLoginsCapped(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	clientID := e.mustRegister()
	var last int
	for range MaxPending + 1 {
		resp, _ := e.startAuthorize(clientID, nil)
		last = resp.StatusCode
	}
	if last != http.StatusServiceUnavailable {
		t.Fatalf("request %d: status %d, want 503", MaxPending+1, last)
	}
	// Expired entries free their slots.
	e.as.mu.Lock()
	e.as.now = func() time.Time { return time.Now().Add(pendingLoginTTL + time.Minute) }
	e.as.mu.Unlock()
	if resp, _ := e.startAuthorize(clientID, nil); resp.StatusCode != http.StatusFound {
		t.Fatalf("after expiry: status %d, want 302", resp.StatusCode)
	}
}

func TestExpiredPendingRefused(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	later := func() time.Time { return time.Now().Add(pendingLoginTTL + time.Minute) }

	resp, _ := e.startAuthorize(e.mustRegister(), nil)
	idp, _ := e.get(resp.Header.Get("Location"))
	e.as.now = later
	page, body := e.get(idp.Header.Get("Location"))
	assertLocalError(t, page, http.StatusBadRequest, body, "expired")

	e.as.now = time.Now
	page, body, _ = e.callback(e.mustRegister())
	e.as.now = later
	assertLocalError(t, e.submitConsent(page, body, map[string]string{"decision": "approve"}, true), http.StatusForbidden, "", "")
}

func TestConsentPage(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	page, body, _ := e.callback(e.mustRegister())
	if page.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d, want consent page", page.StatusCode)
	}
	for _, want := range []string{"Test &lt;App&gt;", "127.0.0.1:9999", "Alice", `action="/oauth/consent"`} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	if strings.Contains(body, "<App>") || strings.Contains(body, "alice@example.com") {
		t.Error("consent page has an unescaped client name or the raw email")
	}
	if csp := page.Header.Get("Content-Security-Policy"); !cspAllowsFormTarget(csp, "http://127.0.0.1:9999") {
		t.Errorf("CSP %q blocks the post-consent redirect to the client", csp)
	}
	if page.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("consent page can be framed")
	}
	var c *http.Cookie
	for _, ck := range page.Cookies() {
		if ck.Name == consentCookie {
			c = ck
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("consent cookie %+v, want HttpOnly SameSite=Strict", c)
	}
}

// With no Google name, the page shows the subject and never the email.
func TestConsentPageWithoutName(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	e.idp.signInAs(Identity{Email: "alice@example.com", EmailVerified: true})
	_, body, _ := e.callback(e.mustRegister())
	if !strings.Contains(body, UserID("alice@example.com")) || strings.Contains(body, "alice@example.com") {
		t.Error("consent page should show the subject, not the email")
	}
}

func TestConsentCSRFAndDecision(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	clientID := e.mustRegister()
	cases := []struct {
		name   string
		fields map[string]string
		cookie bool
	}{
		{"no cookie", map[string]string{"decision": "approve"}, false},
		{"wrong csrf", map[string]string{"decision": "approve", "csrf": "forged"}, true},
		{"empty csrf", map[string]string{"decision": "approve", "csrf": ""}, true},
		{"unknown consent id", map[string]string{"decision": "approve", "consent_id": "nope"}, true},
		{"deny", map[string]string{"decision": "deny"}, true},
		{"no decision", map[string]string{"decision": ""}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, body, _ := e.callback(clientID)
			assertLocalError(t, e.submitConsent(page, body, tc.fields, tc.cookie), http.StatusForbidden, "", "")
		})
	}
	t.Run("cookie from another sign-in", func(t *testing.T) {
		_, body, _ := e.callback(clientID)
		other, _, _ := e.callback(clientID)
		form := url.Values{"decision": {"approve"}}
		for _, m := range formValue.FindAllStringSubmatch(body, -1) {
			form.Set(m[1], m[2])
		}
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/oauth/consent", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(other.Cookies()[0])
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		assertLocalError(t, resp, http.StatusForbidden, "", "")
	})
	t.Run("replay", func(t *testing.T) {
		page, body, _ := e.callback(clientID)
		if r := e.submitConsent(page, body, map[string]string{"decision": "approve"}, true); r.StatusCode != http.StatusSeeOther {
			t.Fatalf("first submit: %d", r.StatusCode)
		}
		if r := e.submitConsent(page, body, map[string]string{"decision": "approve"}, true); r.StatusCode != http.StatusForbidden {
			t.Fatalf("replayed consent: %d, want 403", r.StatusCode)
		}
	})
}

func TestConsentApproveRedirectsToClient(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	page, body, _ := e.callback(e.mustRegister())
	resp := e.submitConsent(page, body, map[string]string{"decision": "approve"}, true)
	u, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(u.String(), asRedirect) || u.Query().Get("code") == "" {
		t.Fatalf("approve: %d %s, want 303 to client with code", resp.StatusCode, u.Redacted())
	}
}

// cspAllowsFormTarget reports whether a CSP's form-action (if any) permits
// origin. Browsers apply form-action to the redirect after the POST.
func cspAllowsFormTarget(csp, origin string) bool {
	for _, d := range strings.Split(csp, ";") {
		f := strings.Fields(d)
		if len(f) > 0 && f[0] == "form-action" {
			return slices.Contains(f[1:], origin) || slices.Contains(f[1:], "*")
		}
	}
	return true
}

func TestConsentCookieSecureOnHTTPS(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	as, err := NewServer(context.Background(), Config{Issuer: "https://finance.example.com", MCPPath: asMCPPath, Storage: e.as.store, IDP: e.as.idp})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	ru, _ := url.Parse("https://claude.ai/cb")
	as.renderConsent(rec, ru, "x", "id", "csrf", "Alice")
	if c := rec.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Fatalf("cookies %+v, want one Secure cookie", c)
	}
}

func TestNoSecretsInLogs(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	access, refresh, clientID := e.login()
	code, _ := e.authorize(clientID)
	e.idp.signInAs(Identity{Email: "mallory@example.com", EmailVerified: true, Name: "Mallory"})
	e.callback(clientID)
	secret, err := e.as.store.HMACSecret(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, UserID("alice@example.com")) {
		t.Fatalf("expected the login in the logs, got %q", logs)
	}
	for name, s := range map[string]string{
		"access token": access, "refresh token": refresh, "code": code, "client id": clientID,
		"email": "alice@example.com", "rejected email": "mallory@example.com",
		"google secret": fakeClientSecret, "hmac secret": string(secret),
	} {
		if strings.Contains(logs, s) {
			t.Errorf("logs contain the %s", name)
		}
	}
}

func TestNewServerValidatesConfig(t *testing.T) {
	e := newASEnv(t, "alice@example.com")
	ctx := context.Background()
	for _, issuer := range []string{"", "finance.example.com", "http://finance.example.com", "https://finance.example.com/", "https://u@finance.example.com"} {
		_, err := NewServer(ctx, Config{Issuer: issuer, MCPPath: asMCPPath, Storage: e.as.store, IDP: e.as.idp})
		if err == nil {
			t.Errorf("issuer %q accepted", issuer)
		}
	}
	for _, issuer := range []string{"https://finance.example.com", "http://localhost:8080", "http://127.0.0.1:8080"} {
		if _, err := NewServer(ctx, Config{Issuer: issuer, MCPPath: asMCPPath, Storage: e.as.store, IDP: e.as.idp}); err != nil {
			t.Errorf("issuer %q: %v", issuer, err)
		}
	}
	if _, err := NewServer(ctx, Config{Issuer: "https://x.example", MCPPath: "mcp", Storage: e.as.store, IDP: e.as.idp}); err == nil {
		t.Error("relative MCP path accepted")
	}
	if _, err := NewServer(ctx, Config{Issuer: "https://x.example", MCPPath: asMCPPath, IDP: e.as.idp}); err == nil {
		t.Error("missing storage accepted")
	}
}
