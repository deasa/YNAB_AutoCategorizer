package mcp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	finauth "github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	metadataURL = "https://finance.example.com/.well-known/oauth-protected-resource"
	planID      = "plan-1"
	staleAfter  = 30 * time.Minute
)

var now = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// tokens: "good" has the MCP scope, "noscope" lacks it, "writer" also has a
// second scope, and anything else is invalid.
func verify(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
	exp := now.Add(1000 * time.Hour)
	if time.Now().After(exp) {
		exp = time.Now().Add(time.Hour)
	}
	info := &sdkauth.TokenInfo{UserID: "u_test", Expiration: exp}
	switch token {
	case "good":
		info.Scopes = []string{finauth.ScopeMCP}
	case "noscope":
		info.Scopes = []string{"other"}
	case "writer":
		info.Scopes = []string{finauth.ScopeMCP, "write"}
	default:
		return nil, sdkauth.ErrInvalidToken
	}
	return info, nil
}

func testOptions(t *testing.T) (Options, *bytes.Buffer) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var logs bytes.Buffer
	return Options{
		Verify: verify, ResourceMetadataURL: metadataURL, Version: "test",
		DB: db, PlanID: planID, StaleAfter: staleAfter,
		Log: slog.New(slog.NewJSONHandler(&logs, nil)), Now: func() time.Time { return now },
	}, &logs
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, url, token string) *sdkmcp.ClientSession {
	t.Helper()
	c := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func toolNames(t *testing.T, s *sdkmcp.ClientSession) []string {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestAuthOnEndpoint(t *testing.T) {
	o, _ := testOptions(t)
	srv := httptest.NewServer(NewHandler(o))
	defer srv.Close()
	for _, c := range []struct {
		token string
		want  int
	}{{"", 401}, {"bad", 401}, {"noscope", 403}} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("token %q: status %d, want %d", c.token, resp.StatusCode, c.want)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="`+metadataURL+`"`) {
			t.Errorf("token %q: WWW-Authenticate = %q", c.token, got)
		}
	}
}

func TestDataFreshness(t *testing.T) {
	ctx := context.Background()
	o, _ := testOptions(t)
	srv := httptest.NewServer(NewHandler(o))
	defer srv.Close()
	s := connect(t, srv.URL, "good")

	call := func() Response[freshnessData] {
		t.Helper()
		res, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "data_freshness"})
		if err != nil || res.IsError {
			t.Fatalf("call: %v %+v", err, res)
		}
		return decode[freshnessData](t, res)
	}

	// Never synced: stale, with no time to report.
	got := call()
	if !got.DataFreshness.Stale || got.DataFreshness.LastSyncedAt != "" || got.DataFreshness.AgeSeconds != nil {
		t.Errorf("never synced: %+v", got.DataFreshness)
	}
	if got.Data.StaleAfterSeconds != 1800 || got.Data.ImportStaleAfterDays != importStaleAfterDays {
		t.Errorf("data = %+v", got.Data)
	}
	if got.DataFreshness.LinksOK || len(got.DataFreshness.Warnings) != 1 {
		t.Errorf("never synced should warn and not claim links are fine: %+v", got.DataFreshness)
	}

	for _, c := range []struct {
		age   time.Duration
		stale bool
	}{{10 * time.Minute, false}, {staleAfter, false}, {staleAfter + time.Second, true}} {
		if err := o.DB.SetSyncState(ctx, store.SyncState{PlanID: planID, ServerKnowledge: 1, SyncedAt: now.Add(-c.age)}); err != nil {
			t.Fatal(err)
		}
		f := call().DataFreshness
		if f.Stale != c.stale || f.AgeSeconds == nil || *f.AgeSeconds != int64(c.age/time.Second) ||
			f.LastSyncedAt != now.Add(-c.age).Format(time.RFC3339) {
			t.Errorf("age %v: %+v", c.age, f)
		}
	}
}

// Another plan's sync says nothing about the active plan.
func TestDataFreshness_OnlyActivePlan(t *testing.T) {
	ctx := context.Background()
	o, _ := testOptions(t)
	if err := o.DB.SetSyncState(ctx, store.SyncState{PlanID: "archived", ServerKnowledge: 1, SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	f, err := readFreshness(ctx, &o)
	if err != nil || !f.Stale || f.LastSyncedAt != "" {
		t.Errorf("%+v, %v", f, err)
	}
}

func TestEveryToolReportsFreshness(t *testing.T) {
	o, _ := testOptions(t)
	srv := httptest.NewServer(NewHandler(o))
	defer srv.Close()
	res, err := connect(t, srv.URL, "good").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) == 0 {
		t.Fatal("no tools")
	}
	for _, tool := range res.Tools {
		schema, _ := tool.OutputSchema.(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["data_freshness"]; !ok {
			t.Errorf("%s: output schema has no data_freshness: %v", tool.Name, schema)
		}
	}
}

type emptyArgs struct{}

// The token decides which tools exist, for the list and the call alike.
func TestToolsLimitedByToken(t *testing.T) {
	ctx := context.Background()
	o, _ := testOptions(t)
	var ran int
	defs := append(append([]toolDef{}, registry...), define("write", &sdkmcp.Tool{Name: "secret_write"},
		func(context.Context, *Options, *sdkmcp.CallToolRequest, emptyArgs) (struct{}, error) {
			ran++
			return struct{}{}, nil
		}))
	srv := httptest.NewServer(newHandler(o, defs))
	defer srv.Close()

	reader := connect(t, srv.URL, "good")
	names := strings.Join(toolNames(t, reader), ",")
	if strings.Contains(names, "secret_write") || !strings.Contains(names, "data_freshness") {
		t.Errorf("reader's tools: %s", names)
	}
	_, refused := reader.CallTool(ctx, &sdkmcp.CallToolParams{Name: "secret_write"})
	_, missing := reader.CallTool(ctx, &sdkmcp.CallToolParams{Name: "no_such_tool"})
	if refused == nil || missing == nil || ran != 0 {
		t.Fatalf("refused = %v, missing = %v, ran = %d", refused, missing, ran)
	}
	if want := strings.ReplaceAll(missing.Error(), "no_such_tool", "secret_write"); refused.Error() != want {
		t.Errorf("a refused tool is distinguishable from a missing one:\n%v\n%v", refused, missing)
	}

	writer := connect(t, srv.URL, "writer")
	if names := strings.Join(toolNames(t, writer), ","); !strings.Contains(names, "secret_write") {
		t.Errorf("writer's tools: %s", names)
	}
	if res, err := writer.CallTool(ctx, &sdkmcp.CallToolParams{Name: "secret_write"}); err != nil || res.IsError || ran != 1 {
		t.Errorf("writer's call: %v %+v, ran = %d", err, res, ran)
	}
}

func TestLimitToToken_NoTokenNoTools(t *testing.T) {
	o, _ := testOptions(t)
	ct, st := sdkmcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := newServer(o, registry).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if names := toolNames(t, cs); len(names) != 0 {
		t.Errorf("tools with no token: %v", names)
	}
	if _, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: "data_freshness"}); err == nil {
		t.Error("call with no token succeeded")
	}
}

// A tool's failure, or a failure to read freshness, reaches the client as a
// generic error, and the detail reaches the log only after redaction.
func TestToolFailureIsGenericAndRedacted(t *testing.T) {
	ctx := context.Background()
	o, logs := testOptions(t)
	o.Redact = func(s string) string { return strings.ReplaceAll(s, "SECRET", "<secret>") }
	defs := []toolDef{define(finauth.ScopeMCP, &sdkmcp.Tool{Name: "boom"},
		func(context.Context, *Options, *sdkmcp.CallToolRequest, emptyArgs) (struct{}, error) {
			return struct{}{}, errors.New("db SECRET broke")
		})}
	srv := httptest.NewServer(newHandler(o, defs))
	defer srv.Close()
	res, err := connect(t, srv.URL, "good").CallTool(ctx, &sdkmcp.CallToolParams{Name: "boom"})
	if err != nil || !res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if text := res.Content[0].(*sdkmcp.TextContent).Text; strings.Contains(text, "SECRET") || strings.Contains(text, "broke") {
		t.Errorf("client saw the detail: %s", text)
	}
	if !strings.Contains(logs.String(), "db <secret> broke") || strings.Contains(logs.String(), "SECRET") {
		t.Errorf("log: %s", logs.String())
	}

	o.DB.Close()
	if _, err := respond(ctx, &o, "t", func() (int, error) { return 1, nil }); !errors.Is(err, errTool) {
		t.Errorf("no freshness, still answered: %v", err)
	}
}

// The guard refuses a public Host only on a connection that arrived at a
// loopback address. Behind cloudflared the connection arrives at the container
// interface, so it's let through; the loopback case proves the guard is on.
func TestDNSRebindingGuard(t *testing.T) {
	o, _ := testOptions(t)
	h := NewHandler(o)
	status := func(local string) int {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
		req := httptest.NewRequest(http.MethodPost, "http://finance.hoidlabs.site/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer good")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		addr := net.TCPAddrFromAddrPort(mustAddrPort(t, local))
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, addr))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := status("172.18.0.2:8080"); got != http.StatusOK {
		t.Errorf("public Host on the container interface: %d, want 200", got)
	}
	if got := status("127.0.0.1:8080"); got != http.StatusForbidden {
		t.Errorf("public Host on loopback: %d, want 403 (is the guard on?)", got)
	}
}

func TestCORS(t *testing.T) {
	var reached bool
	h := CORS(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	req.Header.Set("Origin", "https://inspector.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || reached {
		t.Errorf("preflight: %d, reached = %v", rec.Code, reached)
	}
	for k, want := range map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Allow-Methods":  "POST",
		"Access-Control-Allow-Headers":  "Authorization",
		"Access-Control-Expose-Headers": "WWW-Authenticate",
	} {
		if got := rec.Header().Get(k); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if !reached {
		t.Error("a real request didn't reach the handler")
	}
}

func TestLogRequests_NoQueryHeadersOrStrayPaths(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	h := LogRequests(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(418) }))
	req := httptest.NewRequest(http.MethodGet, "/oauth/google/callback?code=SECRETCODE&state=SECRETSTATE", nil)
	req.Header.Set("Authorization", "Bearer SECRETTOKEN")
	req.Header.Set("CF-Connecting-IP", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/someone@example.com/SECRETPATH", nil))
	got := logs.String()
	for _, bad := range []string{"SECRET", "someone@", "203.0.113.9"} {
		if strings.Contains(got, bad) {
			t.Errorf("log carries %q: %s", bad, got)
		}
	}
	for _, want := range []string{`"path":"/oauth/google/callback"`, `"status":418`, `"path":"other"`} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %s: %s", want, got)
		}
	}
}

func TestStatusRecorderKeepsStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: 200}
	http.NewResponseController(sr).Flush()
	sr.Flush()
	if !rec.Flushed {
		t.Error("Flush didn't reach the writer")
	}
}
