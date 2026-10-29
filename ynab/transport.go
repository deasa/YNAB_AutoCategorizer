package ynab

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
)

// naiveTimestamp matches the full contents of a JSON string that is an
// RFC 3339-like date-time with no zone designator, e.g.
// "2025-08-28T13:55:15.492" (spike F1: goal_snoozed_at arrives like this).
var naiveTimestamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?$`)

// normalizeTimestamps rewrites every zone-less timestamp found in a JSON
// string value of body to UTC by appending "Z". Object keys, zoned values,
// date-only values and other strings are left untouched. It scans the JSON
// lexically, so a timestamp-shaped run of bytes inside a longer string is
// never altered. body is returned as is when nothing changes.
func normalizeTimestamps(body []byte) []byte {
	var out []byte // allocated on the first rewrite
	copied := 0
	for i := 0; i < len(body); i++ {
		if body[i] != '"' {
			continue
		}
		start := i + 1
		end := start
		for end < len(body) && body[end] != '"' {
			if body[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(body) {
			break // unterminated string: leave the rest for the decoder to reject
		}
		i = end
		if !naiveTimestamp.Match(body[start:end]) || isKey(body, end+1) {
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(body)+64)
		}
		out = append(out, body[copied:end]...)
		out = append(out, 'Z')
		copied = end
	}
	if out == nil {
		return body
	}
	return append(out, body[copied:]...)
}

// isKey reports whether the next non-space byte at or after i is a colon,
// i.e. the string that just closed was an object key.
func isKey(body []byte, i int) bool {
	for ; i < len(body); i++ {
		switch body[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case ':':
			return true
		}
		return false
	}
	return false
}

// maxResponseBytes caps how much of a response body the transport reads,
// matching the library's own guard (which runs only after this transport
// has buffered the body). A var so tests can lower it.
var maxResponseBytes int64 = 128 << 20

// normalizingTransport normalizes zone-less timestamps in each response body
// before the library decodes it. Rate budgeting is not here but in
// governorLimiter, which the library runs before every attempt.
type normalizingTransport struct {
	base http.RoundTripper
}

func (t *normalizingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("ynab: response body exceeds %d bytes", maxResponseBytes)
	}
	body = normalizeTimestamps(body)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}

type writeKey struct{}

// asWrite marks ctx as carrying a write request, so governorLimiter lets it
// spend the write reserve. Unmarked requests count as reads.
func asWrite(ctx context.Context) context.Context { return context.WithValue(ctx, writeKey{}, true) }

func isWrite(ctx context.Context) bool { w, _ := ctx.Value(writeKey{}).(bool); return w }

// governorLimiter takes a governor token for every HTTP attempt the library
// makes, retries included. The library treats a limiter error as fatal for
// the call, so an exhausted budget ends it at once instead of sleeping a
// retry backoff first.
type governorLimiter struct {
	gov *Governor
}

func (l governorLimiter) Wait(ctx context.Context) error { return l.gov.Take(isWrite(ctx)) }
