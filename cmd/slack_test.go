package cmd

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/alerts"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/slack"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

const (
	testAppToken = "xapp-1-app-SECRET"
	testBotToken = "xoxb-bot-SECRET"
	testBrendan  = "U0BRENDAN1"
	testHilarie  = "U0HILARIE2"
)

func TestSecrets_CoverSlack(t *testing.T) {
	cfg, err := config.LoadFrom(func(k string) string {
		return map[string]string{
			"YNAB_ACCESS_TOKEN": "tok", "YNAB_BUDGET_ID": "plan",
			"SLACK_APP_TOKEN": testAppToken, "SLACK_BOT_TOKEN": testBotToken,
			"SLACK_USERS": "brendan:" + testBrendan + ",hilarie:" + testHilarie,
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	got := scrub("err "+testAppToken+" from "+testBrendan+" and "+testHilarie+" via "+testBotToken, secrets(cfg)...)
	for _, s := range []string{testAppToken, testBotToken, testBrendan, testHilarie} {
		if strings.Contains(got, s) {
			t.Errorf("scrub leaked %q: %q", s, got)
		}
	}
}

// fakeSocket stands in for Slack. Its self-check fails the first failChecks
// times (forever when negative).
type fakeSocket struct {
	mu         sync.Mutex
	failChecks int
	checks     int
	runs       int
	posts      []string
	apply      slack.Applier // the Applier serve gave Slack's questions
	questions  questionAsker
}

func (f *fakeSocket) Questions() questionAsker {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.questions
}

func (f *fakeSocket) SelfCheck(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	if f.failChecks < 0 || f.checks <= f.failChecks {
		return errors.New("slack: self-check: missing_scope for " + testAppToken)
	}
	return nil
}

func (f *fakeSocket) Run(ctx context.Context) error {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (f *fakeSocket) PostAlert(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, text)
	return nil
}

func (f *fakeSocket) state() (checks, runs int, posts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.runs, append([]string(nil), f.posts...)
}

// prepareSlackServe is prepareServe with Socket Mode on over sock, and the
// status message pointed at a local Slack that refuses everything.
func prepareSlackServe(t *testing.T, sock *fakeSocket) *serveRun {
	t.Helper()
	r := prepareServe(t, false)
	t.Setenv("SLACK_APP_TOKEN", testAppToken)
	t.Setenv("SLACK_BOT_TOKEN", testBotToken)
	t.Setenv("SLACK_USERS", "brendan:"+testBrendan+",hilarie:"+testHilarie)

	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	oldNew, oldURL, oldMin := newSocketClient, slackBaseURL, socketRetryMin
	newSocketClient = func(_ *config.Config, _ *store.DB, apply slack.Applier, _ *slog.Logger, _ func(string) string) (socketClient, error) {
		sock.mu.Lock()
		sock.apply = apply
		sock.mu.Unlock()
		return sock, nil
	}
	slackBaseURL, socketRetryMin = refuse.URL, 10*time.Millisecond
	t.Cleanup(func() {
		r.stop()
		refuse.Close()
		newSocketClient, slackBaseURL, socketRetryMin = oldNew, oldURL, oldMin
	})
	return r
}

func TestServe_SelfCheckFailureAlertsAndSyncContinues(t *testing.T) {
	sock := &fakeSocket{failChecks: -1}
	r := prepareSlackServe(t, sock)
	r.start()
	r.waitHealthy()
	r.eventually("the self-check alert and a retry", func() bool {
		checks, _, posts := sock.state()
		return checks >= 3 && len(posts) > 0
	})
	if r.syncs.Load() < 1 {
		t.Error("no sync ran")
	}
	if code := r.stop(); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, r.logs.String())
	}

	_, runs, posts := sock.state()
	if len(posts) != 1 || posts[0] != selfCheckText {
		t.Errorf("posts = %q, want the self-check alert once (retries are duplicates)", posts)
	}
	if runs != 0 {
		t.Errorf("socket ran %d times without a resolved channel", runs)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, `"msg":"slack self-check failed"`) {
		t.Errorf("no self-check failure log:\n%s", logs)
	}
	for _, s := range []string{testAppToken, testBotToken, testBrendan, testHilarie} {
		if strings.Contains(logs, s) {
			t.Errorf("logs leak %q", s)
		}
	}

	db := openDB(t, r.dbPath)
	if ok, err := db.HasAlert(context.Background(), "plan-1", alerts.KindSocketSelfCheck, selfCheckKey); err != nil || !ok {
		t.Errorf("self-check alert stored = %v, %v; want true", ok, err)
	}
	for id, label := range map[string]string{testBrendan: store.UserBrendan, testHilarie: store.UserWife} {
		if u, ok, err := db.UserBySlackID(context.Background(), id); err != nil || !ok || u.Label != label {
			t.Errorf("user %s = %+v %v %v", label, u, ok, err)
		}
	}
}

// A self-check that passes on retry clears the alert and starts the socket.
func TestServe_SelfCheckRecovers(t *testing.T) {
	sock := &fakeSocket{failChecks: 1}
	r := prepareSlackServe(t, sock)
	r.start()
	r.waitHealthy()
	r.eventually("the socket to run", func() bool { _, runs, _ := sock.state(); return runs == 1 })
	if code := r.stop(); code != 0 {
		t.Fatalf("exit = %d:\n%s", code, r.logs.String())
	}
	if _, _, posts := sock.state(); len(posts) != 1 {
		t.Errorf("posts = %q, want the one alert", posts)
	}
	if ok, err := openDB(t, r.dbPath).HasAlert(context.Background(), "plan-1", alerts.KindSocketSelfCheck, selfCheckKey); err != nil || ok {
		t.Errorf("self-check alert still stored = %v, %v; want cleared", ok, err)
	}
}

func openDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// hungPlan's first request blocks, ignoring its ctx, until release closes.
type hungPlan struct {
	fakePlan
	entered chan struct{}
	release chan struct{}
	once    *sync.Once
}

func (h hungPlan) Export(ctx context.Context) (*ynab.PlanDetail, ynab.ServerKnowledge, error) {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return h.fakePlan.Export(ctx)
}

func TestServe_HungJobDoesNotHoldShutdown(t *testing.T) {
	r := prepareServe(t, false)
	h := hungPlan{fakePlan: fakePlan{&r.syncs}, entered: make(chan struct{}), release: make(chan struct{}), once: &sync.Once{}}
	oldClient, oldTimeout := newPlanClient, shutdownTimeout
	newPlanClient = func(string, string, *log.Logger, *ynab.Governor) finsync.PlanAPI { return h }
	shutdownTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		close(h.release)
		newPlanClient, shutdownTimeout = oldClient, oldTimeout
	})

	r.start()
	select {
	case <-h.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("sync never started:\n%s", r.logs.String())
	}
	started := time.Now()
	code := r.stop()
	if d := time.Since(started); d > shutdownTimeout+2*time.Second {
		t.Errorf("serve took %v to return", d)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1 (unclean)", code)
	}
	if logs := r.logs.String(); !strings.Contains(logs, `"msg":"shutdown deadline passed; exiting anyway"`) ||
		!strings.Contains(logs, `"waiting_on":["scheduler"]}`) {
		t.Errorf("no deadline log naming the scheduler:\n%s", logs)
	}
}

