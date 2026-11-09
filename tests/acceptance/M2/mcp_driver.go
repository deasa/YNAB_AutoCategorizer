// Command M2 runs black-box acceptance checks against a local finbot binary and
// a local SQLite database populated by a read-only `finbot sync` (scope 3).
// It never writes to YNAB and never prints secrets, ids, tokens or emails.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"

	"github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

var (
	uuidRE  = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

type runner struct {
	bin        string
	dbPath     string
	workDir    string
	ynabToken  string
	budgetID   string
	passed     int
	failed     int
	openAcct   string
	secondAcct string
	closedAcct string
}

func (r *runner) ok(msg string) {
	r.passed++
	fmt.Printf("PASS: %s\n", msg)
}

func (r *runner) bad(msg string, args ...any) {
	r.failed++
	fmt.Fprintf(os.Stderr, "FAIL: "+msg+"\n", args...)
}

func (r *runner) check(cond bool, msg string, args ...any) {
	if cond {
		r.ok(fmt.Sprintf(msg, args...))
	} else {
		r.bad(msg, args...)
	}
}

func main() {
	var bin, dbPath, workDir string
	flag.StringVar(&bin, "bin", "", "path to built finbot binary")
	flag.StringVar(&dbPath, "db", "", "path to synced local SQLite database")
	flag.StringVar(&workDir, "workdir", "", "empty working directory with no .env")
	flag.Parse()

	if bin == "" || dbPath == "" || workDir == "" {
		fmt.Fprintln(os.Stderr, "usage: mcp_driver -bin <path> -db <path> -workdir <dir>")
		os.Exit(2)
	}
	ynabToken := os.Getenv("YNAB_ACCESS_TOKEN")
	budgetID := os.Getenv("YNAB_BUDGET_ID")
	if ynabToken == "" || budgetID == "" {
		fmt.Fprintln(os.Stderr, "YNAB_ACCESS_TOKEN and YNAB_BUDGET_ID are required in env")
		os.Exit(2)
	}

	r := &runner{
		bin:       bin,
		dbPath:    dbPath,
		workDir:   workDir,
		ynabToken: ynabToken,
		budgetID:  budgetID,
	}

	r.testPermissionsAndTagAccountCLI()
	r.testRevokeCLIStandalone()
	r.testServeOAuthAndMCP()

	fmt.Printf("SUMMARY: passed=%d failed=%d\n", r.passed, r.failed)
	if r.failed > 0 {
		os.Exit(1)
	}
}

func (r *runner) runCmd(env []string, args ...string) (int, string, string) {
	cmd := exec.Command(r.bin, args...)
	cmd.Dir = r.workDir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + r.workDir}, env...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return code, out.String(), errBuf.String()
}

func (r *runner) testPermissionsAndTagAccountCLI() {
	info, err := os.Stat(r.dbPath)
	r.check(err == nil && info.Mode().Perm() == 0o600, "initial synced DB file mode is 0600")

	// Set 0644 on DB, -wal, -shm and confirm opening tightens all of them to 0600 (M2-0).
	for _, suf := range []string{"", "-wal", "-shm"} {
		_ = os.WriteFile(r.dbPath+suf+".touch", nil, 0o600)
		_ = os.Remove(r.dbPath + suf + ".touch")
		if _, err := os.Stat(r.dbPath + suf); err == nil {
			_ = os.Chmod(r.dbPath+suf, 0o644)
		}
	}

	tagEnv := []string{
		"FINBOT_DB_PATH=" + r.dbPath,
		"YNAB_BUDGET_ID=" + r.budgetID,
	}
	code, out, errOut := r.runCmd(tagEnv, "tag-account")
	r.check(code == 0, "finbot tag-account (no args) exits 0 with only FINBOT_DB_PATH and YNAB_BUDGET_ID")
	r.check(!uuidRE.MatchString(out+errOut), "finbot tag-account output contains no UUIDs")

	for _, suf := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(r.dbPath + suf); err == nil {
			r.check(st.Mode().Perm() == 0o600, "DB file %s tightened from 0644 to 0600 on open", suf)
		}
	}

	codeList, outList, _ := r.runCmd(tagEnv, "tag-account", "--list")
	r.check(codeList == 0 && outList == out, "finbot tag-account --list matches no-arg output")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	r.check(len(lines) >= 3, "finbot tag-account lists accounts (%d lines)", len(lines))
	allUntagged := true
	for _, line := range lines {
		if !strings.HasPrefix(line, "untagged ") {
			allUntagged = false
		}
		name := strings.TrimSpace(strings.TrimPrefix(line, "untagged"))
		if strings.HasSuffix(name, " (closed)") {
			if r.closedAcct == "" {
				r.closedAcct = strings.TrimSuffix(name, " (closed)")
			}
		} else if r.openAcct == "" {
			r.openAcct = name
		} else if r.secondAcct == "" {
			r.secondAcct = name
		}
	}
	r.check(allUntagged, "all accounts initially listed as untagged")
	r.check(r.openAcct != "" && r.secondAcct != "", "identified two open accounts for tag tests")

	// Set tags on open accounts and a closed account (if present).
	code, out, errOut = r.runCmd(tagEnv, "tag-account", strings.ToUpper(r.openAcct), "taxable")
	r.check(code == 0 && strings.HasPrefix(out, "tagged "), "finbot tag-account sets taxable case-insensitively")
	r.check(!uuidRE.MatchString(out+errOut), "finbot tag-account set emits no UUIDs")

	code, _, _ = r.runCmd(tagEnv, "tag-account", r.secondAcct, "none")
	r.check(code == 0, "finbot tag-account sets none bucket")

	if r.closedAcct != "" {
		code, _, _ = r.runCmd(tagEnv, "tag-account", r.closedAcct, "cash")
		r.check(code == 0, "finbot tag-account tags closed account as cash")
	}

	// Verify tags in listing.
	code, out, _ = r.runCmd(tagEnv, "tag-account")
	r.check(code == 0 && strings.Contains(out, "taxable ") && strings.Contains(out, "none "), "finbot tag-account listing reflects updated tags")

	// Error cases.
	code, _, _ = r.runCmd(tagEnv, "tag-account", r.openAcct, "crypto")
	r.check(code == 2, "finbot tag-account rejects unknown bucket (exit 2)")

	code, _, _ = r.runCmd(tagEnv, "tag-account", "nonexistent-account-name-99999", "roth")
	r.check(code == 1, "finbot tag-account rejects unknown account name (exit 1)")

	code, _, _ = r.runCmd([]string{"FINBOT_DB_PATH=" + r.dbPath}, "tag-account")
	r.check(code == 1, "finbot tag-account without YNAB_BUDGET_ID exits 1")

	code, _, _ = r.runCmd([]string{"FINBOT_DB_PATH=" + r.workDir + "/missing.db", "YNAB_BUDGET_ID=" + r.budgetID}, "tag-account")
	r.check(code == 1, "finbot tag-account with missing DB file exits 1")
}

