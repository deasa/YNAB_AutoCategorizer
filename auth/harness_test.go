package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const testRedirect = "http://localhost:9999/cb"

// env is a temp database with a fosite provider composed as the spike does it.
type env struct {
	t        *testing.T
	path     string
	db       *store.DB
	s        *Storage
	provider fosite.OAuth2Provider
	secret   []byte
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, path: filepath.Join(t.TempDir(), "finbot.db")}
	e.open()
	t.Cleanup(func() { e.db.Close() })
	return e
}

func (e *env) open() {
	e.t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, e.path)
	if err != nil {
		e.t.Fatalf("store.Open: %v", err)
	}
	e.db, e.s = db, New(db)
	if e.secret, err = e.s.HMACSecret(ctx); err != nil {
		e.t.Fatalf("HMACSecret: %v", err)
	}
	cfg := &fosite.Config{
		AccessTokenLifespan:            time.Hour,
		RefreshTokenLifespan:           30 * 24 * time.Hour,
		AuthorizeCodeLifespan:          5 * time.Minute,
		GlobalSecret:                   e.secret,
		EnforcePKCE:                    true,
		DisableRefreshTokenValidation:  true,
		EnablePKCEPlainChallengeMethod: false,
		RefreshTokenScopes:             []string{},
		ScopeStrategy:                  fosite.ExactScopeStrategy,
		TokenURL:                       "https://finbot.test/oauth/token",
	}
	e.provider = compose.Compose(cfg, e.s,
		&compose.CommonStrategy{CoreStrategy: compose.NewOAuth2HMACStrategy(cfg)},
		compose.OAuth2AuthorizeExplicitFactory,
		compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2PKCEFactory,
		compose.OAuth2TokenIntrospectionFactory,
		compose.OAuth2TokenRevocationFactory,
	)
}

func (e *env) reopen() {
	e.t.Helper()
	if err := e.db.Close(); err != nil {
		e.t.Fatalf("close: %v", err)
	}
	e.open()
}

func (e *env) register() string {
	e.t.Helper()
	id := rand.Text()
	if err := e.s.RegisterClient(context.Background(), testClient(id), "Test App"); err != nil {
		e.t.Fatalf("RegisterClient: %v", err)
	}
	return id
}

func testClient(id string) *fosite.DefaultClient {
	return &fosite.DefaultClient{
		ID:            id,
		RedirectURIs:  []string{testRedirect},
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},
		Scopes:        []string{"mcp", "offline_access"},
		Public:        true,
	}
}

type issued struct {
	clientID, code, verifier, access, refresh string
}

// authorize runs the authorization endpoint for user. extra adds query parameters.
func (e *env) authorize(clientID, user string, extra url.Values) issued {
	e.t.Helper()
	ctx := context.Background()
	verifier := rand.Text() + rand.Text()
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirect},
		"scope":                 {"mcp offline_access"},
		"state":                 {rand.Text()},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	for k, v := range extra {
		q[k] = v
	}
	r, err := http.NewRequest(http.MethodGet, "https://finbot.test/oauth/authorize?"+q.Encode(), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	ar, err := e.provider.NewAuthorizeRequest(ctx, r)
	if err != nil {
		e.t.Fatalf("NewAuthorizeRequest: %v", fosite.ErrorToRFC6749Error(err).GetDescription())
	}
	for _, sc := range ar.GetRequestedScopes() {
		ar.GrantScope(sc)
	}
	gen, err := e.s.CurrentGeneration(ctx, user)
	if err != nil {
		e.t.Fatal(err)
	}
	resp, err := e.provider.NewAuthorizeResponse(ctx, ar, NewSession(user, "Name of "+user, gen))
	if err != nil {
		e.t.Fatalf("NewAuthorizeResponse: %v", fosite.ErrorToRFC6749Error(err).GetDescription())
	}
	return issued{clientID: clientID, code: resp.GetParameters().Get("code"), verifier: verifier}
}

// token posts form to the token endpoint and returns the tokens or fosite's error.
func (e *env) token(form url.Values) (access, refresh string, err error) {
	e.t.Helper()
	ctx := context.Background()
	r, rerr := http.NewRequest(http.MethodPost, "https://finbot.test/oauth/token", strings.NewReader(form.Encode()))
	if rerr != nil {
		e.t.Fatal(rerr)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ar, err := e.provider.NewAccessRequest(ctx, r, &fosite.DefaultSession{})
	if err != nil {
		return "", "", err
	}
	resp, err := e.provider.NewAccessResponse(ctx, ar)
	if err != nil {
		return "", "", err
	}
	refresh, _ = resp.GetExtra("refresh_token").(string)
	return resp.GetAccessToken(), refresh, nil
}

func (e *env) exchange(i issued) (access, refresh string, err error) {
	e.t.Helper()
	return e.token(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {i.code},
		"code_verifier": {i.verifier},
		"client_id":     {i.clientID},
		"redirect_uri":  {testRedirect},
	})
}

func (e *env) refreshWith(clientID, refresh string) (string, string, error) {
	e.t.Helper()
	return e.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
}

// login runs a whole sign-in for user on a new client.
func (e *env) login(user string) issued {
	e.t.Helper()
	return e.loginClient(e.register(), user)
}

func (e *env) loginClient(clientID, user string) issued {
	e.t.Helper()
	i := e.authorize(clientID, user, nil)
	var err error
	if i.access, i.refresh, err = e.exchange(i); err != nil {
		e.t.Fatalf("exchange: %v", describe(err))
	}
	if i.access == "" || i.refresh == "" {
		e.t.Fatalf("exchange gave access=%t refresh=%t", i.access != "", i.refresh != "")
	}
	return i
}

// accessValid reports whether introspection accepts token as an access token.
func (e *env) accessValid(token string) bool {
	e.t.Helper()
	tu, _, err := e.provider.IntrospectToken(context.Background(), token, fosite.AccessToken, &fosite.DefaultSession{})
	return err == nil && tu == fosite.AccessToken
}

func (e *env) revoke(clientID, token, hint string) error {
	e.t.Helper()
	form := url.Values{"token": {token}, "client_id": {clientID}}
	if hint != "" {
		form.Set("token_type_hint", hint)
	}
	r, err := http.NewRequest(http.MethodPost, "https://finbot.test/oauth/revoke", strings.NewReader(form.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return e.provider.NewRevocationRequest(context.Background(), r)
}

func (e *env) count(table string) int {
	e.t.Helper()
	var n int
	if err := e.db.Reader().QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		e.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// describe flattens everything an error exposes, for assertions and failures.
func describe(err error) string {
	if err == nil {
		return ""
	}
	e := fosite.ErrorToRFC6749Error(err)
	return strings.Join([]string{err.Error(), e.ErrorField, e.GetDescription(), e.DebugField, e.HintField}, " | ")
}

func wantInvalidGrant(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: succeeded, want invalid_grant", what)
	}
	if got := fosite.ErrorToRFC6749Error(err).ErrorField; got != "invalid_grant" {
		t.Fatalf("%s: error = %q, want invalid_grant", what, got)
	}
}