// failingAlerter fails every call and counts them.
type failingAlerter struct{ freshness, anomalies atomic.Int32 }

func (f *failingAlerter) Freshness(context.Context, bool, []finsync.AccountFreshness) error {
	f.freshness.Add(1)
	return errors.New("alerts: store: disk I/O error")
}
func (f *failingAlerter) Anomalies(context.Context) error {
	f.anomalies.Add(1)
	return errors.New("alerts: store: disk I/O error")
}
func (f *failingAlerter) Send(context.Context, alerts.Alert) (alerts.Outcome, error) {
	return 0, errors.New("alerts: store: disk I/O error")
}
func (f *failingAlerter) Clear(context.Context, string, string) error {
	return errors.New("alerts: store: disk I/O error")
}

func TestSyncJob_AlertFailureLeavesSyncAndHealth(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, filepath.Join(t.TempDir(), "finbot.db"))
	var syncs atomic.Int32
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	clients := func(string) finsync.PlanAPI { return fakePlan{&syncs} }
	fa := &failingAlerter{}
	a := &app{
		cfg:     &config.Config{YNABAccessToken: "tok", YNABBudgetID: "plan-1", SyncInterval: time.Minute},
		log:     logger,
		db:      db,
		syncer:  finsync.New(db, clients, slog.NewLogLogger(logger.Handler(), slog.LevelInfo)),
		alerter: fa,
	}
	sched, health := a.newScheduler()
	if err := sched.Sync(ctx); err != nil {
		t.Fatalf("sync job failed: %v", err)
	}
	sched.runSync(ctx)
	if fa.freshness.Load() != 2 || fa.anomalies.Load() != 2 {
		t.Errorf("alerter calls: freshness %d, anomalies %d; want 2 each", fa.freshness.Load(), fa.anomalies.Load())
	}
	rec := httptest.NewRecorder()
	health.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "disk I/O") {
		t.Errorf("health = %d %s, want 200 with no alert error", rec.Code, rec.Body.String())
	}
	for _, msg := range []string{"freshness alerts failed", "anomaly alerts failed"} {
		if !strings.Contains(logs.String(), `"msg":"`+msg+`"`) {
			t.Errorf("no %q log:\n%s", msg, logs.String())
		}
	}
}
