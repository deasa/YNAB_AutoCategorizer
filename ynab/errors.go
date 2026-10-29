package ynab

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	ynabapi "pkg.venceslau.dev/ynab"
)

// ErrRateLimited reports that YNAB answered 429. Errors carrying it are
// *RateLimitError, which also matches the library's ynabapi.ErrRateLimited.
var ErrRateLimited = errors.New("ynab: rate limited (429)")

// RateLimitError is YNAB's 429. RetryAfter is the server-requested wait;
// zero means unknown, never "retry now".
type RateLimitError struct {
	Op         string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s: %v (retry after %s)", e.Op, ErrRateLimited, e.RetryAfter)
	}
	return fmt.Sprintf("%s: %v", e.Op, ErrRateLimited)
}

func (e *RateLimitError) Is(target error) bool {
	return target == ErrRateLimited || target == ynabapi.ErrRateLimited
}

// uuidRE matches any YNAB-style UUID (plan, account, category, transaction).
var uuidRE = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// redactor scrubs secrets and ids from error text.
type redactor struct {
	planID string
	token  string
}

// redact replaces the token with {token}, the plan id with {plan} and every
// other UUID (account, category, transaction ids) with {id}.
func (r redactor) redact(s string) string {
	if r.token != "" {
		s = strings.ReplaceAll(s, r.token, "{token}")
	}
	if r.planID != "" {
		s = strings.ReplaceAll(s, r.planID, "{plan}")
	}
	return uuidRE.ReplaceAllString(s, "{id}")
}

// wrap turns a library error into one safe to log: a 429 becomes a
// *RateLimitError, anything else a redactedError. The original error stays
// reachable through errors.Is (so ErrBudgetExhausted, ynabapi.ErrNotFound and
// friends still match) but deliberately not through Unwrap or errors.As,
// since its text holds the plan id (spike F2).
func (r redactor) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ynabapi.ErrRateLimited) {
		rl := &RateLimitError{Op: r.redact(op)}
		var apiErr *ynabapi.Error
		if errors.As(err, &apiErr) {
			rl.RetryAfter = apiErr.RetryAfter
		}
		return rl
	}
	return &redactedError{msg: r.redact(op + ": " + err.Error()), orig: err}
}

type redactedError struct {
	msg  string
	orig error
}

func (e *redactedError) Error() string { return e.msg }

func (e *redactedError) Is(target error) bool { return errors.Is(e.orig, target) }