func (r *runner) testRevokeCLIStandalone() {
	revEnv := []string{"FINBOT_DB_PATH=" + r.dbPath}

	code, _, _ := r.runCmd(revEnv, "revoke")
	r.check(code == 2, "finbot revoke with no args exits 2")

	code, _, _ = r.runCmd(revEnv, "revoke", "not-an-email-or-hash")
	r.check(code == 2, "finbot revoke rejects malformed target (exit 2)")

	code, _, _ = r.runCmd(revEnv, "revoke", "--badflag")
	r.check(code == 2, "finbot revoke rejects unknown flag (exit 2)")

	code, _, _ = r.runCmd([]string{"FINBOT_DB_PATH=" + r.workDir + "/missing.db"}, "revoke", "--all")
	r.check(code == 1, "finbot revoke with missing DB file exits 1 without creating DB")
	_, statErr := os.Stat(r.workDir + "/missing.db")
	r.check(os.IsNotExist(statErr), "finbot revoke did not create missing DB file")

	code, out, errOut := r.runCmd(revEnv, "revoke", "u_000000000000")
	r.check(code == 0 && strings.Contains(out, "no active tokens for u_000000000000"), "finbot revoke on user with 0 tokens exits 0 and warns of possible typo")
	r.check(!uuidRE.MatchString(out+errOut), "finbot revoke output contains no UUIDs")
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

type serveProc struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	done   chan error
}

func (r *runner) startServe(httpAddr, mcpAddr, publicURL string) (*serveProc, error) {
	cmd := exec.Command(r.bin, "serve")
	cmd.Dir = r.workDir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.workDir,
		"FINBOT_DB_PATH=" + r.dbPath,
		"YNAB_ACCESS_TOKEN=" + r.ynabToken,
		"YNAB_BUDGET_ID=" + r.budgetID,
		"SYNC_INTERVAL=24h",
		"IMPORT_INTERVAL=6h",
		"IMPORT_ENABLED=false",
		"SLACK_BOT_TOKEN=",
		"HEALTHCHECK_PING_URL=",
		"BACKUP_AGE_RECIPIENT=",
		"BACKUP_TURSO_URL=",
		"BACKUP_TURSO_TOKEN=",
		"HTTP_ADDR=" + httpAddr,
		"MCP_LISTEN_ADDR=" + mcpAddr,
		"MCP_PUBLIC_URL=" + publicURL,
		"GOOGLE_OAUTH_CLIENT_ID=test-google-client-id",
		"GOOGLE_OAUTH_CLIENT_SECRET=canary-google-secret-987654321",
		"MCP_ALLOWED_EMAILS=alice@example.test,bob@example.test",
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	sp := &serveProc{cmd: cmd, stderr: &stderr, done: make(chan error, 1)}
	go func() { sp.done <- cmd.Wait() }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(publicURL + "/.well-known/oauth-protected-resource")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				// Also wait for initial sync to mark /healthz 200.
				hz, err := http.Get("http://" + httpAddr + "/healthz")
				if err == nil {
					hz.Body.Close()
					if hz.StatusCode == http.StatusOK {
						return sp, nil
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	return nil, fmt.Errorf("serve did not become ready within 20s")
}

func (sp *serveProc) stop() error {
	_ = sp.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-sp.done:
		return err
	case <-time.After(8 * time.Second):
		_ = sp.cmd.Process.Kill()
		return fmt.Errorf("serve did not exit within 8s after SIGTERM")
	}
}

// mintAuthCode creates a PKCE S256 authorization code in the SQLite DB for clientID and email,
// matching what handleConsent writes after Google sign-in + user approval.
func (r *runner) mintAuthCode(ctx context.Context, clientID, redirectURI, email, displayName, challenge string) (string, error) {
	db, err := store.Open(ctx, r.dbPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	st := auth.New(db)
	secret, err := st.HMACSecret(ctx)
	if err != nil {
		return "", err
	}
	fc := &fosite.Config{
		AccessTokenLifespan:   time.Hour,
		RefreshTokenLifespan:  30 * 24 * time.Hour,
		AuthorizeCodeLifespan: 5 * time.Minute,
		GlobalSecret:          secret,
	}
	strat := compose.NewOAuth2HMACStrategy(fc)
	client, err := st.GetClient(ctx, clientID)
	if err != nil {
		return "", err
	}
	uid := auth.UserID(email)
	gen, err := st.CurrentGeneration(ctx, uid)
	if err != nil {
		return "", err
	}
	sess := auth.NewSession(uid, displayName, gen)
	now := time.Now().UTC()
	sess.SetExpiresAt(fosite.AuthorizeCode, now.Add(5*time.Minute))
	sess.SetExpiresAt(fosite.AccessToken, now.Add(time.Hour))
	sess.SetExpiresAt(fosite.RefreshToken, now.Add(30*24*time.Hour))

	req := &fosite.Request{
		ID:             rand.Text(),
		RequestedAt:    now,
		Client:         client,
		RequestedScope: fosite.Arguments{auth.ScopeMCP, "offline_access"},
		GrantedScope:   fosite.Arguments{auth.ScopeMCP, "offline_access"},
		Form: url.Values{
			"client_id":             {clientID},
			"redirect_uri":          {redirectURI},
			"response_type":         {"code"},
			"scope":                 {"mcp offline_access"},
			"code_challenge":        {challenge},
			"code_challenge_method": {"S256"},
		},
		Session: sess,
	}
	code, sig, err := strat.GenerateAuthorizeCode(ctx, req)
	if err != nil {
		return "", err
	}
	if err := st.CreateAuthorizeCodeSession(ctx, sig, req); err != nil {
		return "", err
	}
	if err := st.CreatePKCERequestSession(ctx, sig, req); err != nil {
		return "", err
	}
	return code, nil
}

func pkcePair() (verifier, challenge string) {
	verifier = rand.Text() + rand.Text()
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func registerDCR(baseURL, name, redirectURI string) (int, string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":   name,
		"redirect_uris": []string{redirectURI},
	})
	resp, err := http.Post(baseURL+"/oauth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	var out struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out.ClientID, nil
}

type tokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
}

func exchangeCode(baseURL, clientID, redirectURI, code, verifier string) (int, tokenResp, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code":          {code},
		"code_verifier": {verifier},
	}
	resp, err := http.PostForm(baseURL+"/oauth/token", form)
	if err != nil {
		return 0, tokenResp{}, err
	}
	defer resp.Body.Close()
	var tr tokenResp
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	return resp.StatusCode, tr, nil
}

