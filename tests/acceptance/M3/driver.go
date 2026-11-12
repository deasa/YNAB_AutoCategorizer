// Command M3 drives finbot's M3 safety checks against the YNAB sandbox plan.
//
// It wires the real store, syncer, writer, classify pipeline, Slack client and
// Questions exactly as serve does, with three substitutions: the Slack Socket
// Mode connection and Web API are in-process fakes (a second real socket would
// steal events from hoid), the model is a fixed fake (so confidences are known),
// and every YNAB request goes through a guard that refuses anything but the
// sandbox plan. It never prints ids, tokens, emails or the plan id.
//
// Phases (one per invocation, sharing -work):
//
//	cleanup     delete this suite's fixtures left in the sandbox
//	writer      scope 2: apply + before/after, refuse a human-edited row, a human edit survives syncs
//	slack       scope 3: unlisted user, double click, restart keeps routing
//	mismatch    scope 4: configured auto with a mismatched calibration hash acts as dry_run, incl. a categorize answer
//	transition  scope 5: dry_run_check closed on entering auto, never written if clicked, pre-AutoSince never written
//	crash1      scope 6: PATCH reaches YNAB, then the process exits before `done` (exit 7 is success)
//	crash2      scope 6: restart with the real `finbot serve` (sync + Reconcile), then: intent done, row not locked
//	autobin     scope 4 extra: real `finbot serve`, CLASSIFY_MODE=auto, shipped calibration, real Jev: runs as dry_run
//	mcp         scope 7: get_category_decisions through the authenticated MCP of a real `finbot serve`
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	gosync "sync"
	"time"

	"github.com/joho/godotenv"
	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	ynabapi "pkg.venceslau.dev/ynab"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/slack"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

const (
	fixtureAccount = "finbot M3 acceptance"
	payeePrefix    = "M3acc "
	fakeModelID    = "m3-fake-model-1"
	chanID         = "C0M3ACCEPT"
	brendanSlack   = "U0M3BRENDAN"
	wifeSlack      = "U0M3WIFE"
	strangerSlack  = "U0M3STRANGER"
	// The suite shares YNAB_ACCESS_TOKEN (200 requests per rolling hour) with hoid.
	suiteCap       = 110 // whole suite, every process; a full run uses about 95
	minRemaining   = 160 // preflight refuses to start below this
	floorRemaining = 40  // the guard stops every request below this
	serveAllowance = 5   // charged per `finbot serve` run (one delta sync; not proxied)
	tokenLimit     = 200 // YNAB's per-token hourly limit; YNAB sends no X-Rate-Limit header
	otherMargin    = 10  // unledgered use of the token (other tools) assumed per hour
)

var (
	hourLedgerPath string
	hoidUsed       int
	uuidRE         = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	fails          int
	scrubs         []string
)

func scrub(v any) string {
	s := fmt.Sprint(v)
	for _, x := range scrubs {
		if x != "" {
			s = strings.ReplaceAll(s, x, "<redacted>")
		}
	}
	return uuidRE.ReplaceAllString(s, "<id>")
}

func check(ok bool, format string, args ...any) {
	msg := scrub(fmt.Sprintf(format, args...))
	if ok {
		fmt.Println("PASS", msg)
		return
	}
	fails++
	fmt.Println("FAIL", msg)
}

func fatal(what string, err error) {
	fmt.Println("FAIL", what+":", scrub(err))
	os.Exit(1)
}

// ---- YNAB sandbox guard -------------------------------------------------

// guard admits only sandbox requests, and protects the YNAB rate budget this
// suite shares with hoid (same token, 200 requests per rolling hour):
//   - a suite-wide cap: every driver process adds to one ledger file in -work,
//     and each `finbot serve` run is charged serveAllowance up front;
//   - a floor: once YNAB's X-Rate-Limit header (or a 429) shows fewer than
//     floorRemaining left for the token, every later request is refused.
type guard struct {
	base          http.RoundTripper
	sandbox, live string
	ledger        string // this suite run's total, in -work
	hourLedger    string // "<unix> <n>" per charge, shared by every M3 run under the .wt dir
	mu            gosync.Mutex
	n             int // this process
	patches       int
	used, limit   int // from the last X-Rate-Limit header; limit 0 = unknown
	tripped       string
}

func (g *guard) total() int {
	b, _ := os.ReadFile(g.ledger)
	var t int
	fmt.Sscan(strings.TrimSpace(string(b)), &t)
	return t
}

