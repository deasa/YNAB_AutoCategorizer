package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"log"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ory/fosite"
)

func TestTokensAndSecretSurviveReopen(t *testing.T) {
	e := newEnv(t)
	i := e.login("alice")
	secret := bytes.Clone(e.secret)

	e.reopen()

	if !bytes.Equal(e.secret, secret) {
		t.Fatal("HMAC secret changed across reopen")
	}
	if !e.accessValid(i.access) {
		t.Fatal("access token rejected after reopen")
	}
	access, refresh, err := e.refreshWith(i.clientID, i.refresh)
	if err != nil {
		t.Fatalf("refresh after reopen: %v", describe(err))
	}
	if refresh == i.refresh || access == i.access || !e.accessValid(access) {
		t.Fatal("refresh after reopen did not issue a new, valid pair")
	}
	if e.accessValid(i.access) {
		t.Fatal("the old access token still validates after rotation")
	}
}

func TestRestoredSessionKeepsIdentity(t *testing.T) {
	e := newEnv(t)
	i := e.login("alice")
	e.reopen()
	_, ar, err := e.provider.IntrospectToken(context.Background(), i.access, fosite.AccessToken, &fosite.DefaultSession{})
	if err != nil {
		t.Fatalf("introspect: %v", describe(err))
	}
	sess := ar.GetSession().(*fosite.DefaultSession)
	if sess.Subject != "alice" || sess.Username != "Name of alice" {
		t.Errorf("session = %q %q", sess.Subject, sess.Username)
	}
	if sess.GetExpiresAt(fosite.AccessToken).IsZero() {
		t.Error("session lost its access expiry")
	}
	if got := ar.GetGrantedScopes(); !got.Has("mcp") || !got.Has("offline_access") {
		t.Errorf("granted scopes = %v", got)
	}
	if ar.GetClient().GetID() != i.clientID || !ar.GetClient().IsPublic() {
		t.Errorf("client not restored")
	}
}

func TestReplayedRefreshRevokesGrantAcrossReopen(t *testing.T) {
	e := newEnv(t)
	i := e.login("alice")
	other := e.loginClient(i.clientID, "alice") // a second grant on the same client

	access2, refresh2, err := e.refreshWith(i.clientID, i.refresh)
	if err != nil {
		t.Fatalf("rotate: %v", describe(err))
	}
	e.reopen()

	_, _, err = e.refreshWith(i.clientID, i.refresh)
	wantInvalidGrant(t, err, "replay of the rotated token")
	e.reopen()

	if e.accessValid(access2) {
		t.Error("replay left the new access token valid")
	}
	_, _, err = e.refreshWith(i.clientID, refresh2)
	wantInvalidGrant(t, err, "the new refresh token after a replay")
	_, _, err = e.refreshWith(i.clientID, i.refresh)
	wantInvalidGrant(t, err, "a second replay")

	if !e.accessValid(other.access) {
		t.Error("replay revoked another grant's access token")
	}
	if _, _, err := e.refreshWith(i.clientID, other.refresh); err != nil {
		t.Errorf("replay revoked another grant's refresh token: %v", describe(err))
	}
}

func TestRefreshTokenIsNotABearerToken(t *testing.T) {
	e := newEnv(t)
	i := e.login("alice")
	if e.accessValid(i.refresh) {
		t.Fatal("a refresh token validated as an access token")
	}
}

func TestConcurrentRefreshIssuesOnce(t *testing.T) {
	e := newEnv(t)
	i := e.login("alice")
	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok int
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := e.refreshWith(i.clientID, i.refresh); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d concurrent refreshes succeeded, want 1", ok)
	}
	// The writer must not be left held by any loser.
	if _, err := e.s.BumpGeneration(context.Background(), "bob"); err != nil {
		t.Fatalf("writer stuck: %v", err)
	}
}

func TestReplayedCodeRevokesGrant(t *testing.T) {
	e := newEnv(t)
	i := e.authorize(e.register(), "alice", nil)
	access, refresh, err := e.exchange(i)
	if err != nil {
		t.Fatalf("exchange: %v", describe(err))
	}
	e.reopen()
	_, _, err = e.exchange(i)
	wantInvalidGrant(t, err, "code replay")
	if e.accessValid(access) {
		t.Error("code replay left the access token valid")
	}
	_, _, err = e.refreshWith(i.clientID, refresh)
	wantInvalidGrant(t, err, "refresh after code replay")
}

