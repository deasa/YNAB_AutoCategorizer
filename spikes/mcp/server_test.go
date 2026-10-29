package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ory/fosite"
	"golang.org/x/oauth2"
)

const testRedirect = "http://127.0.0.1:9999/callback"

// fakeIdP stands in for Google: it "signs in" as a fixed identity.
type fakeIdP struct {
	issuer string
	id     Identity
}

func (f *fakeIdP) AuthCodeURL(state, _, _ string) string {
	return f.issuer + "/oauth/google/callback?code=fake&state=" + url.QueryEscape(state)
}

func (f *fakeIdP) Exchange(context.Context, string, string, string) (Identity, error) {
	return f.id, nil
}

type testEnv struct {
	srv  *httptest.Server
	as   *AuthServer
	gens *Generations
	idp  *fakeIdP
}

func newTestEnv(t *testing.T, allowed string) *testEnv {
	t.Helper()
	var mux http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mux.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	idp := &fakeIdP{issuer: srv.URL, id: Identity{Email: "alice@example.com", EmailVerified: true, Name: "Alice"}}
	gens := NewGenerations(filepath.Join(t.TempDir(), "generations.json"))
	as, err := NewAuthServer(srv.URL, idp, ParseAllowlist(allowed), gens)
	if err != nil {
		t.Fatal(err)
	}
	m := http.NewServeMux()
	as.Register(m, mcpPath)
	m.Handle(mcpPath, mcpHandler(as.VerifyToken, as.ResourceMetadataURL()))
	mux = m
	return &testEnv{srv: srv, as: as, gens: gens, idp: idp}
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (e *testEnv) register(t *testing.T, name string, redirectURIs ...string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"redirect_uris": redirectURIs, "client_name": name})
	resp, err := http.Post(e.srv.URL+"/oauth/register", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	return resp.StatusCode, reg.ClientID
}

// startAuthorize registers a client and sends GET /oauth/authorize (not
// followed). mutate may change the query.
func (e *testEnv) startAuthorize(t *testing.T, mutate func(url.Values)) (*http.Response, string, string) {
	t.Helper()
	status, clientID := e.register(t, "Test <App>", testRedirect)
	if status != http.StatusCreated || clientID == "" {
		t.Fatalf("register: status %d", status)
	}
	verifier := oauth2.GenerateVerifier()
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirect},
		"scope":                 {"mcp"},
		"state":                 {"client-state-123"},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {e.srv.URL + mcpPath},
	}
	if mutate != nil {
		mutate(q)
	}
	resp, err := noFollow.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp, clientID, verifier
}

// consentPage runs authorize + (fake) Google callback. It returns either the
// consent page (200) or the rejection redirect.
func (e *testEnv) consentPage(t *testing.T) (*http.Response, string, string, string) {
	t.Helper()
	resp, clientID, verifier := e.startAuthorize(t, nil)
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), e.srv.URL+"/oauth/google/callback") {
		t.Fatalf("authorize: status %d, want redirect to Google", resp.StatusCode)
	}
	resp, err := noFollow.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body), clientID, verifier
}

var formValue = regexp.MustCompile(`name="(consent_id|csrf)" value="([^"]+)"`)

// submitConsent posts the consent form. cookie=false omits the CSRF cookie.
func (e *testEnv) submitConsent(t *testing.T, page *http.Response, body string, fields map[string]string, cookie bool) *http.Response {
	t.Helper()
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
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// authorize runs DCR, authorize, (fake) Google and an approved consent, and
// returns the final redirect back to the client.
func (e *testEnv) authorize(t *testing.T) (*url.URL, string, string) {
	t.Helper()
	page, body, clientID, verifier := e.consentPage(t)
	resp := page
	if page.StatusCode == http.StatusOK {
		resp = e.submitConsent(t, page, body, map[string]string{"decision": "approve"}, true)
	}
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("consent: status %d", resp.StatusCode)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u, clientID, verifier
}

func (e *testEnv) token(t *testing.T, form url.Values) (int, map[string]any) {
	t.Helper()
	resp, err := http.PostForm(e.srv.URL+"/oauth/token", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func (e *testEnv) login(t *testing.T) (access, refresh, clientID string) {
	t.Helper()
	u, clientID, verifier := e.authorize(t)
	if u.Query().Get("state") != "client-state-123" || u.Query().Get("code") == "" {
		t.Fatalf("bad client redirect: error=%q", u.Query().Get("error"))
	}
	status, body := e.token(t, url.Values{
		"grant_type": {"authorization_code"}, "code": {u.Query().Get("code")},
		"redirect_uri": {testRedirect}, "client_id": {clientID}, "code_verifier": {verifier},
	})
	if status != http.StatusOK {
		t.Fatalf("token: %d %v", status, body)
	}
	access, _ = body["access_token"].(string)
	refresh, _ = body["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("token response missing tokens: %v", body)
	}
	return access, refresh, clientID
}

func mcpStatus(t *testing.T, endpoint, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

func TestUnauthenticatedMCPReturns401WithResourceMetadata(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	want := `Bearer resource_metadata="` + e.srv.URL + `/.well-known/oauth-protected-resource", scope="mcp"`
	for _, tok := range []string{"", "not-a-real-token"} {
		status, wwwAuth := mcpStatus(t, e.srv.URL+mcpPath, tok)
		if status != http.StatusUnauthorized || wwwAuth != want {
			t.Errorf("token %q: got %d %q, want 401 %q", tok, status, wwwAuth, want)
		}
	}
}

func TestDiscoveryMetadata(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
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
	if prm["resource"] != e.srv.URL+mcpPath || prm["authorization_servers"].([]any)[0] != e.srv.URL {
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
	e := newTestEnv(t, "alice@example.com")
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
	}
	for uri, want := range tests {
		if got, _ := e.register(t, "x", uri); got != want {
			t.Errorf("register %q: status %d, want %d", uri, got, want)
		}
	}
}

func TestNonAllowlistedAccountRejected(t *testing.T) {
	for name, allowed := range map[string]string{"other account": "bob@example.com", "empty allowlist": ""} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t, allowed)
			u, _, _ := e.authorize(t)
			q := u.Query()
			if q.Get("code") != "" || q.Get("error") != "access_denied" {
				t.Fatalf("want access_denied and no code, got code=%q error=%q", q.Get("code"), q.Get("error"))
			}
			if !strings.Contains(q.Get("error_description"), "not allowed") {
				t.Errorf("error_description %q should explain the rejection", q.Get("error_description"))
			}
		})
	}
}

