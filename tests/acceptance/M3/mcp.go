package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"

	"github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

var finbotBin string // -bin

// ---- a real `finbot serve` against the sandbox --------------------------

type serveProc struct {
	cmd      *exec.Cmd
	done     chan error
	httpAddr string
	mcpURL   string
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatal("free port", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startServe runs `finbot serve` on the work DB with the sandbox as the active
// plan, every outward service (Slack, backups, ping, claude) blank, and a cwd
// with no .env. extra adds or overrides variables. It waits for /healthz 200.
func (h *harness) startServe(name string, extra map[string]string, wait time.Duration) *serveProc {
	if finbotBin == "" {
		fatal("serve", errors.New("-bin is required for this phase"))
	}
	if err := h.g.charge(serveAllowance); err != nil {
		fatal("serve "+name, err)
	}
	empty := filepath.Join(h.work, "cwd")
	_ = os.MkdirAll(empty, 0o700)
	sp := &serveProc{httpAddr: fmt.Sprintf("127.0.0.1:%d", freePort())}
	vars := map[string]string{
		"PATH": os.Getenv("PATH"), "HOME": empty, "TZ": "America/Denver",
		"FINBOT_DB_PATH":    filepath.Join(h.work, "finbot.db"),
		"YNAB_ACCESS_TOKEN": os.Getenv("YNAB_ACCESS_TOKEN"), "YNAB_BUDGET_ID": h.sb,
		"SYNC_INTERVAL": "24h", "IMPORT_ENABLED": "false", "LOG_LEVEL": "debug",
		"HTTP_ADDR": sp.httpAddr, "MCP_LISTEN_ADDR": fmt.Sprintf("127.0.0.1:%d", freePort()),
		"CLASSIFY_MODE":   "off",
		"SLACK_BOT_TOKEN": "", "SLACK_APP_TOKEN": "", "SLACK_USERS": "", "HEALTHCHECK_PING_URL": "",
		"BACKUP_AGE_RECIPIENT": "", "BACKUP_TURSO_URL": "", "BACKUP_TURSO_TOKEN": "",
		"MCP_PUBLIC_URL": "", "GOOGLE_OAUTH_CLIENT_ID": "", "GOOGLE_OAUTH_CLIENT_SECRET": "", "MCP_ALLOWED_EMAILS": "",
		"CLAUDE_CODE_OAUTH_TOKEN": "", "CLAUDE_BIN": "", "JEV_API_KEY": "",
	}
	for k, v := range extra {
		vars[k] = v
	}
	if v := vars["MCP_PUBLIC_URL"]; v != "" {
		sp.mcpURL = v
	}
	cmd := exec.Command(finbotBin, "serve")
	cmd.Dir = empty
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	logf, err := os.OpenFile(filepath.Join(h.work, "serve-"+name+".log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		fatal("serve log", err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		fatal("start serve", err)
	}
	sp.cmd, sp.done = cmd, make(chan error, 1)
	go func() { sp.done <- cmd.Wait(); logf.Close() }()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if r, err := http.Get("http://" + sp.httpAddr + "/healthz"); err == nil {
			r.Body.Close()
			if r.StatusCode == http.StatusOK {
				fmt.Printf("INFO serve %s healthy (first sync done)\n", name)
				return sp
			}
		}
		select {
		case err := <-sp.done:
			fatal("serve "+name+" exited early", err)
		case <-time.After(250 * time.Millisecond):
		}
	}
	sp.stop()
	fatal("serve "+name, errors.New("not healthy in time"))
	return nil
}

func (sp *serveProc) stop() {
	_ = sp.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-sp.done:
		check(err == nil, "serve exited cleanly on SIGTERM (%v)", err)
	case <-time.After(10 * time.Second):
		_ = sp.cmd.Process.Kill()
		check(false, "serve did not exit within 10s of SIGTERM")
	}
}

func (h *harness) serveLog(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.work, "serve-"+name+".log"))
	return string(b)
}

// Scope 6, second half: restart with the real binary, which syncs and reconciles.
func (h *harness) phaseCrashRestart() {
	h.fixtures()
	h.openDB(daysAgo(30))
	var cs crashState
	b, err := os.ReadFile(filepath.Join(h.work, "crash.json"))
	if err != nil || json.Unmarshal(b, &cs) != nil {
		fatal("state", fmt.Errorf("read crash.json: %v", err))
	}
	n, st := h.writeRows(cs.Txn)
	check(n == 1 && st[0] == "intent", "after the crash: the write is an unresolved intent (%v)", st)
	h.pickCategories()
	c, ap := h.ynabCat(cs.Txn)
	check(catOK(c, cs.Cat) && ap, "after the crash: YNAB already took the PATCH")
	if err := h.db.Close(); err != nil {
		fatal("close", err)
	}
	sp := h.startServe("crash", nil, 90*time.Second)
	sp.stop()
	log := h.serveLog("crash")
	check(strings.Contains(log, `"msg":"sync tick"`), "serve logged a sync tick")
	check(!strings.Contains(log, `"level":"ERROR"`), "serve logged no errors")
	h.phaseCrash2()
}

