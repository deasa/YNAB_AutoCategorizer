package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Identity is what finbot learns about a user from the upstream login.
type Identity struct {
	Email         string
	EmailVerified bool
	Name          string
}

// IdentityProvider is the upstream login (Google). It is an interface so the
// authorization server can be tested without Google.
type IdentityProvider interface {
	// AuthCodeURL is where to send the browser to sign in.
	AuthCodeURL(state, pkceVerifier, nonce string) string
	// Exchange redeems the callback code and returns the verified identity.
	Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, error)
}

// GoogleProvider signs users in with Google (OIDC authorization code + PKCE).
// Only the ID token is used; Google access/refresh tokens are discarded.
type GoogleProvider struct {
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func NewGoogleProvider(ctx context.Context, clientID, clientSecret, redirectURL string) (*GoogleProvider, error) {
	p, err := oidc.NewProvider(ctx, "https://accounts.google.com")
	if err != nil {
		return nil, fmt.Errorf("google discovery: %w", err)
	}
	return &GoogleProvider{
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     google.Endpoint,
			RedirectURL:  redirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier: p.Verifier(&oidc.Config{ClientID: clientID}),
	}, nil
}

func (g *GoogleProvider) AuthCodeURL(state, pkceVerifier, nonce string) string {
	return g.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(pkceVerifier), oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("prompt", "select_account"))
}

func (g *GoogleProvider) Exchange(ctx context.Context, code, pkceVerifier, nonce string) (Identity, error) {
	tok, err := g.oauth.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return Identity{}, fmt.Errorf("google code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return Identity{}, errors.New("google response has no id_token")
	}
	idt, err := g.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("verify google id_token: %w", err)
	}
	if idt.Nonce != nonce {
		return Identity{}, errors.New("google id_token nonce mismatch")
	}
	var c struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&c); err != nil {
		return Identity{}, fmt.Errorf("google id_token claims: %w", err)
	}
	return Identity{Email: c.Email, EmailVerified: c.EmailVerified, Name: c.Name}, nil
}
