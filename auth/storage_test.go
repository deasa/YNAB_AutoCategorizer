package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ory/fosite"
)

var ctxBG = context.Background()

// request builds a stored-request fixture with every expiry set from now.
func request(t *testing.T, e *env, clientID, id, user string) *fosite.Request {
	t.Helper()
	exp := e.s.now().Add(time.Hour)
	sess := NewSession(user, "N", 0)
	sess.SetExpiresAt(fosite.AuthorizeCode, exp)
	sess.SetExpiresAt(fosite.AccessToken, exp)
	sess.SetExpiresAt(fosite.RefreshToken, exp)
	r := fosite.NewRequest()
	r.ID = id
	r.Client = testClient(clientID)
	r.Session = sess
	r.GrantScope("mcp")
	r.SetRequestedScopes(fosite.Arguments{"mcp"})
	r.Form.Set("redirect_uri", testRedirect)
	return r
}

func TestAuthorizeCodeMethods(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	if _, err := e.s.GetAuthorizeCodeSession(ctxBG, "sig", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if err := e.s.InvalidateAuthorizeCodeSession(ctxBG, "sig"); !errors.Is(err, fosite.ErrNotFound) {
		t.Fatalf("Invalidate missing = %v", err)
	}
	if err := e.s.CreateAuthorizeCodeSession(ctxBG, "sig", request(t, e, c, "req1", "alice")); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.GetAuthorizeCodeSession(ctxBG, "sig", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetID() != "req1" || got.GetClient().GetID() != c || got.GetRequestForm().Get("redirect_uri") != testRedirect ||
		!got.GetGrantedScopes().Has("mcp") || !got.GetRequestedScopes().Has("mcp") {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if err := e.s.InvalidateAuthorizeCodeSession(ctxBG, "sig"); err != nil {
		t.Fatal(err)
	}
	got, err = e.s.GetAuthorizeCodeSession(ctxBG, "sig", nil)
	if !errors.Is(err, fosite.ErrInvalidatedAuthorizeCode) || got == nil || got.GetID() != "req1" {
		t.Fatalf("Get used code = %v, %v; fosite needs the request with this error", got, err)
	}
	if err := e.s.InvalidateAuthorizeCodeSession(ctxBG, "sig"); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("second Invalidate = %v, want ErrNotFound", err)
	}
}

func TestPKCEMethods(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	if _, err := e.s.GetPKCERequestSession(ctxBG, "sig", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	r := request(t, e, c, "req1", "alice")
	r.Form.Set("code_challenge", "abc")
	r.Form.Set("code_challenge_method", "S256")
	if err := e.s.CreatePKCERequestSession(ctxBG, "sig", r); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.GetPKCERequestSession(ctxBG, "sig", nil)
	if err != nil || got.GetRequestForm().Get("code_challenge") != "abc" || got.GetRequestForm().Get("code_challenge_method") != "S256" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if err := e.s.DeletePKCERequestSession(ctxBG, "sig"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetPKCERequestSession(ctxBG, "sig", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("Get deleted = %v", err)
	}
	if err := e.s.DeletePKCERequestSession(ctxBG, "sig"); err != nil {
		t.Errorf("Delete missing = %v", err)
	}
}

func TestAccessTokenMethods(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	if _, err := e.s.GetAccessTokenSession(ctxBG, "a", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	for _, sig := range []string{"a", "b"} {
		if err := e.s.CreateAccessTokenSession(ctxBG, sig, request(t, e, c, "req1", "alice")); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.s.CreateAccessTokenSession(ctxBG, "other", request(t, e, c, "req2", "alice")); err != nil {
		t.Fatal(err)
	}
	if got, err := e.s.GetAccessTokenSession(ctxBG, "a", &fosite.DefaultSession{}); err != nil || got.GetID() != "req1" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if err := e.s.DeleteAccessTokenSession(ctxBG, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "a", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("Get deleted = %v", err)
	}
	if err := e.s.RevokeAccessToken(ctxBG, "req1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "b", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("Get revoked = %v", err)
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "other", nil); err != nil {
		t.Errorf("revoke hit another request: %v", err)
	}
	if err := e.s.RevokeAccessToken(ctxBG, "no-such"); err != nil {
		t.Errorf("revoke unknown = %v", err)
	}
}

func TestRefreshTokenMethods(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	if _, err := e.s.GetRefreshTokenSession(ctxBG, "r1", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	mk := func(sig, req string) {
		t.Helper()
		if err := e.s.CreateRefreshTokenSession(ctxBG, sig, "access-sig", request(t, e, c, req, "alice")); err != nil {
			t.Fatal(err)
		}
	}
	mk("r1", "req1")
	mk("other", "req2")
	if err := e.s.CreateAccessTokenSession(ctxBG, "a1", request(t, e, c, "req1", "alice")); err != nil {
		t.Fatal(err)
	}

	if err := e.s.RotateRefreshToken(ctxBG, "req1", "no-such"); !errors.Is(err, fosite.ErrSerializationFailure) {
		t.Errorf("rotate unknown = %v", err)
	}
	if err := e.s.RotateRefreshToken(ctxBG, "req2", "r1"); !errors.Is(err, fosite.ErrSerializationFailure) {
		t.Errorf("rotate under the wrong request = %v", err)
	}
	if err := e.s.RotateRefreshToken(ctxBG, "req1", "r1"); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.GetRefreshTokenSession(ctxBG, "r1", nil)
	if !errors.Is(err, fosite.ErrInactiveToken) || got == nil || got.GetID() != "req1" {
		t.Fatalf("Get rotated = %v, %v; fosite needs the request with this error", got, err)
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "a1", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("rotate left the access token: %v", err)
	}
	if err := e.s.RotateRefreshToken(ctxBG, "req1", "r1"); !errors.Is(err, fosite.ErrSerializationFailure) {
		t.Errorf("second rotate = %v", err)
	}
	if _, err := e.s.GetRefreshTokenSession(ctxBG, "other", nil); err != nil {
		t.Errorf("rotate hit another request: %v", err)
	}

	if err := e.s.RevokeRefreshToken(ctxBG, "req2"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetRefreshTokenSession(ctxBG, "other", nil); !errors.Is(err, fosite.ErrInactiveToken) {
		t.Errorf("Get revoked = %v", err)
	}
	if err := e.s.DeleteRefreshTokenSession(ctxBG, "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetRefreshTokenSession(ctxBG, "other", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("Get deleted = %v", err)
	}
}

func TestCreateRefusals(t *testing.T) {
	e := newEnv(t)
	c := e.register()

	type otherSession struct{ *fosite.DefaultSession }
	r := request(t, e, c, "r", "alice")
	r.Session = otherSession{&fosite.DefaultSession{}}
	if err := e.s.CreateAccessTokenSession(ctxBG, "s", r); err == nil {
		t.Error("stored a session of another type")
	}

	r = request(t, e, c, "r", "alice")
	r.Session = NewSession("alice", "N", 0) // no expiry
	if err := e.s.CreateAccessTokenSession(ctxBG, "s", r); err == nil {
		t.Error("stored a session with no expiry")
	}

	r = request(t, e, c, "r", "alice")
	r.Client = nil
	if err := e.s.CreateAccessTokenSession(ctxBG, "s", r); err == nil {
		t.Error("stored a request with no client")
	}

	if err := e.s.CreateAccessTokenSession(ctxBG, "s", request(t, e, "no-such-client", "r", "alice")); err == nil {
		t.Error("stored a token for an unknown client")
	}
}

func TestStoredFormIsAllowlisted(t *testing.T) {
	e := newEnv(t)
	r := request(t, e, e.register(), "r", "alice")
	for _, k := range []string{"code", "code_verifier", "refresh_token", "token", "client_secret", "state"} {
		r.Form.Set(k, "raw")
	}
	if err := e.s.CreateAuthorizeCodeSession(ctxBG, "s", r); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.GetAuthorizeCodeSession(ctxBG, "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k := range got.GetRequestForm() {
		if k != "redirect_uri" {
			t.Errorf("stored form key %q", k)
		}
	}
}

func TestCheckSessionFailsClosed(t *testing.T) {
	e := newEnv(t)
	for name, sess := range map[string]*fosite.DefaultSession{
		"nil":          nil,
		"no subject":   {Extra: map[string]any{GenKey: 0}},
		"no gen":       {Subject: "alice"},
		"string gen":   {Subject: "alice", Extra: map[string]any{GenKey: "0"}},
		"fraction gen": {Subject: "alice", Extra: map[string]any{GenKey: 0.5}},
		"future gen":   {Subject: "alice", Extra: map[string]any{GenKey: 3}},
	} {
		if err := e.s.CheckSession(ctxBG, sess); !errors.Is(err, ErrRevoked) {
			t.Errorf("%s: %v, want ErrRevoked", name, err)
		}
	}
	for _, gen := range []any{0, float64(0)} {
		if err := e.s.CheckSession(ctxBG, &fosite.DefaultSession{Subject: "alice", Extra: map[string]any{GenKey: gen}}); err != nil {
			t.Errorf("gen %T: %v", gen, err)
		}
	}
	// A stored session without a generation reads as not found.
	r := request(t, e, e.register(), "r", "alice")
	r.Session.(*fosite.DefaultSession).Extra = nil
	if err := e.s.CreateAccessTokenSession(ctxBG, "s", r); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "s", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
}

func TestGenerations(t *testing.T) {
	e := newEnv(t)
	if got, err := e.s.CurrentGeneration(ctxBG, "alice"); err != nil || got != 0 {
		t.Fatalf("Current = %d, %v", got, err)
	}
	for want := 1; want <= 2; want++ {
		if got, err := e.s.BumpGeneration(ctxBG, "alice"); err != nil || got != want {
			t.Fatalf("Bump = %d, %v; want %d", got, err, want)
		}
	}
	if _, err := e.s.BumpGeneration(ctxBG, AllUsers); err != nil {
		t.Fatal(err)
	}
	for user, want := range map[string]int{"alice": 3, "bob": 1, AllUsers: 1} {
		if got, err := e.s.CurrentGeneration(ctxBG, user); err != nil || got != want {
			t.Errorf("Current(%s) = %d, %v; want %d", user, got, err, want)
		}
	}
	if _, err := e.s.BumpGeneration(ctxBG, ""); err == nil {
		t.Error("Bump of an empty user succeeded")
	}
}

func TestHMACSecret(t *testing.T) {
	e := newEnv(t) // the harness has already asked for it once
	if len(e.secret) != secretLen {
		t.Fatalf("secret is %d bytes", len(e.secret))
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := e.s.HMACSecret(ctxBG)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || string(got) != string(e.secret) {
				t.Errorf("HMACSecret differs: %v", err)
			}
		}()
	}
	wg.Wait()
	if n := e.count("oauth_hmac_secret"); n != 1 {
		t.Errorf("%d secret rows", n)
	}
	other := newEnv(t)
	if string(other.secret) == string(e.secret) {
		t.Error("two databases share a secret")
	}
}

func TestConcurrentFirstSecret(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.w.Exec(`DELETE FROM oauth_hmac_secret`); err != nil {
		t.Fatal(err)
	}
	results := make([]string, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := e.s.HMACSecret(ctxBG)
			if err != nil {
				t.Errorf("HMACSecret: %v", err)
			}
			results[i] = string(got)
		}()
	}
	wg.Wait()
	for _, r := range results {
		if r != results[0] || len(r) != secretLen {
			t.Fatal("concurrent first use gave different secrets")
		}
	}
}

func TestTransactions(t *testing.T) {
	e := newEnv(t)
	c := e.register()

	// Rollback undoes everything, and a transaction reads its own writes.
	tx, err := e.s.BeginTX(ctxBG)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.CreateAccessTokenSession(tx, "a", request(t, e, c, "r", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.GetAccessTokenSession(tx, "a", nil); err != nil {
		t.Errorf("read own write: %v", err)
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "a", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("uncommitted row visible outside: %v", err)
	}
	if _, err := e.s.BeginTX(tx); err == nil {
		t.Error("nested BeginTX succeeded")
	}
	if err := e.s.Rollback(tx); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Rollback(tx); err != nil {
		t.Errorf("second Rollback = %v", err)
	}
	if err := e.s.Commit(tx); err == nil {
		t.Error("Commit after Rollback succeeded")
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "a", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("rolled-back row persisted: %v", err)
	}

	// Commit persists it, and Rollback afterwards changes nothing.
	if tx, err = e.s.BeginTX(ctxBG); err != nil {
		t.Fatal(err)
	}
	if err := e.s.CreateAccessTokenSession(tx, "a", request(t, e, c, "r", "alice")); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Commit(tx); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Rollback(tx); err != nil {
		t.Errorf("Rollback after Commit = %v", err)
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "a", nil); err != nil {
		t.Errorf("committed row missing: %v", err)
	}

	if err := e.s.Commit(ctxBG); err == nil {
		t.Error("Commit with no transaction succeeded")
	}
	if err := e.s.Rollback(ctxBG); err == nil {
		t.Error("Rollback with no transaction succeeded")
	}

	// A failed Commit frees the writer by itself, and Rollback is then a no-op.
	cctx, cancel := context.WithCancel(ctxBG)
	if tx, err = e.s.BeginTX(cctx); err != nil {
		t.Fatal(err)
	}
	if err := e.s.CreateAccessTokenSession(tx, "cancelled", request(t, e, c, "r", "alice")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := e.s.Commit(tx); err == nil {
		t.Error("Commit on a cancelled context succeeded")
	}
	wctx, wcancel := context.WithTimeout(ctxBG, 5*time.Second)
	defer wcancel()
	if _, err := e.s.BumpGeneration(wctx, "bob"); err != nil {
		t.Errorf("writer not released: %v", err)
	}
	if err := e.s.Rollback(tx); err != nil {
		t.Errorf("Rollback: %v", err)
	}
	if _, err := e.s.GetAccessTokenSession(ctxBG, "cancelled", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("failed Commit persisted the row: %v", err)
	}

	// A cancelled request can still roll back and free the writer.
	cctx, cancel = context.WithCancel(ctxBG)
	if tx, err = e.s.BeginTX(cctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := e.s.Rollback(tx); err != nil {
		t.Errorf("Rollback on a cancelled context: %v", err)
	}
	if _, err := e.s.BumpGeneration(wctx, "bob"); err != nil {
		t.Errorf("writer not released after Rollback: %v", err)
	}
}

func TestPrune(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	insert := func(suffix string) {
		t.Helper()
		sig, req := "s"+suffix, "r"+suffix
		for _, err := range []error{
			e.s.CreateAuthorizeCodeSession(ctxBG, sig, request(t, e, c, req, "alice")),
			e.s.CreatePKCERequestSession(ctxBG, sig, request(t, e, c, req, "alice")),
			e.s.CreateAccessTokenSession(ctxBG, sig, request(t, e, c, req, "alice")),
			e.s.CreateRefreshTokenSession(ctxBG, sig, "", request(t, e, c, req, "alice")),
		} {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now()
	e.s.now = func() time.Time { return now.Add(-2 * time.Hour) }
	insert("old") // expired an hour before now
	e.s.now = func() time.Time { return now }
	insert("new")
	// A rotated token that hasn't expired must stay, for replay detection.
	if err := e.s.RotateRefreshToken(ctxBG, "rnew", "snew"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.InvalidateAuthorizeCodeSession(ctxBG, "snew"); err != nil {
		t.Fatal(err)
	}

	n, err := e.s.Prune(ctxBG)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("Prune removed %d rows, want 4", n)
	}
	if _, err := e.s.GetRefreshTokenSession(ctxBG, "sold", nil); !errors.Is(err, fosite.ErrNotFound) {
		t.Errorf("expired token: %v", err)
	}
	if _, err := e.s.GetRefreshTokenSession(ctxBG, "snew", nil); !errors.Is(err, fosite.ErrInactiveToken) {
		t.Errorf("rotated unexpired token: %v", err)
	}
	if _, err := e.s.GetAuthorizeCodeSession(ctxBG, "snew", nil); !errors.Is(err, fosite.ErrInvalidatedAuthorizeCode) {
		t.Errorf("used unexpired code: %v", err)
	}
	if n, err := e.s.Prune(ctxBG); err != nil || n != 0 {
		t.Errorf("second Prune = %d, %v", n, err)
	}
	if _, err := e.s.GetClient(ctxBG, c); err != nil {
		t.Errorf("Prune removed the client: %v", err)
	}
}

func TestClientRoundTrip(t *testing.T) {
	e := newEnv(t)
	if _, err := e.s.GetClient(ctxBG, "no-such"); !errors.Is(err, fosite.ErrNotFound) {
		t.Fatalf("GetClient missing = %v", err)
	}
	if _, err := e.s.ClientName(ctxBG, "no-such"); !errors.Is(err, fosite.ErrNotFound) {
		t.Fatalf("ClientName missing = %v", err)
	}
	if err := e.s.RegisterClient(ctxBG, testClient("c1"), "Claude"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RegisterClient(ctxBG, testClient("c1"), "Again"); err == nil {
		t.Error("registered a duplicate id")
	}
	e.reopen()
	got, err := e.s.GetClient(ctxBG, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsPublic() || got.GetID() != "c1" || got.GetRedirectURIs()[0] != testRedirect ||
		!got.GetGrantTypes().Has("refresh_token") || !got.GetResponseTypes().Has("code") || !got.GetScopes().Has("mcp") {
		t.Errorf("client = %+v", got)
	}
	if name, err := e.s.ClientName(ctxBG, "c1"); err != nil || name != "Claude" {
		t.Errorf("ClientName = %q, %v", name, err)
	}
	conf := testClient("c2")
	conf.Public = false
	if err := e.s.RegisterClient(ctxBG, conf, ""); err == nil {
		t.Error("registered a confidential client")
	}
	if err := e.s.RegisterClient(ctxBG, testClient(""), ""); err == nil {
		t.Error("registered an empty id")
	}
}

func TestClientAssertionsRefused(t *testing.T) {
	e := newEnv(t)
	if err := e.s.ClientAssertionJWTValid(ctxBG, "jti"); !errors.Is(err, fosite.ErrJTIKnown) {
		t.Errorf("Valid = %v", err)
	}
	if err := e.s.SetClientAssertionJWT(ctxBG, "jti", time.Now()); !errors.Is(err, fosite.ErrJTIKnown) {
		t.Errorf("Set = %v", err)
	}
}

// fill registers n clients with strictly increasing created_at, oldest first.
func fill(t *testing.T, e *env, n int) []string {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	real := e.s.now
	defer func() { e.s.now = real }()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("client-%04d", i)
		e.s.now = func() time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
		if err := e.s.RegisterClient(ctxBG, testClient(ids[i]), ""); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	return ids
}

func exists(t *testing.T, e *env, id string) bool {
	t.Helper()
	_, err := e.s.GetClient(ctxBG, id)
	if err != nil && !errors.Is(err, fosite.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func TestUnusedClientsDontBlockRegistration(t *testing.T) {
	e := newEnv(t)
	ids := fill(t, e, MaxClients)
	if err := e.s.RegisterClient(ctxBG, testClient("new1"), ""); err != nil {
		t.Fatalf("registration at the cap: %v", err)
	}
	if n := e.count("oauth_clients"); n != MaxClients {
		t.Errorf("%d clients, want %d", n, MaxClients)
	}
	if exists(t, e, ids[0]) {
		t.Error("the oldest unused client was not evicted")
	}
	if !exists(t, e, ids[1]) || !exists(t, e, "new1") {
		t.Error("eviction removed more than it had to")
	}
}

func TestLiveClientSurvivesEviction(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		live bool
		seed func(t *testing.T, e *env, c string)
	}{
		{"live auth code", true, func(t *testing.T, e *env, c string) {
			mustNil(t, e.s.CreateAuthorizeCodeSession(ctxBG, "s", request(t, e, c, "r", "alice")))
		}},
		{"used auth code", false, func(t *testing.T, e *env, c string) {
			mustNil(t, e.s.CreateAuthorizeCodeSession(ctxBG, "s", request(t, e, c, "r", "alice")))
			mustNil(t, e.s.InvalidateAuthorizeCodeSession(ctxBG, "s"))
		}},
		{"live access token", true, func(t *testing.T, e *env, c string) {
			mustNil(t, e.s.CreateAccessTokenSession(ctxBG, "s", request(t, e, c, "r", "alice")))
		}},
		{"expired access token", false, func(t *testing.T, e *env, c string) {
			e.s.now = func() time.Time { return now.Add(-2 * time.Hour) }
			defer func() { e.s.now = time.Now }()
			mustNil(t, e.s.CreateAccessTokenSession(ctxBG, "s", request(t, e, c, "r", "alice")))
		}},
		{"live refresh token", true, func(t *testing.T, e *env, c string) {
			mustNil(t, e.s.CreateRefreshTokenSession(ctxBG, "s", "", request(t, e, c, "r", "alice")))
		}},
		{"rotated refresh token", false, func(t *testing.T, e *env, c string) {
			mustNil(t, e.s.CreateRefreshTokenSession(ctxBG, "s", "", request(t, e, c, "r", "alice")))
			mustNil(t, e.s.RotateRefreshToken(ctxBG, "r", "s"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			ids := fill(t, e, MaxClients)
			tc.seed(t, e, ids[0]) // the oldest, so first in line
			mustNil(t, e.s.RegisterClient(ctxBG, testClient("new1"), ""))
			if got := exists(t, e, ids[0]); got != tc.live {
				t.Errorf("client exists = %t, want %t", got, tc.live)
			}
			if tc.live && exists(t, e, ids[1]) {
				t.Error("the next-oldest client should have been evicted instead")
			}
			if !tc.live && e.count(tableCodes)+e.count(tableAccess)+e.count(tableRefresh) != 0 {
				t.Error("eviction left the client's dead rows behind")
			}
		})
	}
}

func TestRealGrantSurvivesEvictionAndStillWorks(t *testing.T) {
	e := newEnv(t)
	i := e.login("alice")
	fill(t, e, MaxClients-1)
	mustNil(t, e.s.RegisterClient(ctxBG, testClient("new1"), ""))
	e.reopen()
	if !e.accessValid(i.access) {
		t.Fatal("the grant died with eviction")
	}
	if _, _, err := e.refreshWith(i.clientID, i.refresh); err != nil {
		t.Fatalf("refresh: %v", describe(err))
	}
}

func TestAllClientsLiveRefusesRegistration(t *testing.T) {
	e := newEnv(t)
	ids := fill(t, e, MaxClients)
	for i, c := range ids {
		mustNil(t, e.s.CreateAccessTokenSession(ctxBG, fmt.Sprint("s", i), request(t, e, c, fmt.Sprint("r", i), "alice")))
	}
	if err := e.s.RegisterClient(ctxBG, testClient("new1"), ""); !errors.Is(err, ErrTooManyClients) {
		t.Fatalf("RegisterClient = %v, want ErrTooManyClients", err)
	}
	if n := e.count("oauth_clients"); n != MaxClients {
		t.Errorf("%d clients after the refusal", n)
	}
	// The refusal rolled back and released the writer.
	if _, err := e.s.BumpGeneration(ctxBG, "bob"); err != nil {
		t.Errorf("writer: %v", err)
	}
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