func TestExchangeEnforcesPKCEAndRedirect(t *testing.T) {
	e := newEnv(t)
	clientID := e.register()

	i := e.authorize(clientID, "alice", nil)
	i.verifier = strings.Repeat("x", 52)
	if _, _, err := e.exchange(i); err == nil {
		t.Error("wrong verifier accepted")
	}

	// The right verifier with a different redirect_uri must still fail.
	i = e.authorize(clientID, "alice", nil)
	_, _, err := e.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {i.code}, "code_verifier": {i.verifier},
		"client_id": {clientID}, "redirect_uri": {"http://localhost:9999/other"},
	})
	if err == nil {
		t.Error("mismatched redirect_uri accepted")
	}

	// A code issued to one client can't be exchanged by another.
	i = e.authorize(clientID, "alice", nil)
	i.clientID = e.register()
	wantInvalidGrant(t, func() error { _, _, err := e.exchange(i); return err }(), "another client's code")
}

func TestRevocation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token func(issued) string
		hint  string
	}{
		{"refresh", func(i issued) string { return i.refresh }, "refresh_token"},
		{"access", func(i issued) string { return i.access }, "access_token"},
		{"no hint", func(i issued) string { return i.refresh }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			i := e.login("alice")
			other := e.loginClient(i.clientID, "alice")
			if err := e.revoke(i.clientID, tc.token(i), tc.hint); err != nil {
				t.Fatalf("revoke: %v", describe(err))
			}
			e.reopen()
			if e.accessValid(i.access) {
				t.Error("access token valid after revocation")
			}
			_, _, err := e.refreshWith(i.clientID, i.refresh)
			wantInvalidGrant(t, err, "refresh after revocation")
			if !e.accessValid(other.access) {
				t.Error("revocation hit another grant")
			}
		})
	}
}

func TestRevocationEdgeCases(t *testing.T) {
	e := newEnv(t)
	i := e.login("alice")
	// RFC 7009: an unknown token is not an error.
	if err := e.revoke(i.clientID, "not.a-token", ""); err != nil {
		t.Errorf("unknown token: %v", describe(err))
	}
	// Another client can't revoke it.
	if err := e.revoke(e.register(), i.access, "access_token"); err == nil {
		t.Error("another client revoked the token")
	}
	if !e.accessValid(i.access) {
		t.Error("the failed revocation had an effect")
	}
	// Revoking twice is fine.
	for range 2 {
		if err := e.revoke(i.clientID, i.refresh, "refresh_token"); err != nil {
			t.Errorf("revoke: %v", describe(err))
		}
	}
}

