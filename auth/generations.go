package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ory/fosite"
)

const (
	// AllUsers is the user whose generation applies to everyone (revoke --all).
	AllUsers = "*"

	// GenKey is the session Extra key holding the generation at issue time.
	GenKey = "gen"

	secretLen = 32
)

// ErrRevoked means a session was issued before its user's last revoke, or
// doesn't say which generation it belongs to.
var ErrRevoked = errors.New("auth: token revoked")

// NewSession returns the session for a sign-in by user at generation gen.
func NewSession(user, name string, gen int) *fosite.DefaultSession {
	return &fosite.DefaultSession{Subject: user, Username: name, Extra: map[string]any{GenKey: gen}}
}

// CurrentGeneration returns the user's generation plus the global one. It reads
// the database every time, so a revoke by another process applies at once.
func (s *Storage) CurrentGeneration(ctx context.Context, user string) (int, error) {
	var gen int
	err := s.reader(ctx).QueryRowContext(ctx,
		`SELECT COALESCE(SUM(generation), 0) FROM token_generations WHERE user IN (?, ?)`, user, AllUsers).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("auth: read generation: %w", err)
	}
	return gen, nil
}

// BumpGeneration invalidates all of a user's tokens (or everyone's, for
// AllUsers) in one step, and returns that user's own new counter.
func (s *Storage) BumpGeneration(ctx context.Context, user string) (int, error) {
	if user == "" {
		return 0, errors.New("auth: bump generation: empty user")
	}
	var gen int
	err := s.writer(ctx).QueryRowContext(ctx,
		`INSERT INTO token_generations (user, generation) VALUES (?, 1)
		 ON CONFLICT (user) DO UPDATE SET generation = generation + 1
		 RETURNING generation`, user).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("auth: bump generation: %w", err)
	}
	return gen, nil
}

// CheckSession returns ErrRevoked unless the session names a user and carries
// that user's current generation. Every Get of a stored session runs it, so
// fosite sees a revoked token as not found.
func (s *Storage) CheckSession(ctx context.Context, sess *fosite.DefaultSession) error {
	if sess == nil || sess.Subject == "" {
		return ErrRevoked
	}
	var issued int
	switch v := sess.Extra[GenKey].(type) {
	case int: // a session not yet stored
		issued = v
	case float64: // after a JSON round trip
		if v != float64(int(v)) {
			return ErrRevoked
		}
		issued = int(v)
	default: // missing or non-numeric: fail closed
		return ErrRevoked
	}
	cur, err := s.CurrentGeneration(ctx, sess.Subject)
	if err != nil {
		return err
	}
	if issued != cur {
		return ErrRevoked
	}
	return nil
}

// HMACSecret returns fosite's global secret, generating it on first use. It
// survives restarts, so a redeploy doesn't sign anyone out. Never log it.
func (s *Storage) HMACSecret(ctx context.Context) ([]byte, error) {
	secret, err := s.readSecret(ctx)
	if err == nil {
		return secret, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	fresh := make([]byte, secretLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("auth: generate secret: %w", err)
	}
	// If another process got here first, its secret wins.
	if _, err := s.writer(ctx).ExecContext(ctx,
		`INSERT INTO oauth_hmac_secret (id, secret) VALUES (1, ?) ON CONFLICT (id) DO NOTHING`, fresh); err != nil {
		return nil, fmt.Errorf("auth: store secret: %w", err)
	}
	return s.readSecret(ctx)
}

func (s *Storage) readSecret(ctx context.Context) ([]byte, error) {
	var secret []byte
	err := s.reader(ctx).QueryRowContext(ctx, `SELECT secret FROM oauth_hmac_secret WHERE id = 1`).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read secret: %w", err)
	}
	return secret, nil
}