func refreshTokenGrant(baseURL, clientID, refreshTok string) (int, tokenResp, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshTok},
	}
	resp, err := http.PostForm(baseURL+"/oauth/token", form)
	if err != nil {
		return 0, tokenResp{}, err
	}
	defer resp.Body.Close()
	var tr tokenResp
	_ = json.NewDecoder(resp.Body).Decode(&tr)
	return resp.StatusCode, tr, nil
}

func mcpStatusWithToken(baseURL, token string) int {
	req, _ := http.NewRequest("POST", baseURL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r2)
}

func (r *runner) testServeOAuthAndMCP() {
	ctx := context.Background()
	p1, err := freePort()
	if err != nil {
		r.bad("allocate http port: %v", err)
		return
	}
	p2, err := freePort()
	if err != nil {
		r.bad("allocate mcp port: %v", err)
		return
	}
	httpAddr := fmt.Sprintf("127.0.0.1:%d", p1)
	mcpAddr := fmt.Sprintf("127.0.0.1:%d", p2)
	publicURL := "http://" + mcpAddr
	redirectURI := "http://127.0.0.1:59999/cb"

	sp, err := r.startServe(httpAddr, mcpAddr, publicURL)
	if err != nil {
		r.bad("start finbot serve: %v", err)
		return
	}
	defer func() { _ = sp.stop() }()
	r.ok("finbot serve started with MCP enabled and initial sync healthy")

	// 1. /healthz on HTTP_ADDR vs MCP_LISTEN_ADDR, and `finbot health`
	hzResp, err := http.Get("http://" + httpAddr + "/healthz")
	r.check(err == nil && hzResp.StatusCode == 200, "GET /healthz on HTTP_ADDR returns 200")
	if hzResp != nil {
		hzResp.Body.Close()
	}
	mcpHzResp, err := http.Get(publicURL + "/healthz")
	r.check(err == nil && mcpHzResp.StatusCode == 404, "GET /healthz on MCP listener returns 404")
	if mcpHzResp != nil {
		mcpHzResp.Body.Close()
	}
	code, _, _ := r.runCmd([]string{"HTTP_ADDR=" + httpAddr}, "health")
	r.check(code == 0, "finbot health exits 0 against HTTP_ADDR")

	// 2. DCR registration + PKCE code exchange + wrong verifier + code replay
	st, clientID, err := registerDCR(publicURL, "local-test-client", redirectURI)
	r.check(err == nil && st == 201 && clientID != "", "POST /oauth/register creates DCR client (201)")

	_, challenge1 := pkcePair()
	authCode1, err := r.mintAuthCode(ctx, clientID, redirectURI, "alice@example.test", "Alice", challenge1)
	r.check(err == nil && authCode1 != "", "minted PKCE authorization code in SQLite")

	stBadPKCE, trBad, err := exchangeCode(publicURL, clientID, redirectURI, authCode1, "wrong-verifier-value-1234567890")
	r.check(err == nil && stBadPKCE == 400 && trBad.Error == "invalid_grant", "POST /oauth/token rejects wrong PKCE code_verifier (400 invalid_grant)")

	// Mint a fresh code after failed PKCE attempt, exchange it, then replay the same code.
	verifier2, challenge2 := pkcePair()
	authCode2, err := r.mintAuthCode(ctx, clientID, redirectURI, "alice@example.test", "Alice", challenge2)
	r.check(err == nil, "minted second PKCE authorization code")

	stOK, trOK, err := exchangeCode(publicURL, clientID, redirectURI, authCode2, verifier2)
	r.check(err == nil && stOK == 200 && trOK.AccessToken != "" && trOK.RefreshToken != "", "POST /oauth/token exchanges valid code + PKCE verifier for access and refresh tokens")

	stReplayCode, trReplayCode, err := exchangeCode(publicURL, clientID, redirectURI, authCode2, verifier2)
	r.check(err == nil && stReplayCode == 400 && trReplayCode.Error == "invalid_grant", "replaying used authorization code returns 400 invalid_grant")
	r.check(mcpStatusWithToken(publicURL, trOK.AccessToken) == 401, "replaying used authorization code revokes tokens issued from that code")

	// 3. Mint live grants for Alice and Bob
	vAlice, cAlice := pkcePair()
	codeAlice, err := r.mintAuthCode(ctx, clientID, redirectURI, "alice@example.test", "Alice", cAlice)
	r.check(err == nil, "minted live authorization code for Alice")
	stA, tokAlice, err := exchangeCode(publicURL, clientID, redirectURI, codeAlice, vAlice)
	r.check(err == nil && stA == 200 && tokAlice.AccessToken != "", "issued live tokens for Alice")

	vBob, cBob := pkcePair()
	codeBob, err := r.mintAuthCode(ctx, clientID, redirectURI, "bob@example.test", "Bob", cBob)
	r.check(err == nil, "minted live authorization code for Bob")
	stB, tokBob, err := exchangeCode(publicURL, clientID, redirectURI, codeBob, vBob)
	r.check(err == nil && stB == 200 && tokBob.AccessToken != "", "issued live tokens for Bob")

	// 4. Test 500 unused DCR clients eviction without evicting live grant (M2-1a)
	allRegistered := true
	for i := range 505 {
		stReg, _, err := registerDCR(publicURL, fmt.Sprintf("unused-%d", i), redirectURI)
		if err != nil || stReg != 201 {
			allRegistered = false
			break
		}
	}
	r.check(allRegistered, "505 unused DCR registrations succeed past the 500 cap by evicting unused clients")
	r.check(mcpStatusWithToken(publicURL, tokAlice.AccessToken) != 401, "client with live grant survives DCR cap eviction")

	// 5. Restart persistence (M2-1a, M2-1b, M2-2): SIGTERM serve, restart on same DB, verify tokens & client survive
	err = sp.stop()
	r.check(err == nil, "SIGTERM cleanly stops finbot serve and closes listeners")
	firstLogs := sp.stderr.String()

	sp, err = r.startServe(httpAddr, mcpAddr, publicURL)
	if err != nil {
		r.bad("restart finbot serve: %v", err)
		return
	}
	r.ok("finbot serve restarted on the same SQLite database")

	// 6. Connect MCP SDK client with Alice's pre-restart token and test all 6 tools
	r.testAllMCPTools(ctx, publicURL, tokAlice.AccessToken)

	// 7. Refresh token rotation and replay detection across restart (M2-1a, M2-1b)
	stRef, tokAlice2, err := refreshTokenGrant(publicURL, clientID, tokAlice.RefreshToken)
	r.check(err == nil && stRef == 200 && tokAlice2.AccessToken != "" && tokAlice2.RefreshToken != "", "POST /oauth/token rotates Alice's pre-restart refresh token")
	r.check(mcpStatusWithToken(publicURL, tokAlice.AccessToken) == 401, "old access token is rejected (401) after refresh rotation")
	r.check(mcpStatusWithToken(publicURL, tokAlice2.AccessToken) != 401, "new access token works after refresh rotation")

	stRefReplay, trRefReplay, err := refreshTokenGrant(publicURL, clientID, tokAlice.RefreshToken)
	r.check(err == nil && stRefReplay == 400 && trRefReplay.Error == "invalid_grant", "replaying rotated refresh token is rejected (400 invalid_grant)")
	r.check(mcpStatusWithToken(publicURL, tokAlice2.AccessToken) == 401, "replaying rotated refresh token revokes the grant's new access token")

	// 8. RFC 7009 /oauth/revoke
	vAlice3, cAlice3 := pkcePair()
	codeAlice3, _ := r.mintAuthCode(ctx, clientID, redirectURI, "alice@example.test", "Alice", cAlice3)
	_, tokAlice3, _ := exchangeCode(publicURL, clientID, redirectURI, codeAlice3, vAlice3)
	r.check(mcpStatusWithToken(publicURL, tokAlice3.AccessToken) != 401, "fresh Alice grant works before RFC 7009 revoke")
	revResp, err := http.PostForm(publicURL+"/oauth/revoke", url.Values{"token": {tokAlice3.RefreshToken}, "client_id": {clientID}})
	if err == nil {
		revResp.Body.Close()
	}
	r.check(err == nil && revResp.StatusCode == 200, "POST /oauth/revoke returns 200")
	r.check(mcpStatusWithToken(publicURL, tokAlice3.AccessToken) == 401, "RFC 7009 revoke invalidates grant's access token")

	// 9. Live `finbot revoke` CLI while serve is running (no restart!)
	vAlice4, cAlice4 := pkcePair()
	codeAlice4, _ := r.mintAuthCode(ctx, clientID, redirectURI, "alice@example.test", "Alice", cAlice4)
	_, tokAlice4, _ := exchangeCode(publicURL, clientID, redirectURI, codeAlice4, vAlice4)
	r.check(mcpStatusWithToken(publicURL, tokAlice4.AccessToken) != 401, "Alice's 4th grant works before CLI revoke")
	r.check(mcpStatusWithToken(publicURL, tokBob.AccessToken) != 401, "Bob's grant works before CLI revoke")

	revCode, revOut, revErr := r.runCmd([]string{"FINBOT_DB_PATH=" + r.dbPath}, "revoke", "alice@example.test")
	r.check(revCode == 0 && strings.Contains(revOut, "revoked 3 active tokens for "+auth.UserID("alice@example.test")),
		"finbot revoke <email> counts 3 active tokens (access + refresh + unredeemed code) and prints u_ hash, never email")
	r.check(!emailRE.MatchString(revOut+revErr), "finbot revoke output contains no email address")
	r.check(mcpStatusWithToken(publicURL, tokAlice4.AccessToken) == 401, "finbot revoke immediately invalidates Alice's access token without server restart")
	stAfterRev, _, _ := refreshTokenGrant(publicURL, clientID, tokAlice4.RefreshToken)
	r.check(stAfterRev == 400, "finbot revoke immediately invalidates Alice's refresh token without server restart")
	r.check(mcpStatusWithToken(publicURL, tokBob.AccessToken) != 401, "finbot revoke for Alice leaves Bob's tokens valid")

	// Fresh sign-in for Alice after revoke works.
	vAlice5, cAlice5 := pkcePair()
	codeAlice5, _ := r.mintAuthCode(ctx, clientID, redirectURI, "alice@example.test", "Alice", cAlice5)
	_, tokAlice5, _ := exchangeCode(publicURL, clientID, redirectURI, codeAlice5, vAlice5)
	r.check(mcpStatusWithToken(publicURL, tokAlice5.AccessToken) != 401, "fresh sign-in for Alice works after revoke")

	// `finbot revoke --all` invalidates both Alice and Bob.
	revAllCode, revAllOut, _ := r.runCmd([]string{"FINBOT_DB_PATH=" + r.dbPath}, "revoke", "--all")
	r.check(revAllCode == 0 && strings.Contains(revAllOut, "revoked 4 active tokens for all users"), "finbot revoke --all revokes all active tokens across users")
	r.check(mcpStatusWithToken(publicURL, tokAlice5.AccessToken) == 401 && mcpStatusWithToken(publicURL, tokBob.AccessToken) == 401,
		"finbot revoke --all immediately invalidates both Alice's and Bob's tokens")

	// 10. Stop serve and verify logs contain expected events and zero secrets/UUIDs/emails.
	err = sp.stop()
	r.check(err == nil, "second finbot serve stops cleanly on SIGTERM")
	allLogs := firstLogs + "\n" + sp.stderr.String()
	r.check(strings.Contains(allLogs, `"msg":"oauth prune"`), "serve logs show oauth prune ran at startup")
	r.check(strings.Contains(allLogs, `"msg":"import disabled"`), "serve logs show import disabled locally")
	r.check(strings.Contains(allLogs, `"msg":"mcp serving"`), "serve logs show mcp serving")
	r.check(!strings.Contains(allLogs, "canary-google-secret-987654321"), "Google client secret never appears in logs")
	r.check(!strings.Contains(allLogs, r.ynabToken), "YNAB access token never appears in logs")
	r.check(!strings.Contains(allLogs, r.budgetID), "YNAB budget ID never appears in logs")
	r.check(!uuidRE.MatchString(allLogs), "serve logs contain 0 UUIDs")
	r.check(!emailRE.MatchString(allLogs), "serve logs contain 0 email addresses")
	r.check(!strings.Contains(allLogs, tokAlice.AccessToken) && !strings.Contains(allLogs, tokAlice.RefreshToken), "raw OAuth tokens never appear in logs")
}

