package main

import (
	"errors"
	"testing"
)

func TestAllowlistCheck(t *testing.T) {
	a := ParseAllowlist(" Alice@Example.com , ,bob@example.com")
	if len(a) != 2 {
		t.Fatalf("parsed %d entries, want 2", len(a))
	}
	tests := []struct {
		name     string
		email    string
		verified bool
		want     error
	}{
		{"allowed", "alice@example.com", true, nil},
		{"case and space insensitive", "  ALICE@example.COM ", true, nil},
		{"unverified allowed email", "alice@example.com", false, ErrEmailUnverified},
		{"not on list", "mallory@example.com", true, ErrNotAllowlisted},
		{"empty email", "", true, ErrNotAllowlisted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := a.Check(tt.email, tt.verified); !errors.Is(err, tt.want) {
				t.Errorf("Check(%q, %v) = %v, want %v", tt.email, tt.verified, err, tt.want)
			}
		})
	}
}

func TestEmptyAllowlistRejectsEveryone(t *testing.T) {
	if err := ParseAllowlist("").Check("alice@example.com", true); !errors.Is(err, ErrNotAllowlisted) {
		t.Fatalf("got %v, want ErrNotAllowlisted", err)
	}
}

func TestUserIDStableAndOpaque(t *testing.T) {
	a, b := UserID("Alice@Example.com"), UserID(" alice@example.com")
	if a != b {
		t.Fatalf("UserID not normalized: %s vs %s", a, b)
	}
	if len(a) != 14 || a == UserID("bob@example.com") {
		t.Fatalf("unexpected UserID %q", a)
	}
}