// Extra for scope 4: the real binary, CLASSIFY_MODE=auto with the shipped
// calibration (live plan's hashes, not auto-eligible) on the sandbox, with real Jev.
func (h *harness) phaseAutoBinary() {
	h.fixtures()
	h.openDB(today())
	h.mustSync("start")
	h.pickCategories()
	b1, b2 := h.createTxn("bin coffee shop", 0), h.createTxn("bin hardware store", 0)
	before := h.totalWrites()
	if err := h.db.Close(); err != nil {
		fatal("close", err)
	}
	sp := h.startServe("autobin", map[string]string{
		"CLASSIFY_MODE": "auto", "CLASSIFY_AUTO_SINCE": daysAgo(1), "JEV_API_KEY": os.Getenv("JEV_API_KEY"),
	}, 3*time.Minute)
	sp.stop()
	log := h.serveLog("autobin")
	check(strings.Contains(log, "auto running as dry_run"), "serve warned that configured auto ran as dry_run")
	check(strings.Contains(log, `"categorize enabled"`) && strings.Contains(log, `"mode":"auto"`), "serve started with categorize mode auto")
	h.openDB(today())
	check(h.totalWrites() == before, "no write_log rows added by serve (%d -> %d)", before, h.totalWrites())
	for i, id := range []string{b1, b2} {
		d, ok, err := h.db.LatestDecision(h.ctx, h.sb, id)
		check(ok && err == nil && d.Mode == "dry_run" && d.Outcome != "applied", "fixture %d decided by Jev in dry_run (mode=%s outcome=%s)", i, d.Mode, d.Outcome)
		c, _ := h.ynabCat(id)
		check(c == "", "fixture %d still uncategorized in YNAB", i)
	}
}

// ---- scope 7: get_category_decisions through the authenticated MCP ------

func (h *harness) mintCode(clientID, redirectURI, email, challenge string) string {
	db, err := store.Open(h.ctx, filepath.Join(h.work, "finbot.db"))
	if err != nil {
		fatal("open db", err)
	}
	defer db.Close()
	st := auth.New(db)
	secret, err := st.HMACSecret(h.ctx)
	if err != nil {
		fatal("hmac secret", err)
	}
	strat := compose.NewOAuth2HMACStrategy(&fosite.Config{
		AccessTokenLifespan: time.Hour, RefreshTokenLifespan: 30 * 24 * time.Hour, AuthorizeCodeLifespan: 5 * time.Minute, GlobalSecret: secret,
	})
	client, err := st.GetClient(h.ctx, clientID)
	if err != nil {
		fatal("get client", err)
	}
	uid := auth.UserID(email)
	gen, err := st.CurrentGeneration(h.ctx, uid)
	if err != nil {
		fatal("generation", err)
	}
	sess := auth.NewSession(uid, "Tester", gen)
	now := time.Now().UTC()
	sess.SetExpiresAt(fosite.AuthorizeCode, now.Add(5*time.Minute))
	sess.SetExpiresAt(fosite.AccessToken, now.Add(time.Hour))
	sess.SetExpiresAt(fosite.RefreshToken, now.Add(30*24*time.Hour))
	req := &fosite.Request{
		ID: rand.Text(), RequestedAt: now, Client: client,
		RequestedScope: fosite.Arguments{auth.ScopeMCP, "offline_access"}, GrantedScope: fosite.Arguments{auth.ScopeMCP, "offline_access"},
		Form: url.Values{"client_id": {clientID}, "redirect_uri": {redirectURI}, "response_type": {"code"},
			"scope": {"mcp offline_access"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}},
		Session: sess,
	}
	code, sig, err := strat.GenerateAuthorizeCode(h.ctx, req)
	if err != nil {
		fatal("authorize code", err)
	}
	if err := st.CreateAuthorizeCodeSession(h.ctx, sig, req); err != nil {
		fatal("code session", err)
	}
	if err := st.CreatePKCERequestSession(h.ctx, sig, req); err != nil {
		fatal("pkce session", err)
	}
	return code
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r2)
}