func TestUnverifiedEmailRejected(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	e.idp.id.EmailVerified = false
	u, _, _ := e.authorize(t)
	if u.Query().Get("error") != "access_denied" {
		t.Fatalf("want access_denied, got %q", u.Query().Get("error"))
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestHelloThenRevoke(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	access, refresh, clientID := e.login(t)

	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: e.srv.URL + mcpPath, HTTPClient: &http.Client{Transport: bearer{access}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "hello"})
	if err != nil || res.IsError {
		t.Fatalf("hello: %v %+v", err, res)
	}
	got, _ := json.Marshal(res.StructuredContent)
	want := `{"display_name":"Alice","user_id":"` + UserID("alice@example.com") + `"}`
	if string(got) != want {
		t.Fatalf("hello = %s, want %s", got, want)
	}
	if strings.Contains(string(got), "@") {
		t.Fatal("hello leaked the email")
	}

	if _, err := e.gens.Bump(UserID("alice@example.com")); err != nil {
		t.Fatal(err)
	}
	if status, _ := mcpStatus(t, e.srv.URL+mcpPath, access); status != http.StatusUnauthorized {
		t.Errorf("revoked access token: status %d, want 401", status)
	}
	status, body := e.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("revoked refresh: %d %v, want 400 invalid_grant", status, body)
	}

	// A fresh sign-in after the revoke works again.
	access2, _, _ := e.login(t)
	if status, _ := mcpStatus(t, e.srv.URL+mcpPath, access2); status == http.StatusUnauthorized {
		t.Errorf("new token after revoke rejected")
	}
}

func TestRefreshRotates(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	_, refresh, clientID := e.login(t)
	status, body := e.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if status != http.StatusOK || body["refresh_token"] == refresh {
		t.Fatalf("refresh: %d, rotated=%v", status, body["refresh_token"] != refresh)
	}
	if status, body := e.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("reused refresh token: %d %v, want invalid_grant", status, body)
	}
}

func TestOtherUserRevokeDoesNotAffect(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	access, _, _ := e.login(t)
	e.gens.Bump(UserID("bob@example.com"))
	if status, _ := mcpStatus(t, e.srv.URL+mcpPath, access); status == http.StatusUnauthorized {
		t.Fatal("revoking bob invalidated alice")
	}
	e.gens.Bump(allUsers)
	if status, _ := mcpStatus(t, e.srv.URL+mcpPath, access); status != http.StatusUnauthorized {
		t.Fatal("revoke --all did not invalidate alice")
	}
}

func TestRefreshTokenIsNotABearerToken(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	_, refresh, _ := e.login(t)
	if status, _ := mcpStatus(t, e.srv.URL+mcpPath, refresh); status != http.StatusUnauthorized {
		t.Fatalf("refresh token as bearer: status %d, want 401", status)
	}
}

func TestTokenWithoutMCPScopeForbidden(t *testing.T) {
	verify := func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{Scopes: []string{scopeOffline}, UserID: "u_x", Expiration: time.Now().Add(time.Hour)}, nil
	}
	srv := httptest.NewServer(mcpHandler(verify, "http://example/prm"))
	defer srv.Close()
	if status, _ := mcpStatus(t, srv.URL, "anything"); status != http.StatusForbidden {
		t.Fatalf("token without mcp scope: status %d, want 403", status)
	}
}

func TestMCPScopeAlwaysGranted(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	resp, _, _ := e.startAuthorize(t, func(q url.Values) { q.Set("scope", "offline_access") })
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}
	// The full flow with the default scope is covered by login; here we check
	// the parked request was widened to include mcp.
	e.as.mu.Lock()
	defer e.as.mu.Unlock()
	for _, p := range e.as.pending {
		if !p.ar.GetRequestedScopes().Has(scopeMCP) {
			t.Fatalf("requested scopes %v lack mcp", p.ar.GetRequestedScopes())
		}
	}
}

