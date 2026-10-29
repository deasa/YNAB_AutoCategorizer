package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// pingTimeout bounds each dead-man ping.
const pingTimeout = 10 * time.Second

// Pinger sends the dead-man ping (HEALTHCHECK_PING_URL) after each sync tick:
// URL after a success, URL/fail after a failure, nothing after a skipped
// tick. An empty URL disables it. A ping failure is only logged. The URL is a
// secret, so it is scrubbed from everything logged.
type Pinger struct {
	URL    string
	Client *http.Client // defaults to http.DefaultClient
	Log    *slog.Logger
}

// After pings for one sync tick's outcome.
func (p *Pinger) After(ctx context.Context, syncErr error) {
	if p == nil || p.URL == "" || errors.Is(syncErr, errSkipped) {
		return
	}
	target, outcome := p.URL, "ok"
	if syncErr != nil {
		outcome = "fail"
		u, err := url.Parse(p.URL)
		if err != nil {
			p.Log.Warn("healthcheck ping failed", "outcome", outcome, "err", p.scrub(err.Error()))
			return
		}
		target = u.JoinPath("fail").String() // keeps any query after the path
	}
	if err := p.get(ctx, target); err != nil {
		p.Log.Warn("healthcheck ping failed", "outcome", outcome, "err", p.scrub(err.Error()))
	}
}

func (p *Pinger) get(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ping: status %d", resp.StatusCode)
	}
	return nil
}

// scrub strips the URL (with or without a trailing slash) from s.
func (p *Pinger) scrub(s string) string {
	return scrub(s, p.URL, strings.TrimRight(p.URL, "/"))
}