func (h *harness) phaseMCP() {
	h.fixtures()
	h.openDB(daysAgo(30))
	var txn string
	var want int
	err := h.db.Writer().QueryRowContext(h.ctx, `SELECT txn_id, (SELECT count(*) FROM decisions d2 WHERE d2.plan_id = d.plan_id AND d2.txn_id = d.txn_id)
		FROM decisions d WHERE plan_id = ? AND outcome = 'applied' AND superseded_by IS NULL ORDER BY id DESC LIMIT 1`, h.sb).Scan(&txn, &want)
	if err != nil {
		fatal("pick a decided transaction (run the transition phase first)", err)
	}
	if err := h.db.Close(); err != nil {
		fatal("close", err)
	}
	pub := fmt.Sprintf("http://127.0.0.1:%d", freePort())
	email := "m3-tester@example.test"
	sp := h.startServe("mcp", map[string]string{
		"MCP_LISTEN_ADDR": strings.TrimPrefix(pub, "http://"), "MCP_PUBLIC_URL": pub,
		"GOOGLE_OAUTH_CLIENT_ID": "m3-test-client-id", "GOOGLE_OAUTH_CLIENT_SECRET": "m3-canary-google-secret-5551212",
		"MCP_ALLOWED_EMAILS": email,
	}, 90*time.Second)
	defer sp.stop()
	ready := false
	for i := 0; i < 120 && !ready; i++ {
		if r, err := http.Get(pub + "/.well-known/oauth-protected-resource"); err == nil {
			r.Body.Close()
			ready = r.StatusCode == http.StatusOK
		}
		if !ready {
			time.Sleep(250 * time.Millisecond)
		}
	}
	check(ready, "MCP listener ready (Google discovery done)")

	r, err := http.Post(pub+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	check(err == nil && r.StatusCode == http.StatusUnauthorized, "POST /mcp without a token: 401")
	if r != nil {
		r.Body.Close()
	}

	redirect := "http://127.0.0.1:59999/cb"
	body, _ := json.Marshal(map[string]any{"client_name": "m3-acceptance", "redirect_uris": []string{redirect}})
	r, err = http.Post(pub+"/oauth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		fatal("register", err)
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reg)
	r.Body.Close()
	check(r.StatusCode == http.StatusCreated && reg.ClientID != "", "dynamic client registration: 201")
	verifier := rand.Text() + rand.Text()
	sum := sha256.Sum256([]byte(verifier))
	code := h.mintCode(reg.ClientID, redirect, email, base64.RawURLEncoding.EncodeToString(sum[:]))
	r, err = http.PostForm(pub+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {reg.ClientID},
		"redirect_uri": {redirect}, "code": {code}, "code_verifier": {verifier}})
	if err != nil {
		fatal("token", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&tok)
	r.Body.Close()
	check(r.StatusCode == http.StatusOK && tok.AccessToken != "", "PKCE code exchange: access token issued")
	scrubs = append(scrubs, tok.AccessToken, code, verifier)

	ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
	defer cancel()
	cl := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "m3-acceptance", Version: "1.0.0"}, nil)
	sess, err := cl.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: pub + "/mcp", HTTPClient: &http.Client{Transport: bearer{tok.AccessToken}}}, nil)
	if err != nil {
		fatal("mcp connect", err)
	}
	defer sess.Close()
	tools, err := sess.ListTools(ctx, nil)
	found := false
	if err == nil {
		for _, t := range tools.Tools {
			found = found || t.Name == "get_category_decisions"
		}
	}
	check(found, "tools/list includes get_category_decisions")

	call := func(id string) (decs []map[string]any, fresh map[string]any, isErr bool, text string) {
		res, err := sess.CallTool(ctx, &sdkmcp.CallToolParams{Name: "get_category_decisions", Arguments: map[string]any{"transaction_id": id}})
		if err != nil {
			fatal("call tool", err)
		}
		for _, c := range res.Content {
			if tc, ok := c.(*sdkmcp.TextContent); ok {
				text += tc.Text
			}
		}
		if res.IsError {
			return nil, nil, true, text
		}
		var env struct {
			Data struct {
				Decisions []map[string]any `json:"decisions"`
			} `json:"data"`
			Fresh map[string]any `json:"data_freshness"`
		}
		_ = json.Unmarshal([]byte(text), &env)
		return env.Data.Decisions, env.Fresh, false, text
	}
	decs, fresh, isErr, _ := call(txn)
	check(!isErr && len(decs) == want, "decided transaction: %d decisions returned (want %d)", len(decs), want)
	if len(decs) > 0 {
		check(decs[0]["outcome"] == "applied" && decs[0]["mode"] == "auto" && decs[0]["model"] == fakeModelID,
			"newest first: current decision is applied/auto by the recorded model (%v/%v)", decs[0]["outcome"], decs[0]["mode"])
		check(decs[len(decs)-1]["mode"] == "dry_run", "oldest decision is the superseded dry_run one (%v)", decs[len(decs)-1]["mode"])
		_, hasConf := decs[0]["confidence"]
		check(hasConf && decs[0]["category"] != nil, "decision carries category and confidence")
	}
	check(fresh != nil && fresh["last_synced_at"] != nil, "response carries data_freshness")
	decs, _, isErr, _ = call("00000000-0000-4000-8000-000000000000")
	check(!isErr && len(decs) == 0, "unknown transaction: empty decision list, not an error")
	_, _, isErr, text := call("bad id")
	check(isErr && !strings.Contains(text, h.sb), "malformed transaction id: a client error that leaks nothing")
	log := h.serveLog("mcp")
	check(!strings.Contains(log, "m3-canary-google-secret-5551212") && !strings.Contains(log, email), "serve log has no Google secret or email")
}
