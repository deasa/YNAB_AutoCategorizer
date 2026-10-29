package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	m.Handle(mcpPath, mcpHandler(as))
	mux = m
	return &testEnv{srv: srv, as: as, gens: gens, idp: idp}
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// authorize runs DCR + authorize + (fake) Google callback and returns the
// final redirect back to the client, plus the client id and PKCE verifier.
func (e *testEnv) authorize(t *testing.T) (*url.URL, string, string) {
	t.Helper()
	resp, err := http.Post(e.srv.URL+"/oauth/register", "application/json",
		strings.NewReader(`{"redirect_uris":["`+testRedirect+`"],"client_name":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || reg.ClientID == "" {
		t.Fatalf("register: status %d", resp.StatusCode)
	}

	verifier := oauth2.GenerateVerifier()
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {reg.ClientID},
		"redirect_uri":          {testRedirect},
		"scope":                 {"mcp"},
		"state":                 {"client-state-123"},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {e.srv.URL + mcpPath},
	}
	loc := e.srv.URL + "/oauth/authorize?" + q.Encode()
	for range 2 { // authorize -> "Google" callback -> client redirect
		resp, err := noFollow.Get(loc)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("GET %s: status %d", resp.Request.URL.Path, resp.StatusCode)
		}
		loc = resp.Header.Get("Location")
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return u, reg.ClientID, verifier
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
	want := `Bearer resource_metadata="` + e.srv.URL + `/.well-known/oauth-protected-resource"`
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

func TestRegisterRejectsNonLoopbackHTTP(t *testing.T) {
	e := newTestEnv(t, "alice@example.com")
	resp, err := http.Post(e.srv.URL+"/oauth/register", "application/json",
		strings.NewReader(`{"redirect_uris":["http://evil.example.com/cb"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", resp.StatusCode)
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
