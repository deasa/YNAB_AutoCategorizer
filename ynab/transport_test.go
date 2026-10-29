package ynab

import (
	"io"
	"net/http"
	"strings"
	"testing"
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

func TestNormalizingTransport(t *testing.T) {
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"t":"2025-08-28T13:55:15.492"}`)),
		}, nil
	})
	tr := &normalizingTransport{base: base}

	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != `{"t":"2025-08-28T13:55:15.492Z"}` || resp.ContentLength != int64(len(b)) {
		t.Errorf("body %s, content length %d", b, resp.ContentLength)
	}
}

// A body over maxResponseBytes is refused, and the transport reads no more
// than one byte past the cap.
func TestNormalizingTransport_CapsBody(t *testing.T) {
	defer func(old int64) { maxResponseBytes = old }(maxResponseBytes)
	maxResponseBytes = 16

	for _, tc := range []struct {
		size    int
		wantErr bool
	}{{16, false}, {17, true}, {1 << 20, true}} {
		body := &countingReader{r: strings.NewReader(strings.Repeat("x", tc.size))}
		tr := &normalizingTransport{base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(body)}, nil
		})}
		req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/x", nil)
		_, err := tr.RoundTrip(req)
		if (err != nil) != tc.wantErr {
			t.Errorf("size %d: err = %v, want error %v", tc.size, err, tc.wantErr)
		}
		if body.n > 17 {
			t.Errorf("size %d: read %d bytes, want at most cap+1", tc.size, body.n)
		}
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}
