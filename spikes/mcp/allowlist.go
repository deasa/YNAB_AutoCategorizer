package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Errors returned by Allowlist.Check. Messages are shown to the user, so they
// never contain the email address itself.
var (
	ErrEmailUnverified = errors.New("google account email is not verified")
	ErrNotAllowlisted  = errors.New("google account is not allowed to use this server")
)

// Allowlist holds the normalized email addresses allowed to sign in.
type Allowlist map[string]struct{}

// ParseAllowlist parses a comma-separated list of emails (MCP_ALLOWED_EMAILS).
func ParseAllowlist(s string) Allowlist {
	a := Allowlist{}
	for _, e := range strings.Split(s, ",") {
		if e = normalizeEmail(e); e != "" {
			a[e] = struct{}{}
		}
	}
	return a
}

// Check admits only a verified email that is on the list.
func (a Allowlist) Check(email string, verified bool) error {
	if !verified {
		return ErrEmailUnverified
	}
	if _, ok := a[normalizeEmail(email)]; !ok {
		return ErrNotAllowlisted
	}
	return nil
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// UserID is the stable, log-safe identifier for a user: a short hash of the
// normalized email. Raw emails are never logged or returned to clients.
func UserID(email string) string {
	sum := sha256.Sum256([]byte(normalizeEmail(email)))
	return "u_" + hex.EncodeToString(sum[:6])
}
