package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	googleIssuer = "https://accounts.google.com"
	oidcTimeout  = 10 * time.Second
)

// Identity is what finbot learns about a user from the upstream login.
type Identity struct {
	Email         string
	EmailVerified bool
	Name          string
}

// IdentityProvider is the upstream login.
type IdentityProvider interface {
	// AuthCodeURL is where to send the browser to sign in.
	AuthCodeURL(state, pkceVerifier, nonce string) string
	// Exchange redeems the callback code and returns the verified identity.
	Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, error)
}

// GoogleProvider signs users in with Google (OIDC authorization code + PKCE).
// Only the ID token is used; Google's access and refresh tokens are discarded.
type GoogleProvider struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

// NewGoogleProvider runs OIDC discovery against Google.
func NewGoogleProvider(ctx context.Context, clientID, clientSecret, redirectURL string) (*GoogleProvider, error) {
	return newOIDCProvider(ctx, googleIssuer, clientID, clientSecret, redirectURL)
}

func newOIDCProvider(ctx context.Context, issuer, clientID, clientSecret, redirectURL string) (*GoogleProvider, error) {
	client := &http.Client{Timeout: oidcTimeout}
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc discovery: %w", err)
	}
	endpoint := p.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	return &GoogleProvider{
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     endpoint,
			RedirectURL:  redirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier: p.Verifier(&oidc.Config{ClientID: clientID}),
		client:   client,
	}, nil
}

// AuthCodeURL implements IdentityProvider.
func (g *GoogleProvider) AuthCodeURL(state, pkceVerifier, nonce string) string {
	return g.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(pkceVerifier), oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("prompt", "select_account"))
}

// Exchange implements IdentityProvider. It checks the ID token's signature,
// issuer, audience, expiry and nonce.
func (g *GoogleProvider) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, error) {
	ctx = oidc.ClientContext(ctx, g.client)
	tok, err := g.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, fmt.Errorf("auth: oidc code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return Identity{}, errors.New("auth: oidc response has no id_token")
	}
	idt, err := g.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: verify id_token: %w", err)
	}
	if idt.Nonce != nonce {
		return Identity{}, errors.New("auth: id_token nonce mismatch")
	}
	var c struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&c); err != nil {
		return Identity{}, fmt.Errorf("auth: id_token claims: %w", err)
	}
	return Identity{Email: c.Email, EmailVerified: c.EmailVerified, Name: c.Name}, nil
}