// charge adds n requests to the suite ledger; it fails past suiteCap.
func (g *guard) charge(n int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.chargeLocked(n)
}

func (g *guard) chargeLocked(n int) error {
	if g.tripped != "" {
		return errors.New("sandbox guard: stopped to protect the shared rate budget: " + g.tripped)
	}
	t := g.total() + n
	if t > suiteCap {
		return fmt.Errorf("sandbox guard: suite request cap %d reached", suiteCap)
	}
	if err := os.WriteFile(g.ledger, []byte(fmt.Sprint(t)), 0o600); err != nil {
		return err
	}
	return appendLine(g.hourLedger, fmt.Sprintf("%d %d", time.Now().Unix(), n))
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

// lastHour sums the hour ledger's charges made in the last 60 minutes.
func lastHour(path string) int {
	b, _ := os.ReadFile(path)
	cut, sum := time.Now().Add(-time.Hour).Unix(), 0
	for _, l := range strings.Split(string(b), "\n") {
		var ts int64
		var n int
		if _, err := fmt.Sscan(l, &ts, &n); err == nil && ts > cut {
			sum += n
		}
	}
	return sum
}

func (g *guard) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.String(), g.live) {
		return nil, errors.New("sandbox guard: request names the live plan")
	}
	prefix := "/v1/plans/" + g.sandbox
	if p := r.URL.Path; p != prefix && !strings.HasPrefix(p, prefix+"/") {
		return nil, errors.New("sandbox guard: request is not for the sandbox plan")
	}
	g.mu.Lock()
	if err := g.chargeLocked(1); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	g.n++
	if r.Method == http.MethodPatch {
		g.patches++
	}
	g.mu.Unlock()
	resp, err := g.base.RoundTrip(r)
	if resp != nil {
		g.mu.Lock()
		var u, l int
		if _, e := fmt.Sscanf(resp.Header.Get("X-Rate-Limit"), "%d/%d", &u, &l); e == nil && l > 0 {
			g.used, g.limit = u, l
			if l-u < floorRemaining {
				g.tripped = fmt.Sprintf("%d of %d left", l-u, l)
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			g.tripped = "HTTP 429"
			// Record the token as exhausted for the next hour.
			_ = appendLine(g.hourLedger, fmt.Sprintf("%d %d 429", time.Now().Unix(), tokenLimit))
		}
		g.mu.Unlock()
	}
	return resp, err
}

func (g *guard) patchCount() int { g.mu.Lock(); defer g.mu.Unlock(); return g.patches }

// ---- fake Slack ---------------------------------------------------------

type fakeAPI struct {
	mu    gosync.Mutex
	seq   int
	texts []string // every posted/updated text + blocks, for the secret grep
	dump  io.Writer
}

func (f *fakeAPI) GetConversationsForUserContext(context.Context, *slackapi.GetConversationsForUserParameters) ([]slackapi.Channel, string, error) {
	var ch slackapi.Channel
	ch.ID, ch.Name = chanID, "finbot-finances"
	return []slackapi.Channel{ch}, "", nil
}

func (f *fakeAPI) record(kind string, opts []slackapi.MsgOption) {
	_, vals, err := slackapi.UnsafeApplyMsgOptions("", chanID, "", opts...)
	if err != nil {
		return
	}
	line := kind + " text=" + vals.Get("text") + " blocks=" + vals.Get("blocks")
	f.texts = append(f.texts, line)
	if f.dump != nil {
		fmt.Fprintln(f.dump, line)
	}
}

func (f *fakeAPI) PostMessageContext(_ context.Context, _ string, opts ...slackapi.MsgOption) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.record("post", opts)
	return chanID, fmt.Sprintf("%d.%06d", time.Now().Unix(), f.seq), nil
}

func (f *fakeAPI) UpdateMessageContext(_ context.Context, _ string, ts string, opts ...slackapi.MsgOption) (string, string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("update", opts)
	return chanID, ts, "", nil
}

func (f *fakeAPI) lastText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.texts) == 0 {
		return ""
	}
	return f.texts[len(f.texts)-1]
}

type fakeSocket struct{ ch chan socketmode.Event }

func (s *fakeSocket) Events() <-chan socketmode.Event { return s.ch }
func (s *fakeSocket) Ack(socketmode.Request) error    { return nil }
func (s *fakeSocket) Run(ctx context.Context) error   { <-ctx.Done(); return nil }

