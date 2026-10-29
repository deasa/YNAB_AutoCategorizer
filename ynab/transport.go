package ynab

import (
	"bytes"
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

// governedTransport takes a governor token for every HTTP attempt the
// library makes (retries included) and normalizes zone-less timestamps in
// each response body before the library decodes it.
type governedTransport struct {
	base http.RoundTripper
	gov  *Governor
}

func (t *governedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	if err := t.gov.Take(write); err != nil {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, err
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	body = normalizeTimestamps(body)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}