func callToolJSON[T any](ctx context.Context, session *sdkmcp.ClientSession, name string, args map[string]any) (T, map[string]any, bool, string, error) {
	var zero T
	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return zero, nil, false, "", err
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*sdkmcp.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError {
		return zero, nil, true, text, nil
	}
	var env struct {
		Data          T              `json:"data"`
		DataFreshness map[string]any `json:"data_freshness"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		return zero, nil, false, text, err
	}
	return env.Data, env.DataFreshness, false, text, nil
}

func checkFreshness(r *runner, toolName string, f map[string]any) {
	raw, _ := json.Marshal(f)
	hasSync := f["last_synced_at"] != nil && f["last_synced_at"] != ""
	stale, _ := f["stale"].(bool)
	_, hasLinksOK := f["links_ok"].(bool)
	accts, hasAccts := f["accounts"].([]any)
	r.check(hasSync && !stale && hasLinksOK && hasAccts && len(accts) > 0,
		"%s response includes populated data_freshness (stale=false, accounts=%d)", toolName, len(accts))
	r.check(!uuidRE.Match(raw), "%s data_freshness contains 0 UUIDs", toolName)
}

func parseMilli(s string) (int64, bool) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, false
	}
	var f int64
	if frac != "" {
		f, err = strconv.ParseInt((frac+"000")[:3], 10, 64)
		if err != nil {
			return 0, false
		}
	}
	m := w*1000 + f
	if neg {
		m = -m
	}
	return m, true
}

func (r *runner) testAllMCPTools(ctx context.Context, publicURL, token string) {
	httpClient := &http.Client{
		Transport: &bearerRoundTripper{token: token, base: http.DefaultTransport},
		Timeout:   15 * time.Second,
	}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "m2-acceptance", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint:   publicURL + "/mcp",
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		r.bad("connect MCP client after restart: %v", err)
		return
	}
	defer session.Close()
	r.ok("MCP client connected with pre-restart access token")

	// 1. tools/list
	toolsRes, err := session.ListTools(ctx, nil)
	if err != nil {
		r.bad("tools/list: %v", err)
		return
	}
	var names []string
	for _, t := range toolsRes.Tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	wantNames := []string{
		"compare_spending",
		"data_freshness",
		"get_financial_snapshot",
		"get_goals",
		"get_income_events",
		"query_transactions",
	}
	r.check(strings.Join(names, ",") == strings.Join(wantNames, ","), "tools/list returns all 6 expected read tools")

	// 2. data_freshness
	var dfData struct {
		StaleAfterSeconds    int64 `json:"stale_after_seconds"`
		ImportStaleAfterDays int   `json:"import_stale_after_days"`
	}
	dfData, dfFresh, isErr, _, err := callToolJSON[struct {
		StaleAfterSeconds    int64 `json:"stale_after_seconds"`
		ImportStaleAfterDays int   `json:"import_stale_after_days"`
	}](ctx, session, "data_freshness", nil)
	r.check(err == nil && !isErr && dfData.ImportStaleAfterDays == 14 && dfData.StaleAfterSeconds > 0,
		"data_freshness tool returns import_stale_after_days=14 and stale_after_seconds")
	checkFreshness(r, "data_freshness", dfFresh)

	// 3. query_transactions
	type qLine struct {
		TransactionID    string `json:"transaction_id"`
		SubtransactionID string `json:"subtransaction_id"`
		Date             string `json:"date"`
		Amount           string `json:"amount"`
		Account          string `json:"account"`
		Category         string `json:"category"`
		CategoryID       string `json:"category_id"`
		Approved         bool   `json:"approved"`
	}
	type qOut struct {
		Lines      []qLine `json:"lines"`
		MatchCount int     `json:"match_count"`
		MatchTotal string  `json:"match_total"`
		NextCursor string  `json:"next_cursor"`
	}
	q1, q1Fresh, isErr, _, err := callToolJSON[qOut](ctx, session, "query_transactions", nil)
	r.check(err == nil && !isErr && len(q1.Lines) == 50 && q1.MatchCount > 200 && q1.NextCursor != "",
		"query_transactions default returns 50 lines with match_count=%d and next_cursor", q1.MatchCount)
	checkFreshness(r, "query_transactions", q1Fresh)

	// Page size cap at 200.
	qCap, _, isErr, _, err := callToolJSON[qOut](ctx, session, "query_transactions", map[string]any{"page_size": 1000})
	r.check(err == nil && !isErr && len(qCap.Lines) == 200, "query_transactions caps page_size=1000 at 200")

	// Cursor paging across 3 pages of size 10: no duplicates, descending dates, filter-bound cursor.
	seen := map[string]bool{}
	cursor := ""
	prevDate := "9999-99-99"
	orderOK := true
	for page := range 3 {
		args := map[string]any{"page_size": 10}
		if cursor != "" {
			args["cursor"] = cursor
		}
		qp, _, isErr, _, err := callToolJSON[qOut](ctx, session, "query_transactions", args)
		if err != nil || isErr || len(qp.Lines) != 10 {
			r.bad("query_transactions page %d failed", page)
			orderOK = false
			break
		}
		for _, l := range qp.Lines {
			key := l.TransactionID + "/" + l.SubtransactionID
			if seen[key] || l.Date > prevDate {
				orderOK = false
			}
			seen[key] = true
			prevDate = l.Date
		}
		cursor = qp.NextCursor
	}
	r.check(orderOK && len(seen) == 30, "query_transactions cursor paging returns 30 distinct lines in descending date order")

	// Reusing cursor with different filters is refused.
	_, _, isErr, errText, err := callToolJSON[qOut](ctx, session, "query_transactions", map[string]any{
		"page_size": 10,
		"cursor":    cursor,
		"start":     "2025-01-01",
	})
	r.check(err == nil && isErr && strings.Contains(errText, "cursor"), "query_transactions refuses cursor reused with different filters")

	// Verify against store that splits are expanded under child categories and match_count/match_total match.
	db, err := store.Open(ctx, r.dbPath)
	if err == nil {
		defer db.Close()
		_, sum, qErr := db.QueryLines(ctx, r.budgetID, store.LineFilter{}, nil, 1)
		totMilli, ok := parseMilli(q1.MatchTotal)
		r.check(qErr == nil && ok && sum.Count == q1.MatchCount && sum.Total == totMilli,
			"query_transactions match_count and match_total equal split-aware store.QueryLines summary")
	}

	// Filter by an account and year that actually have transactions in the synced mirror,
	// and verify nonexistent account/category returns client error.
	acctWithTx := q1.Lines[0].Account
	txYear := q1.Lines[0].Date[:4]
	qAcct, _, isErr, _, err := callToolJSON[qOut](ctx, session, "query_transactions", map[string]any{
		"account": acctWithTx,
		"start":   txYear + "-01-01",
		"end":     txYear + "-12-31",
	})
	r.check(err == nil && !isErr && qAcct.MatchCount > 0, "query_transactions filters by account name and date range (matched %d lines)", qAcct.MatchCount)
	boundsOK := true
	for _, l := range qAcct.Lines {
		if l.Date < txYear+"-01-01" || l.Date > txYear+"-12-31" || !strings.EqualFold(l.Account, acctWithTx) || l.Category == "Split" {
			boundsOK = false
		}
	}
	r.check(boundsOK, "query_transactions lines respect inclusive date boundaries, account filter, and never expose Split sentinel")

	_, _, isErr, _, err = callToolJSON[qOut](ctx, session, "query_transactions", map[string]any{"account": "nonexistent-account-xyz"})
	r.check(err == nil && isErr, "query_transactions returns client error for unknown account name")

	_, _, isErr, _, err = callToolJSON[qOut](ctx, session, "query_transactions", map[string]any{"category": "nonexistent-category-xyz"})
	r.check(err == nil && isErr, "query_transactions returns client error for unknown category name")

	// 4. compare_spending
	type cDiff struct {
		CategoryID    string `json:"category_id"`
		Category      string `json:"category"`
		PeriodSpent   string `json:"period_spent"`
		BaselineSpent string `json:"baseline_spent"`
		Difference    string `json:"difference"`
	}
	type cOut struct {
		Period struct {
			Start string `json:"start"`
			End   string `json:"end"`
			Days  int    `json:"days"`
		} `json:"period"`
		Baseline struct {
			Start string `json:"start"`
			End   string `json:"end"`
			Days  int    `json:"days"`
		} `json:"baseline"`
		Categories    []cDiff `json:"categories"`
		PeriodTotal   string  `json:"period_total"`
		BaselineTotal string  `json:"baseline_total"`
		Difference    string  `json:"difference"`
	}
	cmp, cmpFresh, isErr, _, err := callToolJSON[cOut](ctx, session, "compare_spending", map[string]any{
		"period":   map[string]any{"month": txYear + "-10"},
		"baseline": map[string]any{"month": txYear + "-09"},
	})
	r.check(err == nil && !isErr && cmp.Period.Days == 31 && cmp.Baseline.Days == 30 && len(cmp.Categories) > 0,
		"compare_spending resolves month periods (%s-10: 31d vs %s-09: 30d, %d categories)", txYear, txYear, len(cmp.Categories))
	checkFreshness(r, "compare_spending", cmpFresh)

	var sumP, sumB int64
	mathOK := true
	prevAbs := int64(1 << 62)
	for _, c := range cmp.Categories {
		p, ok1 := parseMilli(c.PeriodSpent)
		b, ok2 := parseMilli(c.BaselineSpent)
		d, ok3 := parseMilli(c.Difference)
		if !ok1 || !ok2 || !ok3 || p-b != d || c.Category == "Split" {
			mathOK = false
		}
		absD := d
		if absD < 0 {
			absD = -absD
		}
		if absD > prevAbs {
			mathOK = false
		}
		prevAbs = absD
		sumP += p
		sumB += b
	}
	totP, _ := parseMilli(cmp.PeriodTotal)
	totB, _ := parseMilli(cmp.BaselineTotal)
	totD, _ := parseMilli(cmp.Difference)
	r.check(mathOK && sumP == totP && sumB == totB && totP-totB == totD,
		"compare_spending exact decimal arithmetic and sort order verified across %d categories", len(cmp.Categories))

	_, _, isErr, _, err = callToolJSON[cOut](ctx, session, "compare_spending", map[string]any{
		"period":   map[string]any{"month": "2025-10", "start": "2025-10-01"},
		"baseline": map[string]any{"month": "2025-09"},
	})
	r.check(err == nil && isErr, "compare_spending rejects period with both month and start/end")

	// 5. get_goals
	type gOut struct {
		Goals []struct {
			CategoryID string `json:"category_id"`
			Category   string `json:"category"`
			GoalType   string `json:"goal_type"`
			Balance    string `json:"balance"`
		} `json:"goals"`
	}
	goals, goalsFresh, isErr, _, err := callToolJSON[gOut](ctx, session, "get_goals", nil)
	r.check(err == nil && !isErr && len(goals.Goals) > 0, "get_goals returns %d live category goals", len(goals.Goals))
	checkFreshness(r, "get_goals", goalsFresh)

	// 6. get_income_events
	type incOut struct {
		Start  string `json:"start"`
		End    string `json:"end"`
		Kind   string `json:"kind"`
		Count  int    `json:"count"`
		Total  string `json:"total"`
		Events []struct {
			TransactionID string `json:"transaction_id"`
			Date          string `json:"date"`
			Amount        string `json:"amount"`
		} `json:"events"`
		Truncated bool `json:"truncated"`
	}
	inc, incFresh, isErr, _, err := callToolJSON[incOut](ctx, session, "get_income_events", map[string]any{
		"start": "2020-01-01",
		"end":   "2026-12-31",
		"limit": 1000,
	})
	r.check(err == nil && !isErr && strings.Contains(inc.Kind, "Not confirmed paychecks"),
		"get_income_events labels events as inflows, not confirmed paychecks (count=%d)", inc.Count)
	checkFreshness(r, "get_income_events", incFresh)
	var incSum int64
	incPositive := true
	for _, ev := range inc.Events {
		m, ok := parseMilli(ev.Amount)
		if !ok || m <= 0 {
			incPositive = false
		}
		incSum += m
	}
	incTot, _ := parseMilli(inc.Total)
	r.check(incPositive && (inc.Truncated || incSum == incTot), "get_income_events amounts are positive and sum to total")

	// 7. get_financial_snapshot
	type snapOut struct {
		NetWorth string `json:"net_worth"`
		Buckets  []struct {
			Bucket   string `json:"bucket"`
			Balance  string `json:"balance"`
			Accounts int    `json:"accounts"`
		} `json:"tax_buckets"`
		Accounts []struct {
			Name      string `json:"name"`
			Balance   string `json:"balance"`
			TaxBucket string `json:"tax_bucket"`
		} `json:"accounts"`
		Debts    []map[string]any `json:"debts"`
		CashFlow struct {
			Window struct {
				Start           string `json:"start"`
				End             string `json:"end"`
				MonthsRequested int    `json:"months_requested"`
				MonthsCovered   string `json:"months_covered"`
			} `json:"window"`
			AvgMonthlyIncome        string `json:"avg_monthly_income"`
			AvgMonthlySpending      string `json:"avg_monthly_spending"`
			AvgMonthlyCategorized   string `json:"avg_monthly_categorized_spending"`
			AvgMonthlyUncategorized string `json:"avg_monthly_uncategorized_spending"`
			CSPGrouping             string `json:"csp_grouping_proposed"`
		} `json:"cash_flow"`
		Goals struct {
			Count int `json:"count"`
		} `json:"goals"`
	}
	snap, snapFresh, isErr, _, err := callToolJSON[snapOut](ctx, session, "get_financial_snapshot", nil)
	r.check(err == nil && !isErr, "get_financial_snapshot default (12m) succeeds")
	checkFreshness(r, "get_financial_snapshot", snapFresh)

	var acctSum int64
	bucketSums := map[string]int64{}
	bucketCounts := map[string]int{}
	sawTaxable, sawNone, sawUntagged, sawClosed := false, false, false, false
	for _, a := range snap.Accounts {
		m, _ := parseMilli(a.Balance)
		acctSum += m
		bucketSums[a.TaxBucket] += m
		bucketCounts[a.TaxBucket]++
		if strings.EqualFold(a.Name, r.openAcct) && a.TaxBucket == "taxable" {
			sawTaxable = true
		}
		if strings.EqualFold(a.Name, r.secondAcct) && a.TaxBucket == "none" {
			sawNone = true
		}
		if a.TaxBucket == "untagged" {
			sawUntagged = true
		}
		if r.closedAcct != "" && strings.EqualFold(a.Name, r.closedAcct) {
			sawClosed = true
		}
	}
	nwMilli, _ := parseMilli(snap.NetWorth)
	r.check(acctSum == nwMilli, "get_financial_snapshot net_worth equals exact sum of open account balances")
	r.check(sawTaxable && sawNone && sawUntagged && !sawClosed,
		"get_financial_snapshot reflects tag-account tags (taxable, none), keeps untagged as 'untagged', and excludes closed accounts")

	bucketsMatch := true
	for _, b := range snap.Buckets {
		bm, _ := parseMilli(b.Balance)
		if bucketSums[b.Bucket] != bm || bucketCounts[b.Bucket] != b.Accounts {
			bucketsMatch = false
		}
	}
	r.check(bucketsMatch && len(snap.Buckets) >= 3, "get_financial_snapshot tax_buckets totals and counts match open accounts")
	r.check(strings.Contains(snap.CashFlow.CSPGrouping, "PROPOSED"), "get_financial_snapshot exposes CSP grouping only as PROPOSED")
	r.check(snap.CashFlow.Window.MonthsRequested == 12 && snap.Goals.Count == len(goals.Goals),
		"get_financial_snapshot reports window coverage and goal count matching get_goals")

	_, _, isErr, _, err = callToolJSON[snapOut](ctx, session, "get_financial_snapshot", map[string]any{"months": 61})
	r.check(err == nil && isErr, "get_financial_snapshot rejects months > 60")

	// 8. Seed a synthetic split transaction, a deleted subtransaction, a deleted transaction,
	// and a refund in the local temp DB (year 2019, isolated from live dates) to verify M2-3
	// split expansion, deleted row exclusion, refund netting, and inclusive date boundaries over MCP.
	r.testSplitsDeletedRefundsAndBoundaries(ctx, session)
}

func (r *runner) testSplitsDeletedRefundsAndBoundaries(ctx context.Context, session *sdkmcp.ClientSession) {
	db, err := store.Open(ctx, r.dbPath)
	if err != nil {
		r.bad("open local db for split fixture: %v", err)
		return
	}
	defer db.Close()

	var acctID, catA, catB, catNameA, catNameB string
	err = db.Reader().QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE plan_id = ? AND deleted = 0 AND on_budget = 1 LIMIT 1`, r.budgetID).Scan(&acctID)
	if err != nil {
		r.bad("find on-budget account: %v", err)
		return
	}
	rows, err := db.Reader().QueryContext(ctx,
		`SELECT id, name FROM categories WHERE plan_id = ? AND deleted = 0 AND COALESCE(internal, 0) = 0 LIMIT 2`, r.budgetID)
	if err != nil {
		r.bad("find categories: %v", err)
		return
	}
	if rows.Next() {
		_ = rows.Scan(&catA, &catNameA)
	}
	if rows.Next() {
		_ = rows.Scan(&catB, &catNameB)
	}
	rows.Close()
	if catA == "" || catB == "" {
		r.bad("need 2 categories for split test")
		return
	}

	// Insert into isolated window 2019-05-01..2019-05-31 ( plus boundary neighbors 2019-04-30 and 2019-06-01 ):
	// - tx-before (2019-04-30): -10.00 in catA (outside May window)
	// - tx-start  (2019-05-01): -20.00 in catA (inclusive start boundary)
	// - tx-split  (2019-05-15): parent -100.00 with sentinel category "split-sentinel-id",
	//     sub1: -60.00 in catA (live)
	//     sub2: -40.00 in catB (live)
	//     sub3: -99.00 in catA (deleted=1, must be ignored)
	// - tx-refund (2019-05-20): +15.00 in catA (refund nets against catA spending)
	// - tx-del    (2019-05-25): -50.00 in catA (deleted=1, must be ignored)
	// - tx-end    (2019-05-31): -5.00 in catB (inclusive end boundary)
	// - tx-after  (2019-06-01): -30.00 in catB (outside May window)
	// Expected in 2019-05-01..2019-05-31:
	//   catA net spending: 20.00 + 60.00 - 15.00 = 65.00 (3 lines)
	//   catB net spending: 40.00 + 5.00 = 45.00 (2 lines)
	//   total lines in May = 5, match_total = -110.00
	w := db.Writer()
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO transactions (plan_id, id, date, amount, account_id, category_id, approved, cleared, deleted, first_seen_at) VALUES (?, 'tx-before', '2019-04-30', -10000, ?, ?, 1, 'cleared', 0, '2026-11-08T00:00:00.000000000Z')`, []any{r.budgetID, acctID, catA}},
		{`INSERT INTO transactions (plan_id, id, date, amount, account_id, category_id, approved, cleared, deleted, first_seen_at) VALUES (?, 'tx-start',  '2019-05-01', -20000, ?, ?, 1, 'cleared', 0, '2026-11-08T00:00:00.000000000Z')`, []any{r.budgetID, acctID, catA}},
		{`INSERT INTO transactions (plan_id, id, date, amount, account_id, category_id, approved, cleared, deleted, first_seen_at) VALUES (?, 'tx-split',  '2019-05-15', -100000, ?, 'split-sentinel-id', 1, 'cleared', 0, '2026-11-08T00:00:00.000000000Z')`, []any{r.budgetID, acctID}},
		{`INSERT INTO subtransactions (plan_id, id, transaction_id, amount, category_id, deleted) VALUES (?, 'sub-1', 'tx-split', -60000, ?, 0)`, []any{r.budgetID, catA}},
		{`INSERT INTO subtransactions (plan_id, id, transaction_id, amount, category_id, deleted) VALUES (?, 'sub-2', 'tx-split', -40000, ?, 0)`, []any{r.budgetID, catB}},
		{`INSERT INTO subtransactions (plan_id, id, transaction_id, amount, category_id, deleted) VALUES (?, 'sub-3-del', 'tx-split', -99000, ?, 1)`, []any{r.budgetID, catA}},
		{`INSERT INTO transactions (plan_id, id, date, amount, account_id, category_id, approved, cleared, deleted, first_seen_at) VALUES (?, 'tx-refund', '2019-05-20', 15000, ?, ?, 1, 'cleared', 0, '2026-11-08T00:00:00.000000000Z')`, []any{r.budgetID, acctID, catA}},
		{`INSERT INTO transactions (plan_id, id, date, amount, account_id, category_id, approved, cleared, deleted, first_seen_at) VALUES (?, 'tx-del',    '2019-05-25', -50000, ?, ?, 1, 'cleared', 1, '2026-11-08T00:00:00.000000000Z')`, []any{r.budgetID, acctID, catA}},
		{`INSERT INTO transactions (plan_id, id, date, amount, account_id, category_id, approved, cleared, deleted, first_seen_at) VALUES (?, 'tx-end',    '2019-05-31', -5000, ?, ?, 1, 'cleared', 0, '2026-11-08T00:00:00.000000000Z')`, []any{r.budgetID, acctID, catB}},
		{`INSERT INTO transactions (plan_id, id, date, amount, account_id, category_id, approved, cleared, deleted, first_seen_at) VALUES (?, 'tx-after',  '2019-06-01', -30000, ?, ?, 1, 'cleared', 0, '2026-11-08T00:00:00.000000000Z')`, []any{r.budgetID, acctID, catB}},
	}
	for _, s := range stmts {
		if _, err := w.ExecContext(ctx, s.q, s.args...); err != nil {
			r.bad("seed split fixture: %v", err)
			return
		}
	}

	type qOut struct {
		Lines []struct {
			TransactionID    string `json:"transaction_id"`
			SubtransactionID string `json:"subtransaction_id"`
			Date             string `json:"date"`
			Amount           string `json:"amount"`
			Category         string `json:"category"`
		} `json:"lines"`
		MatchCount int    `json:"match_count"`
		MatchTotal string `json:"match_total"`
	}
	qMay, _, isErr, _, err := callToolJSON[qOut](ctx, session, "query_transactions", map[string]any{
		"start": "2019-05-01",
		"end":   "2019-05-31",
	})
	r.check(err == nil && !isErr && qMay.MatchCount == 5 && qMay.MatchTotal == "-110.00",
		"query_transactions over seeded window expands splits, excludes deleted tx/subtx, and respects inclusive bounds (count=%d total=%s)",
		qMay.MatchCount, qMay.MatchTotal)

	type cOut struct {
		Categories []struct {
			CategoryID    string `json:"category_id"`
			PeriodSpent   string `json:"period_spent"`
			BaselineSpent string `json:"baseline_spent"`
			Difference    string `json:"difference"`
		} `json:"categories"`
		PeriodTotal   string `json:"period_total"`
		BaselineTotal string `json:"baseline_total"`
		Difference    string `json:"difference"`
	}
	cmpMay, _, isErr, _, err := callToolJSON[cOut](ctx, session, "compare_spending", map[string]any{
		"period":   map[string]any{"month": "2019-05"},
		"baseline": map[string]any{"month": "2019-04"},
	})
	r.check(err == nil && !isErr && cmpMay.PeriodTotal == "110.00" && cmpMay.BaselineTotal == "10.00" && cmpMay.Difference == "100.00",
		"compare_spending nets refunds, counts splits once under child categories, and excludes deleted rows (period=%s baseline=%s diff=%s)",
		cmpMay.PeriodTotal, cmpMay.BaselineTotal, cmpMay.Difference)
}
