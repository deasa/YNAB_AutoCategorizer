package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// maxResponse caps how much of a Slack response is read.
const maxResponse = 8 << 20

// apiError is Slack's {"ok":false,"error":code}. The code is safe to log.
type apiError struct{ code string }

func (e *apiError) Error() string { return "slack: " + e.code }

// statusError is a non-200 HTTP response.
type statusError struct {
	method string
	code   int
}

func (e *statusError) Error() string { return fmt.Sprintf("slack: %s: status %d", e.method, e.code) }

// notPosted reports whether err proves Slack did not create the message: it
// answered ok:false or a 4xx. After a 5xx, a timeout or an unreadable response
// the message may exist.
func notPosted(err error) bool {
	var ae *apiError
	var se *statusError
	return errors.As(err, &ae) || (errors.As(err, &se) && se.code >= 400 && se.code < 500)
}

// isCode reports whether err is a Slack API error with one of codes.
func isCode(err error, codes ...string) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.code == c {
			return true
		}
	}
	return false
}

// slack is a minimal Slack Web API client. The token only ever travels in the
// Authorization header.
type slack struct {
	token   string
	baseURL string
	client  *http.Client
}

// do sends req, and decodes a 200 {"ok":true} response into out.
func (s *slack) do(req *http.Request, method string, out any) error {
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		// url.Error repeats the URL, which holds no secret; keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &statusError{method: method, code: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("slack: %s: read: %w", method, err)
	}
	var ok struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &ok); err != nil {
		return fmt.Errorf("slack: %s: malformed response", method)
	}
	if !ok.OK {
		return &apiError{code: ok.Error}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("slack: %s: malformed response", method)
		}
	}
	return nil
}

// findPrivateChannel returns the id of the private channel called name that
// the bot is a member of.
func (s *slack) findPrivateChannel(ctx context.Context, name string) (string, error) {
	cursor := ""
	for {
		q := url.Values{"types": {"private_channel"}, "exclude_archived": {"true"}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/users.conversations?"+q.Encode(), nil)
		if err != nil {
			return "", fmt.Errorf("slack: users.conversations: %w", err)
		}
		var page struct {
			Channels []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"channels"`
			Meta struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := s.do(req, "users.conversations", &page); err != nil {
			return "", err
		}
		for _, c := range page.Channels {
			if c.Name == name {
				return c.ID, nil
			}
		}
		if page.Meta.NextCursor == "" {
			return "", fmt.Errorf("slack: the bot is not in a private channel named %q", name)
		}
		cursor = page.Meta.NextCursor
	}
}

// send calls chat.postMessage (or chat.update when ts is set) and returns the
// message's ts. parse is explicit because the two methods have different
// defaults: chat.update's is client, and omitting it there overwrites the value.
func (s *slack) send(ctx context.Context, channel, ts, text string) (string, error) {
	method := "chat.postMessage"
	payload := map[string]string{"channel": channel, "text": text, "parse": "none"}
	if ts != "" {
		method = "chat.update"
		payload["ts"] = ts
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("slack: %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("slack: %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	var out struct {
		TS string `json:"ts"`
	}
	if err := s.do(req, method, &out); err != nil {
		return "", err
	}
	if out.TS == "" {
		return "", fmt.Errorf("slack: %s: response has no ts", method)
	}
	return out.TS, nil
}