func TestBumpGenerationInvalidatesOnlyThatUser(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	alice, bob := e.login("alice"), e.login("bob")
	pending := e.authorize(alice.clientID, "alice", nil)

	if _, err := e.s.BumpGeneration(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	if e.accessValid(alice.access) {
		t.Error("alice's access token survived her revoke")
	}
	_, _, err := e.refreshWith(alice.clientID, alice.refresh)
	wantInvalidGrant(t, err, "alice's refresh token after revoke")
	_, _, err = e.exchange(pending)
	wantInvalidGrant(t, err, "alice's code after revoke")

	e.reopen() // the revoke persists, and bob's tokens still work
	if e.accessValid(alice.access) {
		t.Error("alice's access token valid after reopen")
	}
	if !e.accessValid(bob.access) {
		t.Error("bob's access token was affected")
	}
	if _, _, err := e.refreshWith(bob.clientID, bob.refresh); err != nil {
		t.Errorf("bob's refresh: %v", describe(err))
	}

	// Signing in again issues tokens at the new generation.
	again := e.login("alice")
	if !e.accessValid(again.access) {
		t.Error("alice's new sign-in is not valid")
	}
}

func TestBumpAllUsers(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	alice, bob := e.login("alice"), e.login("bob")
	if _, err := e.s.BumpGeneration(ctx, AllUsers); err != nil {
		t.Fatal(err)
	}
	if e.accessValid(alice.access) || e.accessValid(bob.access) {
		t.Error("a token survived revoke --all")
	}
	if !e.accessValid(e.login("alice").access) {
		t.Error("a new sign-in after revoke --all is not valid")
	}
}

// finbot revoke runs in another process: its write must apply with no restart.
func TestRevokeFromAnotherHandleAppliesAtOnce(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	i := e.login("alice")
	first := e.db
	firstProvider := e.provider
	e.open() // a second handle on the same file
	defer first.Close()
	if _, err := e.s.BumpGeneration(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	tu, _, err := firstProvider.IntrospectToken(ctx, i.access, fosite.AccessToken, &fosite.DefaultSession{})
	if err == nil && tu == fosite.AccessToken {
		t.Fatal("the first handle still accepts the revoked token")
	}
}

func TestNoSecretsInLogsErrorsOrDatabase(t *testing.T) {
	var logs bytes.Buffer
	oldLog, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	oldSlog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		log.SetOutput(oldLog)
		log.SetFlags(oldFlags)
		slog.SetDefault(oldSlog)
	})

	e := newEnv(t)
	ctx := context.Background()
	const planted = "PLANTEDRAWVALUE123456"
	// fosite's default sanitation list keeps "code", so a request carrying one
	// must not get it into the database.
	i := e.authorize(e.register(), "alice", url.Values{"code": {planted}, "refresh_token": {planted}})
	code, verifier := i.code, i.verifier
	access, refresh, err := e.exchange(i)
	if err != nil {
		t.Fatalf("exchange: %v", describe(err))
	}
	access2, refresh2, err := e.refreshWith(i.clientID, refresh)
	if err != nil {
		t.Fatalf("refresh: %v", describe(err))
	}

	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, describe(err))
		}
	}
	// Every failure path reachable with the raw values in hand.
	_, _, err = e.exchange(i)
	note(err)
	_, _, err = e.refreshWith(i.clientID, refresh)
	note(err)
	_, _, err = e.refreshWith(i.clientID, access)
	note(err)
	_, _, err = e.refreshWith("no-such-client", refresh2)
	note(err)
	_, _, err = e.provider.IntrospectToken(ctx, access, fosite.AccessToken, &fosite.DefaultSession{})
	note(err)
	note(e.revoke("no-such-client", refresh2, ""))

	// The database, read in full before it's closed.
	var dbBytes []byte
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(e.path + suffix)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		dbBytes = append(dbBytes, b...)
	}

	// Storage errors with the database gone.
	sig := "sig"
	e.db.Close()
	_, err = e.s.GetAccessTokenSession(ctx, sig, nil)
	note(err)
	_, err = e.s.GetRefreshTokenSession(ctx, sig, nil)
	note(err)
	_, err = e.s.GetAuthorizeCodeSession(ctx, sig, nil)
	note(err)
	_, err = e.s.HMACSecret(ctx)
	note(err)
	note(e.s.CreateAccessTokenSession(ctx, sig, fosite.NewRequest()))
	_, _, err = e.refreshWith(i.clientID, refresh2)
	note(err)
	e.open() // for cleanup

	if len(errs) < 8 {
		t.Fatalf("only %d failure paths were exercised", len(errs))
	}
	raw := map[string]string{
		"code": code, "verifier": verifier, "access": access, "refresh": refresh,
		"access2": access2, "refresh2": refresh2,
	}
	// A token is "<random>.<signature>"; the random half must not leak either.
	for name, v := range map[string]string{"access": access, "refresh": refresh, "code": code} {
		raw[name+" key"], _, _ = strings.Cut(v, ".")
	}
	secretForms := map[string]string{
		"secret raw": string(e.secret), "secret b64": base64.StdEncoding.EncodeToString(e.secret),
		"secret b64url": base64.RawURLEncoding.EncodeToString(e.secret), "secret hex": hex.EncodeToString(e.secret),
	}
	joined := strings.Join(errs, "\n")
	for _, where := range []struct{ name, text string }{{"logs", logs.String()}, {"errors", joined}} {
		for name, v := range raw {
			if strings.Contains(where.text, v) {
				t.Errorf("%s contain the raw %s", where.name, name)
			}
		}
		for name, v := range secretForms {
			if strings.Contains(where.text, v) {
				t.Errorf("%s contain the %s", where.name, name)
			}
		}
	}
	raw["planted"] = planted
	for name, v := range raw {
		if bytes.Contains(dbBytes, []byte(v)) {
			t.Errorf("the database contains the raw %s", name)
		}
	}
	if !bytes.Contains(dbBytes, []byte("oauth_access_tokens")) {
		t.Error("database scan read nothing useful")
	}
}
