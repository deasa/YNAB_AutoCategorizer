package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

const (
	testEmail    = "alice@example.com"
	testYNAB     = "ynab-token-SECRET"
	testGoogle   = "google-client-SECRET"
	testRedirect = "http://localhost:9999/cb"
	publicHost   = "finance.hoidlabs.site"
)

// fakePlan stands in for YNAB, so serve tests make no network calls.
type fakePlan struct{ syncs *atomic.Int32 }

func (f fakePlan) Export(context.Context) (*ynab.PlanDetail, ynab.ServerKnowledge, error) {
	f.syncs.Add(1)
	return &ynab.PlanDetail{}, 1, nil
}
func (f fakePlan) Delta(context.Context, *ynab.SyncState) (*ynab.PlanDetail, error) {
	f.syncs.Add(1)
	return &ynab.PlanDetail{}, nil
}
func (fakePlan) ImportTransactions(context.Context) ([]string, error) {
	return nil, errors.New("not in tests")
}
func (fakePlan) GetCategories(context.Context) ([]*ynab.CategoryGroup, error) {
	return nil, errors.New("not in tests")
}

// fakeIDP signs testEmail in at once, redirecting straight to the callback.
type fakeIDP struct{ issuer string }

func (f fakeIDP) AuthCodeURL(state, _, _ string) string {
	return f.issuer + "/oauth/google/callback?code=c&state=" + url.QueryEscape(state)
}
func (fakeIDP) Exchange(context.Context, string, string, string) (auth.Identity, error) {
	return auth.Identity{Email: testEmail, EmailVerified: true, Name: "Alice"}, nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// serveRun is a real `finbot serve` through Main, ended by a real SIGTERM.
type serveRun struct {
	t         *testing.T
	dbPath    string
	healthURL string
	mcpPort   int
	issuer    string
	syncs     atomic.Int32
	logs      lockedBuffer
	exit      chan int
	stopped   bool
}

// startServe runs serve with MCP on when mcpOn, and returns once /healthz is 200.
func startServe(t *testing.T, mcpOn bool) *serveRun {
	t.Helper()
	r := prepareServe(t, mcpOn)
	r.start()
	r.waitHealthy()
	return r
}

func prepareServe(t *testing.T, mcpOn bool) *serveRun {
	t.Helper()
	httpPort := freePort(t)
	r := &serveRun{t: t, dbPath: filepath.Join(t.TempDir(), "finbot.db"), mcpPort: freePort(t), exit: make(chan int, 1)}
	r.healthURL = fmt.Sprintf("http://127.0.0.1:%d/healthz", httpPort)
	r.issuer = fmt.Sprintf("http://127.0.0.1:%d", r.mcpPort)

	env := map[string]string{
		"YNAB_ACCESS_TOKEN": testYNAB, "YNAB_BUDGET_ID": "plan-1", "FINBOT_DB_PATH": r.dbPath,
		"HTTP_ADDR": fmt.Sprintf("127.0.0.1:%d", httpPort), "SYNC_INTERVAL": "1h", "LOG_LEVEL": "info",
		"MCP_LISTEN_ADDR": fmt.Sprintf(":%d", r.mcpPort), "MCP_PUBLIC_URL": "", "MCP_ALLOWED_EMAILS": "",
		"GOOGLE_OAUTH_CLIENT_ID": "", "GOOGLE_OAUTH_CLIENT_SECRET": "",
		"SLACK_BOT_TOKEN": "", "HEALTHCHECK_PING_URL": "", "IMPORT_ENABLED": "false",
		"BACKUP_AGE_RECIPIENT": "", "BACKUP_TURSO_URL": "", "BACKUP_TURSO_TOKEN": "",
	}
	if mcpOn {
		env["MCP_PUBLIC_URL"] = r.issuer
		env["MCP_ALLOWED_EMAILS"] = testEmail
		env["GOOGLE_OAUTH_CLIENT_ID"] = "client-id"
		env["GOOGLE_OAUTH_CLIENT_SECRET"] = testGoogle
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	oldClient, oldIDP := newPlanClient, newIDP
	newPlanClient = func(string, string, *log.Logger, *ynab.Governor) finsync.PlanAPI { return fakePlan{&r.syncs} }
	newIDP = func(context.Context, config.MCP) (auth.IdentityProvider, error) { return fakeIDP{r.issuer}, nil }
	// Holding SIGTERM here too means a mistimed signal can't kill the test binary.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	t.Cleanup(func() {
		r.stop()
		signal.Stop(sigs)
		newPlanClient, newIDP = oldClient, oldIDP
	})
	return r
}

