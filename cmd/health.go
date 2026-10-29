package cmd

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// Health tracks the last successful sync for GET /healthz. It reports
// healthy only while that sync is younger than StaleAfter (2×SYNC_INTERVAL).
type Health struct {
	StaleAfter time.Duration
	Now        func() time.Time // defaults to time.Now

	mu      sync.Mutex
	lastOK  time.Time
	lastErr string
}

func (h *Health) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// OK records a successful sync and clears the last error.
func (h *Health) OK() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastOK = h.now()
	h.lastErr = ""
}

// Fail records a failed (or skipped) sync. The last success stands until it
// goes stale.
func (h *Health) Fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastErr = redact(err.Error())
}

type healthBody struct {
	LastSyncOK string `json:"last_sync_ok"`
	LastError  string `json:"last_error"`
}

// ServeHTTP answers 200 while the last success is fresh, otherwise 503.
// The body is the same JSON either way.
func (h *Health) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.mu.Lock()
	body := healthBody{LastError: h.lastErr}
	healthy := !h.lastOK.IsZero() && h.now().Sub(h.lastOK) <= h.StaleAfter
	if !h.lastOK.IsZero() {
		body.LastSyncOK = h.lastOK.UTC().Format(time.RFC3339)
	}
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(body)
}

var uuidRE = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// redact strips any id that slipped past the ynab package's own redaction
// before an error is shown on an unauthenticated endpoint.
func redact(s string) string { return uuidRE.ReplaceAllString(s, "<id>") }
