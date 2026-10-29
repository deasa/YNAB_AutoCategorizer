package ynab

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTimestamps(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "goal_snoozed_at sample gains Z",
			in:   `{"goal_snoozed_at":"2025-08-28T13:55:15.492","name":"x"}`,
			want: `{"goal_snoozed_at":"2025-08-28T13:55:15.492Z","name":"x"}`,
		},
		{
			name: "any key, no fraction, whitespace and arrays",
			in:   `{"other_at" : "2025-08-28T13:55:15", "list":["2025-01-02T03:04:05.1"]}`,
			want: `{"other_at" : "2025-08-28T13:55:15Z", "list":["2025-01-02T03:04:05.1Z"]}`,
		},
		{
			name: "zoned values untouched",
			in:   `{"a":"2025-08-28T13:55:15.492Z","b":"2025-08-28T13:55:15+02:00","c":"2025-08-28T13:55:15.1-07:00"}`,
			want: `{"a":"2025-08-28T13:55:15.492Z","b":"2025-08-28T13:55:15+02:00","c":"2025-08-28T13:55:15.1-07:00"}`,
		},
		{
			name: "date-only untouched",
			in:   `{"date":"2026-10-29","month":"2026-10-01"}`,
			want: `{"date":"2026-10-29","month":"2026-10-01"}`,
		},
		{
			name: "non-timestamp strings untouched",
			in:   `{"memo":"paid 2025-08-28T13:55:15 cash","payee":"T13:55","n":12}`,
			want: `{"memo":"paid 2025-08-28T13:55:15 cash","payee":"T13:55","n":12}`,
		},
		{
			name: "object key untouched",
			in:   `{"2025-08-28T13:55:15":"2025-08-28T13:55:15"}`,
			want: `{"2025-08-28T13:55:15":"2025-08-28T13:55:15Z"}`,
		},
		{
			name: "escaped quotes don't desync the scan",
			in:   `{"memo":"say \"hi\" 2025-08-28T13:55:15","t":"2025-08-28T13:55:15"}`,
			want: `{"memo":"say \"hi\" 2025-08-28T13:55:15","t":"2025-08-28T13:55:15Z"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(normalizeTimestamps([]byte(tt.in))); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGovernedTransport_NormalizesAndCounts(t *testing.T) {
	sent := 0
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"t":"2025-08-28T13:55:15.492"}`)),
		}, nil
	})
	clock := &fakeClock{t: time.Unix(0, 0)}
	gov := NewGovernor(2, 1, clock.Now)
	tr := &governedTransport{base: base, gov: gov}

	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"t":"2025-08-28T13:55:15.492Z"}` || resp.ContentLength != int64(len(b)) {
		t.Errorf("body %s, content length %d", b, resp.ContentLength)
	}

	// The one remaining token is the write reserve: a second GET is refused
	// before it reaches the network.
	if _, err := tr.RoundTrip(req); err == nil {
		t.Fatal("second read should be refused")
	}
	if sent != 1 {
		t.Errorf("sent %d requests, want 1", sent)
	}
}