// lateMsg breaks the Questions <-> Client construction cycle, like serve does.
type lateMsg struct{ c *slack.Client }

func (l *lateMsg) ChannelID() string { return l.c.ChannelID() }
func (l *lateMsg) PostBlocks(ctx context.Context, th, text string, b []slackapi.Block) (string, error) {
	return l.c.PostBlocks(ctx, th, text, b)
}
func (l *lateMsg) UpdateBlocks(ctx context.Context, ts, text string, b []slackapi.Block) error {
	return l.c.UpdateBlocks(ctx, ts, text, b)
}

var actionSeq int

func click(user string, q store.Question, option int) socketmode.Event {
	actionSeq++
	return clickAt(user, q, option, fmt.Sprintf("1800000500.%06d", actionSeq))
}

func clickAt(user string, q store.Question, option int, actionTS string) socketmode.Event {
	ts := ""
	if q.MessageTS != nil {
		ts = *q.MessageTS
	}
	return socketmode.Event{
		Type:    socketmode.EventTypeInteractive,
		Request: &socketmode.Request{EnvelopeID: "env-" + actionTS},
		Data: slackapi.InteractionCallback{
			Type:      slackapi.InteractionTypeBlockActions,
			User:      slackapi.User{ID: user},
			Container: slackapi.Container{ChannelID: chanID, MessageTs: ts},
			ActionCallback: slackapi.ActionCallbacks{BlockActions: []*slackapi.BlockAction{{
				ActionID: fmt.Sprintf("q_pick_%d", option), Value: fmt.Sprintf("%d:%d", q.ID, option), ActionTs: actionTS,
			}}},
		},
	}
}

// ---- fake model ---------------------------------------------------------

// fakeModel answers by payee: "M3acc hi ..." with 0.97 on the first candidate,
// anything else with 0.40 (below τ).
type fakeModel struct{}

func (fakeModel) Classify(_ context.Context, txns []classify.Txn, cands []classify.Candidate) ([]classify.Prediction, error) {
	out := make([]classify.Prediction, len(txns))
	for i, t := range txns {
		p := 0.40
		if strings.HasPrefix(t.Payee, payeePrefix+"hi ") {
			p = 0.97
		}
		probs := map[string]float64{}
		var top []string
		for j, c := range cands {
			v := (1 - p) / float64(len(cands)-1)
			if j == 0 {
				v = p
			}
			probs[c.Name] = v
			if j < 3 {
				top = append(top, c.Name)
			}
		}
		out[i] = classify.Prediction{Probs: probs, Top: top, Model: fakeModelID}
	}
	return out, nil
}

// ---- harness ------------------------------------------------------------

type harness struct {
	ctx     context.Context
	work    string
	sb      string
	g       *guard
	client  *ynab.Client
	fx      *ynabapi.Plan
	account string
	cats    []string // usable category ids: [0] is the fake model's top pick
	catName map[string]string

	db      *store.DB
	syncer  *finsync.Syncer
	gate    *writer.Gate
	w       *writer.Writer
	api     *fakeAPI
	sock    *fakeSocket
	slackC  *slack.Client
	q       *slack.Questions
	stop    context.CancelFunc
	runDone chan struct{}
	logf    *os.File
	apiWrap writer.API
}

