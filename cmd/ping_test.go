package cmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/config"
)

// pingServer records the paths it was asked for and answers with status.
type pingServer struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

func newPingServer(t *testing.T, status int) *pingServer {
	ps := &pingServer{}
	ps.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps.mu.Lock()
		ps.paths = append(ps.paths, r.Method+" "+r.URL.Path)
		ps.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(ps.Close)
	return ps
}

func (ps *pingServer) got() []string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return append([]string(nil), ps.paths...)
}

func TestPinger_Outcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		syncErr error
		want    []string
	}{
		{"success", nil, []string{"GET /ping/check-key"}},
		{"failure", errors.New("boom"), []string{"GET /ping/check-key/fail"}},
		{"skipped", errSkipped, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := newPingServer(t, http.StatusOK)
			var logs bytes.Buffer
			p := &Pinger{URL: ps.URL + "/ping/check-key", Log: slog.New(slog.NewJSONHandler(&logs, nil))}
			p.After(context.Background(), tc.syncErr)
			if got := ps.got(); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("requests = %q, want %q", got, tc.want)
			}
			if logs.Len() != 0 {
				t.Errorf("unexpected log: %s", logs.String())
			}
		})
	}
}

func TestPinger_FailKeepsQuery(t *testing.T) {
	var got string
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.URL.RequestURI() }))
	t.Cleanup(ps.Close)
	for _, u := range []string{ps.URL + "/ping/key?rid=x", ps.URL + "/ping/key/?rid=x"} {
		p := &Pinger{URL: u, Log: slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))}
		p.After(context.Background(), errors.New("boom"))
		if got != "/ping/key/fail?rid=x" {
			t.Errorf("%s: fail ping went to %q", u, got)
		}
	}
}

func TestPinger_DisabledSendsNothing(t *testing.T) {
	var p *Pinger
	p.After(context.Background(), nil) // nil pinger is a no-op
	p = &Pinger{Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("disabled pinger sent a request")
		return nil, nil
	})}}
	p.After(context.Background(), nil)
	p.After(context.Background(), errors.New("boom"))
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPinger_FailureOnlyLogsAndScrubsURL(t *testing.T) {
	ps := newPingServer(t, http.StatusOK)
	ps.Close() // connection refused: the error carries the URL
	for _, url := range []string{ps.URL + "/ping/s3cret-key", ps.URL + "/ping/s3cret-key/"} {
		var logs bytes.Buffer
		p := &Pinger{URL: url, Log: slog.New(slog.NewJSONHandler(&logs, nil))}
		p.After(context.Background(), nil)
		p.After(context.Background(), errors.New("boom"))
		out := logs.String()
		if strings.Count(out, `"msg":"healthcheck ping failed"`) != 2 {
			t.Fatalf("want two failure logs, got: %s", out)
		}
		if strings.Contains(out, "s3cret-key") || strings.Contains(out, ps.URL) {
			t.Errorf("ping URL leaked into log: %s", out)
		}
	}
}

func TestPinger_Non2xxLogs(t *testing.T) {
	ps := newPingServer(t, http.StatusNotFound)
	var logs bytes.Buffer
	p := &Pinger{URL: ps.URL + "/ping/key", Log: slog.New(slog.NewJSONHandler(&logs, nil))}
	p.After(context.Background(), nil)
	if !strings.Contains(logs.String(), "status 404") {
		t.Errorf("no status log: %s", logs.String())
	}
}

func TestNewScheduler_PingsAfterSync(t *testing.T) {
	ps := newPingServer(t, http.StatusOK)
	url := ps.URL + "/ping/s3cret-key"
	var logs bytes.Buffer
	a := &app{
		cfg: &config.Config{YNABAccessToken: "tok", YNABBudgetID: "plan", SyncInterval: time.Minute,
			HealthcheckPingURL: url},
		log: slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	a.newScheduler()
	if !strings.Contains(logs.String(), `"msg":"healthcheck ping","enabled":true`) {
		t.Errorf("no startup ping log: %s", logs.String())
	}
	if got := a.redact("GET " + url + "/fail: refused"); strings.Contains(got, "s3cret-key") {
		t.Errorf("redact leaked the ping URL: %q", got)
	}

	logs.Reset()
	a.cfg.HealthcheckPingURL = ""
	a.newScheduler()
	if !strings.Contains(logs.String(), `"msg":"healthcheck ping","enabled":false`) {
		t.Errorf("no startup ping-off log: %s", logs.String())
	}
}

func TestProbe(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := strings.Cut(addr, ":")
	var stderr bytes.Buffer
	if code := probe(addr, &stderr); code != 0 {
		t.Errorf("healthy: exit %d (%s)", code, stderr.String())
	}
	if code := probe(":"+port, &stderr); code != 0 {
		t.Errorf("healthy via :port: exit %d (%s)", code, stderr.String())
	}
	status = http.StatusServiceUnavailable
	if code := probe(addr, &stderr); code != 1 {
		t.Errorf("503: exit %d, want 1", code)
	}
	srv.Close()
	if code := probe(addr, &stderr); code != 1 {
		t.Errorf("down: exit %d, want 1", code)
	}
	if code := probe("nonsense", &stderr); code != 1 {
		t.Errorf("bad addr: exit %d, want 1", code)
	}
}