func TestAuthorizeRequiresS256PKCE(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	for name, mutate := range map[string]func(url.Values){
		"missing challenge": func(q url.Values) { q.Del("code_challenge"); q.Del("code_challenge_method") },
		"plain method":      func(q url.Values) { q.Set("code_challenge_method", "plain") },
		"missing method":    func(q url.Values) { q.Del("code_challenge_method") },
	} {
		t.Run(name, func(t *testing.T) {
			resp, _, _ := e.startAuthorize(t, mutate)
			u, _ := url.Parse(resp.Header.Get("Location"))
			if !strings.HasPrefix(u.String(), testRedirect) || u.Query().Get("error") != "invalid_request" {
				t.Fatalf("want redirect to client with invalid_request, got %d %s", resp.StatusCode, u.Redacted())
			}
		})
	}
	e.as.mu.Lock()
	defer e.as.mu.Unlock()
	if len(e.as.pending) != 0 {
		t.Fatalf("%d logins parked for bad requests", len(e.as.pending))
	}
}

func TestTokenRejectsWrongVerifier(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	u, clientID, _ := e.authorize(t)
	status, body := e.token(t, url.Values{
		"grant_type": {"authorization_code"}, "code": {u.Query().Get("code")},
		"redirect_uri": {testRedirect}, "client_id": {clientID}, "code_verifier": {oauth2.GenerateVerifier()},
	})
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v, want 400 invalid_grant", status, body)
	}
}

func TestPendingLoginsCapped(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	_, clientID := e.register(t, "x", testRedirect)
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {testRedirect}, "state": {"client-state-123"},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier())}, "code_challenge_method": {"S256"},
	}
	var last int
	for range maxPending + 1 {
		resp, err := noFollow.Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusServiceUnavailable {
		t.Fatalf("request %d: status %d, want 503", maxPending+1, last)
	}
}

func TestConsentPage(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	page, body, _, _ := e.consentPage(t)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("callback: status %d, want consent page", page.StatusCode)
	}
	for _, want := range []string{"Test &lt;App&gt;", "127.0.0.1:9999", "Alice", `action="/oauth/consent"`} {
		if !strings.Contains(body, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	if strings.Contains(body, "<App>") || strings.Contains(body, "alice@example.com") {
		t.Error("consent page has unescaped client name or the raw email")
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

func TestConsentCSRFAndDecision(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	cases := []struct {
		name   string
		fields map[string]string
		cookie bool
		status int
		errStr string
	}{
		{"no cookie", map[string]string{"decision": "approve"}, false, http.StatusForbidden, ""},
		{"wrong csrf", map[string]string{"decision": "approve", "csrf": "forged"}, true, http.StatusForbidden, ""},
		{"unknown consent id", map[string]string{"decision": "approve", "consent_id": "nope"}, true, http.StatusForbidden, ""},
		{"deny", map[string]string{"decision": "deny"}, true, http.StatusSeeOther, "access_denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, body, _, _ := e.consentPage(t)
			resp := e.submitConsent(t, page, body, tc.fields, tc.cookie)
			if resp.StatusCode != tc.status && !(tc.errStr != "" && resp.StatusCode == http.StatusFound) {
				t.Fatalf("status %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.errStr != "" {
				u, _ := url.Parse(resp.Header.Get("Location"))
				if u.Query().Get("error") != tc.errStr || u.Query().Get("code") != "" {
					t.Fatalf("redirect %s, want error=%s and no code", u.Redacted(), tc.errStr)
				}
			}
		})
	}
	t.Run("replay", func(t *testing.T) {
		page, body, _, _ := e.consentPage(t)
		if r := e.submitConsent(t, page, body, map[string]string{"decision": "approve"}, true); r.StatusCode >= 400 {
			t.Fatalf("first submit: %d", r.StatusCode)
		}
		if r := e.submitConsent(t, page, body, map[string]string{"decision": "approve"}, true); r.StatusCode != http.StatusForbidden {
			t.Fatalf("replayed consent: %d, want 403", r.StatusCode)
		}
	})
}

func TestCheckGenerationFailsClosed(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	uid := UserID("alice@example.com")
	for name, tc := range map[string]struct {
		sess *fosite.DefaultSession
		ok   bool
	}{
		"int gen":         {newSession(uid, "", 0), true},
		"json float gen":  {&fosite.DefaultSession{Subject: uid, Extra: map[string]any{genKey: float64(0)}}, true},
		"missing gen":     {&fosite.DefaultSession{Subject: uid}, false},
		"non-numeric gen": {&fosite.DefaultSession{Subject: uid, Extra: map[string]any{genKey: "0"}}, false},
		"fractional gen":  {&fosite.DefaultSession{Subject: uid, Extra: map[string]any{genKey: 0.5}}, false},
		"empty subject":   {newSession("", "", 0), false},
	} {
		if err := e.as.checkGeneration(tc.sess); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", name, err, tc.ok)
		}
	}
}