func newHarness(ctx context.Context, work string) *harness {
	_ = godotenv.Load(".env")
	sb, live, tok := os.Getenv("YNAB_SANDBOX_PLAN_ID"), os.Getenv("YNAB_BUDGET_ID"), os.Getenv("YNAB_ACCESS_TOKEN")
	switch {
	case sb == "" || live == "" || tok == "":
		fatal("env", errors.New("YNAB_SANDBOX_PLAN_ID, YNAB_BUDGET_ID and YNAB_ACCESS_TOKEN must be set"))
	case sb == live:
		fatal("env", errors.New("sandbox plan equals the live plan: refusing"))
	}
	scrubs = []string{tok, sb, live, os.Getenv("JEV_API_KEY"), os.Getenv("SLACK_BOT_TOKEN"), os.Getenv("SLACK_APP_TOKEN")}
	g := &guard{base: http.DefaultTransport, sandbox: sb, live: live, ledger: filepath.Join(work, "requests.count"), hourLedger: hourLedgerPath}
	logf, err := os.OpenFile(filepath.Join(work, "driver-app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fatal("open log", err)
	}
	h := &harness{
		ctx: ctx, work: work, sb: sb, g: g, logf: logf,
		client: ynab.NewClient(tok, sb, log.New(logf, "ynab ", 0), ynab.WithBaseTransport(g)),
		fx:     ynabapi.New(tok, ynabapi.WithHTTPClient(&http.Client{Transport: g})).Plan(ynabapi.PlanID(sb)),
	}
	h.apiWrap = h.client
	return h
}

func (h *harness) slog() *slog.Logger {
	return slog.New(slog.NewJSONHandler(h.logf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (h *harness) fixtures() {
	accts, _, err := h.fx.Accounts.List(h.ctx)
	if err != nil {
		fatal("list accounts", err)
	}
	for _, a := range accts {
		if a.Name == fixtureAccount && !a.Deleted && !a.Closed {
			h.account = a.ID
		}
	}
	if h.account == "" {
		a, err := h.fx.Accounts.Create(h.ctx, ynabapi.AccountSpec{Name: fixtureAccount, Type: ynabapi.AccountSpecTypeChecking})
		if err != nil {
			fatal("create account", err)
		}
		h.account = a.ID
	}
}

// openDB opens the store and the components that hang off it, as serve does.
func (h *harness) openDB(autoSince string) {
	db, err := store.Open(h.ctx, filepath.Join(h.work, "finbot.db"))
	if err != nil {
		fatal("open store", err)
	}
	h.db = db
	h.syncer = finsync.New(db, func(planID string) finsync.PlanAPI {
		if planID != h.sb {
			panic("M3 driver: sync asked for a plan other than the sandbox")
		}
		return h.client
	}, log.New(h.logf, "sync ", 0)).DetectLocks(h.sb)
	h.gate = writer.NewGate()
	h.gate.Set(writer.ModeDryRun)
	h.w = writer.New(writer.Config{PlanID: h.sb, AutoSince: autoSince, Gate: h.gate, API: h.apiWrap, DB: db, Resync: h.sync})
	if err := db.UpsertUsers(h.ctx, []store.User{
		{SlackUserID: brendanSlack, Label: store.UserBrendan}, {SlackUserID: wifeSlack, Label: store.UserWife},
	}, time.Now()); err != nil {
		fatal("upsert users", err)
	}
}

func (h *harness) sync(ctx context.Context) error { _, err := h.syncer.SyncPlan(ctx, h.sb); return err }

func (h *harness) mustSync(what string) {
	if err := h.sync(h.ctx); err != nil {
		fatal("sync "+what, err)
	}
}

// startSlack builds the Slack client and Questions and runs the event loop.
func (h *harness) startSlack() {
	if h.api == nil {
		dump, err := os.OpenFile(filepath.Join(h.work, "slack-text.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fatal("open slack dump", err)
		}
		h.api = &fakeAPI{dump: dump}
	}
	h.sock = &fakeSocket{ch: make(chan socketmode.Event, 16)}
	lm := &lateMsg{}
	h.q = slack.NewQuestions(h.db, h.w, lm, h.slog())
	h.slackC = slack.New(slack.Config{Channel: chanID}, h.api, h.sock, h.db, h.q.Handlers(), h.slog())
	lm.c = h.slackC
	if err := h.slackC.SelfCheck(h.ctx); err != nil {
		fatal("slack self-check", err)
	}
	ctx, cancel := context.WithCancel(h.ctx)
	h.stop, h.runDone = cancel, make(chan struct{})
	go func() { _ = h.slackC.Run(ctx); close(h.runDone) }()
}

// restart stops the Slack loop and closes the DB, then opens everything fresh.
func (h *harness) restart(autoSince string, mode writer.Mode) {
	h.stop()
	<-h.runDone
	if err := h.db.Close(); err != nil {
		fatal("close db", err)
	}
	h.openDB(autoSince)
	h.gate.Set(mode)
	h.startSlack()
}

func (h *harness) send(evts ...socketmode.Event) {
	for _, e := range evts {
		h.sock.ch <- e
	}
}

// settle waits until cond holds (or 15 s), then 1.5 s more for stragglers.
func settle(cond func() bool) bool {
	deadline := time.Now().Add(15 * time.Second)
	ok := false
	for time.Now().Before(deadline) {
		if cond() {
			ok = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	return ok
}

func (h *harness) pickCategories() {
	cats, err := h.db.CategoriesWithGroups(h.ctx, h.sb)
	if err != nil {
		fatal("categories", err)
	}
	pol, err := classify.DefaultPolicy()
	if err != nil {
		fatal("policy", err)
	}
	set := classify.Candidates(cats, pol)
	byName := map[string]string{}
	for _, c := range cats {
		if !c.Deleted {
			byName[c.Name] = c.ID
		}
	}
	h.catName = map[string]string{}
	for _, c := range set.Candidates {
		if id, ok := byName[c.Name]; ok {
			h.cats = append(h.cats, id)
			h.catName[id] = c.Name
		}
	}
	if len(h.cats) < 3 {
		fatal("candidates", fmt.Errorf("sandbox has %d candidate categories; want at least 3", len(h.cats)))
	}
}

func (h *harness) createTxn(tag string, daysAgo int) string {
	t, _, err := h.fx.Transactions.Create(h.ctx, ynabapi.TransactionSpec{
		AccountID: h.account,
		Date:      ynabapi.Today().AddDays(-daysAgo),
		Amount:    -4560,
		PayeeName: ynabapi.Set(payeePrefix + tag + " " + time.Now().UTC().Format("150405.000000")),
		Memo:      ynabapi.Set("finbot M3 acceptance fixture; safe to delete"),
		Approved:  ynabapi.Set(false),
	})
	if err != nil {
		fatal("create txn "+tag, err)
	}
	return t.ID
}

func (h *harness) humanEdit(txnID, catID string) {
	if _, _, err := h.fx.Transactions.Update(h.ctx, txnID, ynabapi.TransactionUpdate{CategoryID: ynabapi.Set(catID)}); err != nil {
		fatal("human edit", err)
	}
}

// ynabCat reads the row back from YNAB itself (not the mirror).
func (h *harness) ynabCat(txnID string) (cat string, approved bool) {
	t, _, err := h.fx.Transactions.Get(h.ctx, txnID)
	if err != nil {
		fatal("read back from YNAB", err)
	}
	if t.CategoryID != nil {
		cat = *t.CategoryID
	}
	// YNAB reports an uncategorized outflow under its internal "Uncategorized" category.
	if cat != "" && h.catName[cat] == "" {
		cat = "(uncategorized/other)"
	}
	return cat, t.Approved
}

func (h *harness) writeRows(txnID string) (n int, statuses []string) {
	rows, err := h.db.Writer().QueryContext(h.ctx, `SELECT status FROM write_log WHERE plan_id = ? AND txn_id = ? ORDER BY id`, h.sb, txnID)
	if err != nil {
		fatal("query write_log", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		statuses = append(statuses, s)
	}
	return len(statuses), statuses
}

func (h *harness) totalWrites() int {
	var n int
	if err := h.db.Writer().QueryRowContext(h.ctx, `SELECT count(*) FROM write_log`).Scan(&n); err != nil {
		fatal("count write_log", err)
	}
	return n
}

func (h *harness) locks(txnID string) []store.HumanLock {
	m, err := h.db.Locks(h.ctx, h.sb, []string{txnID})
	if err != nil {
		fatal("locks", err)
	}
	return m[txnID]
}

func (h *harness) question(txnID, kind string) (store.Question, bool) {
	qs, err := h.db.QuestionsBySubject(h.ctx, h.sb, txnID)
	if err != nil {
		fatal("questions", err)
	}
	var found store.Question
	ok := false
	for _, q := range qs {
		if q.Kind == kind {
			found, ok = q, true // newest wins
		}
	}
	return found, ok
}

func (h *harness) reload(q store.Question) store.Question {
	n, ok, err := h.db.QuestionByID(h.ctx, q.ID)
	if err != nil || !ok {
		fatal("reload question", fmt.Errorf("found=%t err=%v", ok, err))
	}
	return n
}

// calibration is an auto-eligible calibration measured (in effect) on the
// sandbox's own policy and candidates, so it matches.
func (h *harness) calibration(bool) classify.Calibration {
	cats, err := h.db.CategoriesWithGroups(h.ctx, h.sb)
	if err != nil {
		fatal("categories", err)
	}
	pol, _ := classify.DefaultPolicy()
	tau := 0.8
	return classify.Calibration{Model: fakeModelID, PolicyHash: classify.PolicyHash(pol),
		CandidatesHash: classify.CandidatesHash(classify.Candidates(cats, pol).Candidates), Tau: &tau, AutoEligible: true}
}

func (h *harness) runPipeline(mode writer.Mode, autoSince string, backlog int, cal classify.Calibration) classify.Result {
	pol, _ := classify.DefaultPolicy()
	p := classify.New(classify.Config{
		PlanID: h.sb, Mode: mode, Policy: pol, ModelID: fakeModelID, Model: fakeModel{},
		Store: h.db, Applier: h.w, Asker: h.q, SetMode: h.gate.Set, AutoSince: autoSince,
		BacklogDays: backlog, QuestionCap: 20, Log: h.slog(),
	})
	res, err := p.Run(h.ctx, cal)
	if err != nil {
		fatal("pipeline run", err)
	}
	return res
}

func (h *harness) cleanup() {
	h.fixtures()
	txns, _, err := h.fx.Transactions.ListByAccount(h.ctx, h.account, ynabapi.TransactionFilter{})
	if err != nil {
		fatal("list fixtures", err)
	}
	n := 0
	for _, t := range txns {
		if t.Deleted || t.PayeeName == nil || !strings.HasPrefix(*t.PayeeName, payeePrefix) {
			continue
		}
		if _, _, err := h.fx.Transactions.Delete(h.ctx, t.ID); err != nil {
			fatal("delete fixture", err)
		}
		n++
	}
	fmt.Printf("INFO cleanup: deleted %d fixture transactions\n", n)
}

// preflight refuses unless the shared token has at least minRemaining requests
// left this hour by our accounting: YNAB sends no rate header, so the estimate
// is this hour's ledger (every M3 run, plus any 429 recorded as the whole limit)
// + hoid's own requests (from its logs) + a margin for other tools. It then
// makes one cheap GET, which must not be a 429.
func (h *harness) preflight() {
	if hoidUsed < 0 {
		check(false, "preflight: -hoid-used is required")
		return
	}
	suite := lastHour(hourLedgerPath)
	left := tokenLimit - suite - hoidUsed - otherMargin
	check(left >= minRemaining, "preflight: estimated %d of %d left this hour (M3 ledger %d, hoid %d, margin %d; need >= %d)",
		left, tokenLimit, suite, hoidUsed, otherMargin, minRemaining)
	if left < minRemaining {
		return
	}
	_, _, err := h.fx.Accounts.List(h.ctx)
	check(err == nil, "preflight GET (%v)", err)
}

func today() string               { return time.Now().Format("2006-01-02") }
func daysAgo(n int) string        { return time.Now().AddDate(0, 0, -n).Format("2006-01-02") }
func catOK(got, want string) bool { return got != "" && got == want }
func names(ls []store.HumanLock) string {
	var s []string
	for _, l := range ls {
		s = append(s, l.Field+"/"+l.Source)
	}
	return strings.Join(s, ",")
}

func main() {
	phase := flag.String("phase", "", "phase to run")
	work := flag.String("work", "", "work directory (holds the DB between phases)")
	flag.StringVar(&finbotBin, "bin", "", "finbot binary (crash2, autobin, mcp)")
	flag.StringVar(&hourLedgerPath, "ledger", "", "shared hourly YNAB request ledger (required)")
	flag.IntVar(&hoidUsed, "hoid-used", -1, "preflight: hoid's YNAB requests in the last hour (from its logs)")
	flag.Parse()
	if *phase == "" || *work == "" || hourLedgerPath == "" {
		fmt.Fprintln(os.Stderr, "usage: M3 -phase <phase> -work <dir>")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	h := newHarness(ctx, *work)
	switch *phase {
	case "preflight":
		h.preflight()
	case "cleanup":
		h.cleanup()
	case "writer":
		h.phaseWriter()
	case "slack":
		h.phaseSlack()
	case "mismatch":
		h.phaseMismatch()
	case "transition":
		h.phaseTransition()
	case "crash1":
		h.phaseCrash1()
	case "crash2":
		h.phaseCrashRestart()
	case "autobin":
		h.phaseAutoBinary()
	case "mcp":
		h.phaseMCP()
	default:
		fmt.Fprintln(os.Stderr, "unknown phase")
		os.Exit(2)
	}
	fmt.Printf("INFO %s: %d YNAB requests (%d PATCH) from the driver, all through the sandbox guard; suite ledger %d/%d\n",
		*phase, h.g.n, h.g.patchCount(), h.g.total(), suiteCap)
	if fails > 0 {
		fmt.Printf("RESULT %s FAIL (%d)\n", *phase, fails)
		os.Exit(1)
	}
	fmt.Printf("RESULT %s PASS\n", *phase)
}
