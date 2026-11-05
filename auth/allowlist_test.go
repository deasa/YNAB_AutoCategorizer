package auth

import (
	"errors"
	"regexp"
	"testing"
)

func TestAllowlistCheck(t *testing.T) {
	a := ParseAllowlist(" Alice@Example.com , ,bob@example.com")
	if len(a) != 2 {
		t.Fatalf("parsed %d emails, want 2", len(a))
	}
	for _, tc := range []struct {
		email    string
		verified bool
		want     error
	}{
		{"alice@example.com", true, nil},
		{"ALICE@example.com ", true, nil},
		{"alice@example.com", false, ErrEmailUnverified},
		{"mallory@example.com", true, ErrNotAllowlisted},
		{"", true, ErrNotAllowlisted},
	} {
		if err := a.Check(tc.email, tc.verified); !errors.Is(err, tc.want) {
			t.Errorf("Check(%q, %v) = %v, want %v", tc.email, tc.verified, err, tc.want)
		}
	}
	if err := ParseAllowlist("").Check("alice@example.com", true); !errors.Is(err, ErrNotAllowlisted) {
		t.Errorf("empty allowlist: %v", err)
	}
}

func TestUserID(t *testing.T) {
	id := UserID("Alice@Example.com ")
	if !regexp.MustCompile(`^u_[0-9a-f]{12}$`).MatchString(id) {
		t.Errorf("UserID = %q, want u_ and 12 hex digits", id)
	}
	if id != UserID("alice@example.com") {
		t.Error("UserID differs across normalization")
	}
	if id == UserID("bob@example.com") {
		t.Error("UserID collides")
	}
}