func (r *serveRun) start() {
	go func() { r.exit <- Main([]string{"serve"}, io.Discard, &r.logs) }()
}

func (r *serveRun) waitHealthy() {
	r.t.Helper()
	r.eventually("healthz 200", func() bool {
		resp, err := http.Get(r.healthURL)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

func (r *serveRun) eventually(what string, ok func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	r.t.Fatalf("timed out waiting for %s; logs:\n%s", what, r.logs.String())
}

// stop sends SIGTERM and returns Main's exit code.
func (r *serveRun) stop() int {
	r.t.Helper()
	if r.stopped {
		return 0
	}
	r.stopped = true
	syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case code := <-r.exit:
		return code
	case <-time.After(shutdownTimeout + 5*time.Second):
		r.t.Fatal("serve didn't stop after SIGTERM")
		return -1
	}
}

func (r *serveRun) url(path string) string { return r.issuer + path }

func (r *serveRun) waitMCP() {
	r.t.Helper()
	r.eventually("mcp ready", func() bool { return strings.Contains(r.logs.String(), `"msg":"mcp serving"`) })
}

var (
	noFollow  = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	formField = regexp.MustCompile(`name="(consent_id|csrf)" value="([^"]+)"`)
)

// signIn runs the whole flow over HTTP: DCR, authorize, the fake Google,
// consent and the token exchange.
func (r *serveRun) signIn() string {
	t := r.t
	t.Helper()
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{testRedirect}, "client_name": "Test"})
	resp, err := http.Post(r.url("/oauth/register"), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()

	verifier := oauth2.GenerateVerifier()
	q := url.Values{"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {testRedirect},
		"scope": {"mcp offline_access"}, "state": {"client-state-123"}, "resource": {r.url("/mcp")},
		"code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"}}
	resp, err = noFollow.Get(r.url("/oauth/authorize?" + q.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}

	page, err := noFollow.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(page.Body)
	page.Body.Close()
	form := url.Values{"decision": {"approve"}}
	for _, m := range formField.FindAllStringSubmatch(string(html), -1) {
		form.Set(m[1], m[2])
	}
	req, _ := http.NewRequest(http.MethodPost, r.url("/oauth/consent"), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range page.Cookies() {
		req.AddCookie(c)
	}
	resp, err = noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || back.Query().Get("code") == "" {
		t.Fatalf("consent: %d, no code", resp.StatusCode)
	}

	resp, err = http.PostForm(r.url("/oauth/token"), url.Values{"grant_type": {"authorization_code"},
		"code": {back.Query().Get("code")}, "redirect_uri": {testRedirect}, "client_id": {reg.ClientID}, "code_verifier": {verifier}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	json.NewDecoder(resp.Body).Decode(&tok)
	if tok.AccessToken == "" {
		t.Fatalf("token: %d, no access token", resp.StatusCode)
	}
	return tok.AccessToken
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

// postMCP sends initialize to target with the given Host (empty keeps target's).
func postMCP(t *testing.T, target, host, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, target, strings.NewReader(initializeBody))
	if host != "" {
		req.Host = host
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connectMCP(t *testing.T, endpoint, token string) *sdkmcp.ClientSession {
	t.Helper()
	c := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &sdkmcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerRT{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func httpStatus(t *testing.T, method, target string) int {
	t.Helper()
	req, _ := http.NewRequest(method, target, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The acceptance path end to end, on real listeners and the real auth server.
func TestServe_MCPEndToEnd(t *testing.T) {
	r := startServe(t, true)
	r.waitMCP()
	ctx := context.Background()

	// No token: 401, pointing at the metadata.
	resp := postMCP(t, r.url("/mcp"), "", "")
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(resp.Header.Get("WWW-Authenticate"), r.url("/.well-known/oauth-protected-resource")) {
		t.Errorf("no token: %d, WWW-Authenticate %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	// The metadata names finbot, and the preflight works on the listener.
	metaResp, err := http.Get(r.url("/.well-known/oauth-protected-resource"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		ResourceName string `json:"resource_name"`
	}
	json.NewDecoder(metaResp.Body).Decode(&meta)
	metaResp.Body.Close()
	if meta.ResourceName != "finbot" {
		t.Errorf("resource_name = %q", meta.ResourceName)
	}
	req, _ := http.NewRequest(http.MethodOptions, r.url("/mcp"), nil)
	req.Header.Set("Origin", "https://inspector.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	preflight, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	preflight.Body.Close()
	if preflight.StatusCode != http.StatusNoContent || preflight.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight: %d %v", preflight.StatusCode, preflight.Header)
	}

	// A signed-in user sees the tool and can call it, with a real sync behind it.
	token := r.signIn()
	s := connectMCP(t, r.url("/mcp"), token)
	list, err := s.ListTools(ctx, nil)
	if err != nil || len(list.Tools) == 0 {
		t.Fatalf("tools/list: %v %+v", err, list)
	}
	res, err := s.CallTool(ctx, &sdkmcp.CallToolParams{Name: "data_freshness"})
	if err != nil || res.IsError {
		t.Fatalf("data_freshness: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var got struct {
		DataFreshness struct {
			LastSyncedAt string `json:"last_synced_at"`
			Stale        bool   `json:"stale"`
		} `json:"data_freshness"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.DataFreshness.LastSyncedAt == "" || got.DataFreshness.Stale {
		t.Errorf("data_freshness = %s (%v)", raw, err)
	}

	// /healthz exists only on its own listener.
	if got := httpStatus(t, http.MethodGet, r.url("/healthz")); got != http.StatusNotFound {
		t.Errorf("GET /healthz on the MCP port: %d", got)
	}
	if got := httpStatus(t, http.MethodOptions, r.url("/healthz")); got != http.StatusNotFound {
		t.Errorf("OPTIONS /healthz on the MCP port: %d", got)
	}
	if got := httpStatus(t, http.MethodGet, r.healthURL); got != http.StatusOK {
		t.Errorf("GET /healthz on HTTP_ADDR: %d", got)
	}
	t.Setenv("HTTP_ADDR", strings.TrimSuffix(strings.TrimPrefix(r.healthURL, "http://"), "/healthz"))
	var out, errOut bytes.Buffer
	if code := Main([]string{"health"}, &out, &errOut); code != 0 {
		t.Errorf("finbot health = %d: %s", code, errOut.String())
	}

	// The public Host isn't refused on a real non-loopback connection.
	if ip := nonLoopbackIP(); ip != "" {
		target := fmt.Sprintf("http://%s/mcp", net.JoinHostPort(ip, fmt.Sprint(r.mcpPort)))
		if got := postMCP(t, target, publicHost, token).StatusCode; got != http.StatusOK {
			t.Errorf("public Host on %s: %d, want 200", "the host interface", got)
		}
	} else {
		t.Log("no non-loopback interface here; TestDNSRebindingGuard in mcp/ still covers this")
	}
	if got := postMCP(t, r.url("/mcp"), publicHost, token).StatusCode; got != http.StatusForbidden {
		t.Errorf("public Host on loopback: %d, want 403", got)
	}

	// `finbot revoke` from a second process's view: 401 at once, no restart.
	if got := postMCP(t, r.url("/mcp"), "", token).StatusCode; got != http.StatusOK {
		t.Fatalf("before revoke: %d", got)
	}
	out.Reset()
	if code := Main([]string{"revoke", testEmail}, &out, &errOut); code != 0 {
		t.Fatalf("revoke = %d: %s", code, errOut.String())
	}
	if !regexp.MustCompile(`^revoked [1-9]\d* active tokens for u_[0-9a-f]{12}\n$`).MatchString(out.String()) {
		t.Errorf("real revoke printed %q", out.String())
	}
	if got := postMCP(t, r.url("/mcp"), "", token).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("revoked token: %d, want 401", got)
	}
	// The same user again, and a typo: both plainly nothing, both exit 0.
	for _, arg := range []string{testEmail, "alcie@example.com"} {
		out.Reset()
		if code := Main([]string{"revoke", arg}, &out, &errOut); code != 0 {
			t.Errorf("revoke %q = %d", arg, code)
		}
		if !strings.HasPrefix(out.String(), "no active tokens for u_") || strings.Contains(out.String(), "@") {
			t.Errorf("revoke %q printed %q", arg, out.String())
		}
	}
	if got := postMCP(t, r.url("/mcp"), "", r.signIn()).StatusCode; got != http.StatusOK {
		t.Errorf("fresh sign-in after revoke: %d", got)
	}

	if code := r.stop(); code != 0 {
		t.Errorf("exit = %d", code)
	}
	logs := r.logs.String()
	for _, secret := range []string{testEmail, testYNAB, testGoogle, token, "client-state-123", "plan-1"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs carry %q", secret)
		}
	}
	if !strings.Contains(logs, `"msg":"mcp request"`) || !strings.Contains(logs, `"msg":"oauth prune"`) {
		t.Errorf("expected request and prune logs:\n%s", logs)
	}
}

func nonLoopbackIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

func TestServe_SIGTERMClosesListenersAndDB(t *testing.T) {
	r := startServe(t, true)
	r.waitMCP()
	// A connected client holds a stream open, which Shutdown alone waits on forever.
	s := connectMCP(t, r.url("/mcp"), r.signIn())
	if _, err := s.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	if code := r.stop(); code != 0 {
		t.Errorf("exit = %d:\n%s", code, r.logs.String())
	}
	if d := time.Since(started); d > shutdownTimeout+2*time.Second {
		t.Errorf("shutdown took %v", d)
	}
	for name, addr := range map[string]string{
		"health": strings.TrimSuffix(strings.TrimPrefix(r.healthURL, "http://"), "/healthz"),
		"mcp":    strings.TrimPrefix(r.issuer, "http://"),
	} {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			t.Errorf("%s listener still open", name)
		}
	}
	// The last connection closing checkpoints and removes the WAL.
	if _, err := os.Stat(r.dbPath + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("WAL still there, so the DB wasn't closed: %v", err)
	}
	if !strings.Contains(r.logs.String(), `"msg":"stopped"`) {
		t.Errorf("no stopped log:\n%s", r.logs.String())
	}
}

// With MCP config blank: no MCP listener, no OAuth job, sync and health as before.
func TestServe_MCPBlankBehavesAsToday(t *testing.T) {
	r := startServe(t, false)
	if r.syncs.Load() == 0 {
		t.Error("no sync ran")
	}
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(r.issuer, "http://"), time.Second); err == nil {
		c.Close()
		t.Error("an MCP listener is up")
	}
	if code := r.stop(); code != 0 {
		t.Errorf("exit = %d", code)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, `"msg":"mcp disabled"`) || strings.Contains(logs, "oauth prune") {
		t.Errorf("logs:\n%s", logs)
	}
	for _, key := range []string{"GOOGLE_OAUTH_CLIENT_SECRET", "MCP_PUBLIC_URL"} {
		if !strings.Contains(logs, key) {
			t.Errorf("the disabled log doesn't name %s:\n%s", key, logs)
		}
	}
}

// The scheduler's other jobs don't depend on MCP: backups stay armed and the
// status message still goes out, through serve itself.
func TestServe_MCPBlankKeepsStatusAndBackup(t *testing.T) {
	var updates atomic.Int32
	var logs lockedBuffer
	var syncs atomic.Int32
	var dummy bytes.Buffer
	a := statusApp(t, &dummy, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users.conversations":
			json.NewEncoder(w).Encode(map[string]any{"ok": true,
				"channels": []map[string]string{{"id": "C1", "name": "finbot-finances"}}})
		default:
			updates.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.000"})
		}
	}))
	a.log = slog.New(slog.NewJSONHandler(&logs, nil))
	a.cfg.HTTPAddr = "127.0.0.1:0"
	a.cfg.Backup = config.Backup{AgeRecipient: "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p",
		TursoURL: "libsql://db.example", TursoToken: "tok", AtHour: 3, AtMinute: 17, Keep: 14}
	a.syncer = finsync.New(a.db, func(string) finsync.PlanAPI { return fakePlan{&syncs} }, log.New(io.Discard, "", 0))

	sched, _ := a.newScheduler()
	if sched.Backup == nil || sched.Prune != nil {
		t.Errorf("Backup set = %v, Prune set = %v", sched.Backup != nil, sched.Prune != nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.serve(ctx) }()
	for deadline := time.Now().Add(5 * time.Second); updates.Load() == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
	if syncs.Load() == 0 || updates.Load() == 0 {
		t.Errorf("syncs = %d, status updates = %d", syncs.Load(), updates.Load())
	}
}

// Google being down at startup doesn't stop the sync jobs. The MCP port answers
// 503 until discovery works, /healthz is never on it, and then MCP comes up.
func TestServe_GoogleDownAtStartup(t *testing.T) {
	r := prepareServe(t, true)
	oldRetry := idpRetry
	idpRetry = 50 * time.Millisecond
	t.Cleanup(func() { idpRetry = oldRetry })
	var down atomic.Bool
	down.Store(true)
	newIDP = func(context.Context, config.MCP) (auth.IdentityProvider, error) {
		if down.Load() {
			return nil, errors.New("discovery failed for " + testGoogle)
		}
		return fakeIDP{r.issuer}, nil
	}
	r.start()
	r.waitHealthy()
	if got := postMCP(t, r.url("/mcp"), "", "").StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("before discovery: %d", got)
	}
	if got := httpStatus(t, http.MethodGet, r.url("/healthz")); got != http.StatusNotFound {
		t.Errorf("/healthz on the MCP port before discovery: %d", got)
	}
	down.Store(false)
	r.waitMCP()
	if got := postMCP(t, r.url("/mcp"), "", "").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("after discovery: %d", got)
	}
	r.stop()
	if logs := r.logs.String(); strings.Contains(logs, testGoogle) || !strings.Contains(logs, "google discovery failed") {
		t.Errorf("logs:\n%s", logs)
	}
}

func TestServe_MCPPortTakenFailsAtOnce(t *testing.T) {
	r := prepareServe(t, true)
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", r.mcpPort))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r.start()
	select {
	case code := <-r.exit:
		r.stopped = true
		if code != 1 || r.syncs.Load() != 0 {
			t.Errorf("exit = %d, syncs = %d", code, r.syncs.Load())
		}
	case <-time.After(5 * time.Second):
		t.Error("serve kept running without its MCP port")
	}
}

func TestProbe_DefaultMatchesServe(t *testing.T) {
	if !strings.HasPrefix(config.DefaultHTTPAddr, "127.0.0.1:") {
		t.Errorf("DefaultHTTPAddr = %q: /healthz must stay on loopback", config.DefaultHTTPAddr)
	}
	ln, err := net.Listen("tcp", config.DefaultHTTPAddr)
	if err != nil {
		t.Skipf("%s is busy here", config.DefaultHTTPAddr)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	var errOut bytes.Buffer
	if code := probe("", &errOut); code != 0 {
		t.Errorf("probe with no HTTP_ADDR = %d: %s", code, errOut.String())
	}
}

// revokeEnv leaves FINBOT_DB_PATH as the only setting finbot could read.
func revokeEnv(t *testing.T) *store.DB {
	t.Helper()
	for _, k := range []string{"YNAB_ACCESS_TOKEN", "YNAB_BUDGET_ID", "GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_SECRET",
		"MCP_PUBLIC_URL", "MCP_ALLOWED_EMAILS", "SLACK_BOT_TOKEN", "BACKUP_TURSO_URL"} {
		t.Setenv(k, "")
	}
	path := filepath.Join(t.TempDir(), "finbot.db")
	t.Setenv("FINBOT_DB_PATH", path)
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRevoke_OnlyNeedsDBPath(t *testing.T) {
	db := revokeEnv(t)
	ctx := context.Background()
	s := auth.New(db)
	subject := auth.UserID(testEmail)
	for i, arg := range []string{testEmail, "  ALICE@example.com", subject, "--all"} {
		var out, errOut bytes.Buffer
		if code := Main([]string{"revoke", arg}, &out, &errOut); code != 0 {
			t.Fatalf("revoke %q = %d: %s", arg, code, errOut.String())
		}
		if strings.Contains(out.String(), "@") || strings.Contains(strings.ToLower(out.String()), "alice") {
			t.Errorf("revoke %q printed the email: %s", arg, out.String())
		}
		if !strings.HasPrefix(out.String(), "no active tokens for ") {
			t.Errorf("revoke %q with no tokens printed %q", arg, out.String())
		}
		if gen, err := s.CurrentGeneration(ctx, subject); err != nil || gen != i+1 {
			t.Errorf("after revoke %q: generation %d, %v", arg, gen, err)
		}
	}
	if gen, _ := s.CurrentGeneration(ctx, auth.UserID("bob@example.com")); gen != 1 {
		t.Errorf("--all didn't reach everyone: %d", gen)
	}
}

func TestRevoke_RefusesWhatWouldRevokeNobody(t *testing.T) {
	db := revokeEnv(t)
	for _, args := range [][]string{nil, {"a@b.c", "d@e.f"}, {"u_abc"}, {"u_ABCDEF123456"}, {"alice"}, {"--bogus"}, {""}} {
		var out, errOut bytes.Buffer
		if code := Main(append([]string{"revoke"}, args...), &out, &errOut); code != 2 {
			t.Errorf("revoke %q = %d, want 2", args, code)
		}
	}
	if gen, _ := auth.New(db).CurrentGeneration(context.Background(), auth.AllUsers); gen != 0 {
		t.Errorf("a refused revoke changed something: %d", gen)
	}

	missing := filepath.Join(t.TempDir(), "typo.db")
	t.Setenv("FINBOT_DB_PATH", missing)
	var out, errOut bytes.Buffer
	if code := Main([]string{"revoke", "--all"}, &out, &errOut); code != 1 {
		t.Errorf("revoke with no database = %d, want 1", code)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("revoke created a database at a wrong path: %v", err)
	}
}

func TestAddPrune(t *testing.T) {
	var logs bytes.Buffer
	a := statusApp(t, &logs, http.NotFoundHandler())
	a.cfg.MCP = config.MCP{PublicURL: "https://x.example", GoogleClientID: "i", GoogleClientSecret: "s", AllowedEmails: testEmail}
	sched := &Scheduler{}
	a.addPrune(sched)
	if sched.Prune == nil || sched.PruneEvery != 24*time.Hour {
		t.Fatalf("Prune set = %v, every %v", sched.Prune != nil, sched.PruneEvery)
	}
	if err := sched.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `"msg":"oauth prune"`) {
		t.Errorf("no outcome log: %s", logs.String())
	}
	a.db.Close()
	if err := sched.Prune(context.Background()); err == nil {
		t.Error("Prune on a closed DB succeeded")
	}
	if !strings.Contains(logs.String(), `"msg":"oauth prune failed"`) {
		t.Errorf("no failure log: %s", logs.String())
	}
}

func TestSecrets_CoverMCP(t *testing.T) {
	cfg := &config.Config{MCP: config.MCP{GoogleClientSecret: "shh-secret", AllowedEmails: "a@example.com, b@example.com"}}
	got := scrub("err shh-secret for a@example.com and b@example.com", secrets(cfg)...)
	if strings.Contains(got, "shh-secret") || strings.Contains(got, "@example.com") {
		t.Errorf("scrub leaked: %q", got)
	}
}

// An open stream never finishes, so shutdown must end it rather than leave its
// handler running while the DB closes.
func TestShutdown_EndsOpenStreams(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() { shutdownTimeout = old })

	entered, ended := make(chan struct{}), make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(ended)
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	resp, err := http.Get("http://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered

	shutdown([]*http.Server{srv})
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Error("the stream's handler is still running after shutdown")
	}
}

// A failed write is a failure, never "no active tokens". A failed count must not
// stop the revoke, and doesn't pretend there were none.
func TestRevoke_FailuresAreNotReportedAsNoTokens(t *testing.T) {
	ctx := context.Background()

	db := revokeEnv(t)
	if _, err := db.Writer().ExecContext(ctx, `DROP TABLE oauth_pkce_sessions; DROP TABLE oauth_auth_codes`); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Main([]string{"revoke", testEmail}, &out, &errOut); code != 0 {
		t.Errorf("count failed: exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "couldn't count") || strings.Contains(out.String(), "no active") {
		t.Errorf("count failed: printed %q", out.String())
	}
	if gen, err := auth.New(db).CurrentGeneration(ctx, auth.UserID(testEmail)); err != nil || gen != 1 {
		t.Errorf("count failed: the revoke didn't happen: generation %d, %v", gen, err)
	}

	db = revokeEnv(t)
	if _, err := db.Writer().ExecContext(ctx, `DROP TABLE token_generations`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Main([]string{"revoke", testEmail}, &out, &errOut); code != 1 {
		t.Errorf("bump failed: exit %d", code)
	}
	if out.Len() != 0 || strings.Contains(errOut.String(), testEmail) {
		t.Errorf("bump failed: stdout %q, stderr %q", out.String(), errOut.String())
	}
}
