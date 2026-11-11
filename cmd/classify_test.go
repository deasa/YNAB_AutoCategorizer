package cmd

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/alerts"
	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/llm"
	"github.com/deasa/YNAB_AutoCategorizer/slack"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

const (
	testJevKey    = "jev-SECRET-key"
	testClaudeTok = "sk-ant-oat-SECRET"
	testPlan      = "plan-1"
)

// countModel is a classify.Model that counts calls and never answers.
type countModel struct{ calls *atomic.Int32 }

func (m countModel) Classify(context.Context, []classify.Txn, []classify.Candidate) ([]classify.Prediction, error) {
	m.calls.Add(1)
	return nil, errors.New("not in tests")
}

// countPatch is a writer.API that counts PATCHes.
type countPatch struct{ calls *atomic.Int32 }

func (p countPatch) PatchTransactions(context.Context, []ynab.TransactionChange) ([]ynab.PatchedTransaction, error) {
	p.calls.Add(1)
	return nil, errors.New("not in tests")
}

// models counts what serve builds and calls.
type models struct{ built, calls, claudes, patches atomic.Int32 }

// fakeModels replaces the model, claude and writer API constructors.
func fakeModels(t *testing.T) *models {
	t.Helper()
	m := &models{}
	oldModel, oldClaude, oldAPI := newClassifyModel, newClaude, newWriterAPI
	newClassifyModel = func(string) (classify.Model, string, error) {
		m.built.Add(1)
		return countModel{&m.calls}, "jev-test", nil
	}
	newClaude = func(llm.Config) (claudeRunner, classify.Model, error) {
		m.claudes.Add(1)
		return &fakeRunner{}, countModel{&m.calls}, nil
	}
	newWriterAPI = func(string, string, *log.Logger, *ynab.Governor) writer.API { return countPatch{&m.patches} }
	t.Cleanup(func() { newClassifyModel, newClaude, newWriterAPI = oldModel, oldClaude, oldAPI })
	return m
}

func classifyApp(t *testing.T, cc config.Classify) (*app, *bytes.Buffer) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var logs bytes.Buffer
	return &app{
		cfg: &config.Config{YNABAccessToken: "tok", YNABBudgetID: testPlan, DBPath: filepath.Join(t.TempDir(), "t.db"),
			SyncInterval: time.Minute, Classify: cc},
		log: slog.New(slog.NewJSONHandler(&logs, nil)),
		db:  db,
	}, &logs
}

var claudeOn = config.Classify{ClaudeToken: testClaudeTok, ClaudeBin: "/opt/claude"}

func TestCategorize_OffBuildsNoPipelineAndCallsNoModel(t *testing.T) {
	m := fakeModels(t)
	cc := claudeOn
	cc.Mode = config.ClassifyOff
	a, _ := classifyApp(t, cc)
	c, err := a.newCategorizer()
	if err != nil {
		t.Fatal(err)
	}
	if c.Run != nil {
		t.Error("mode off built a pipeline")
	}
	a.cat = c
	sched, _ := a.newScheduler()
	if sched.Probe != nil {
		t.Error("mode off probes claude")
	}
	c.After(context.Background(), time.Now())
	if m.built.Load() != 0 || m.claudes.Load() != 0 || m.calls.Load() != 0 || m.patches.Load() != 0 {
		t.Errorf("mode off: built %d models, %d claudes, %d model calls, %d patches",
			m.built.Load(), m.claudes.Load(), m.calls.Load(), m.patches.Load())
	}
}

func TestCategorize_DryRunBuildsPipelineAndProbe(t *testing.T) {
	m := fakeModels(t)
	cc := claudeOn
	cc.Mode, cc.JevAPIKey = config.ClassifyDryRun, testJevKey
	a, _ := classifyApp(t, cc)
	c, err := a.newCategorizer()
	if err != nil {
		t.Fatal(err)
	}
	a.cat = c
	if c.Run == nil || m.built.Load() != 1 || m.claudes.Load() != 1 {
		t.Fatalf("pipeline %v, models %d, claudes %d", c.Run != nil, m.built.Load(), m.claudes.Load())
	}
	if sched, _ := a.newScheduler(); sched.Probe == nil || sched.ProbeEvery != 24*time.Hour {
		t.Error("no daily probe with claude configured")
	}
}

// stepRecorder records the categorizer's steps.
type stepRecorder struct {
	mu        sync.Mutex
	steps     []string
	syncStart time.Time
}

func (r *stepRecorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, s)
}

func (r *stepRecorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.steps...)
}

func recordingCategorizer(a *app, r *stepRecorder, run func(ctx context.Context) (classify.Result, error)) *categorizer {
	return &categorizer{
		Mode: config.ClassifyDryRun,
		Reconcile: func(_ context.Context, start time.Time) error {
			r.mu.Lock()
			r.syncStart = start
			r.mu.Unlock()
			r.add("reconcile")
			return nil
		},
		Run: func(ctx context.Context) (classify.Result, error) {
			r.add("pipeline")
			if run != nil {
				return run(ctx)
			}
			return classify.Result{Mode: writer.ModeDryRun}, nil
		},
		CloseStale: func(context.Context) (int, error) { r.add("close_stale"); return 0, nil },
		Timeout:    time.Second,
		a:          a,
	}
}

func TestSyncJob_CategorizesOnlyAfterSuccessfulSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"failed", errors.New("boom"), nil},
		{"budget skipped", errSkipped, nil},
		{"ok", nil, []string{"reconcile", "pipeline", "close_stale"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := classifyApp(t, config.Classify{})
			r := &stepRecorder{}
			a.cat = recordingCategorizer(a, r, nil)
			var syncAt time.Time
			job := a.syncJob(func(context.Context) (finsync.Result, error) {
				syncAt = time.Now()
				r.add("sync")
				return finsync.Result{Skipped: tc.err == errSkipped}, tc.err
			}, &Pinger{Log: a.log})
			if err := job(context.Background()); err != tc.err {
				t.Fatalf("job err = %v", err)
			}
			want := append([]string{"sync"}, tc.want...)
			if got := r.got(); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("steps %v, want %v", got, want)
			}
			if tc.err == nil && (r.syncStart.IsZero() || r.syncStart.After(syncAt)) {
				t.Errorf("Reconcile got sync start %v; sync ran at %v", r.syncStart, syncAt)
			}
		})
	}
}

func TestSyncJob_CategorizeBoundCancelsAndNextSyncRuns(t *testing.T) {
	a, _ := classifyApp(t, config.Classify{})
	r := &stepRecorder{}
	var cancelled atomic.Int32
	a.cat = recordingCategorizer(a, r, func(ctx context.Context) (classify.Result, error) {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cancelled.Add(1)
		}
		return classify.Result{}, ctx.Err()
	})
	a.cat.Timeout = 40 * time.Millisecond
	var liveCloses atomic.Int32
	a.cat.CloseStale = func(ctx context.Context) (int, error) {
		if ctx.Err() == nil {
			liveCloses.Add(1)
		}
		r.add("close_stale")
		return 0, nil
	}
	var syncs atomic.Int32
	tk := &tickers{}
	stop := start(t, &Scheduler{
		Sync: a.syncJob(func(context.Context) (finsync.Result, error) {
			syncs.Add(1)
			return finsync.Result{}, nil
		}, &Pinger{Log: a.log}),
		SyncEvery: syncEvery, NewTicker: tk.new,
	})
	defer stop()
	waitFor(t, func() bool { return cancelled.Load() >= 1 }) // the sync ticker exists by now
	// A tick that lands while the job runs is drained, so tick until the
	// second sync is seen.
	tickUntil(t, tk.get(syncEvery), func() bool { return syncs.Load() >= 2 })
	waitFor(t, func() bool { return cancelled.Load() >= 2 && liveCloses.Load() >= 2 })
}

// tickUntil ticks f until cond holds: a tick that lands while a job runs is
// dropped by the scheduler's drain.
func tickUntil(t *testing.T, f *fakeTicker, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(wait); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		f.tick()
	}
}

// slackApplier starts Slack on a fake socket and returns the Applier serve
// gave Slack's questions.
func slackApplier(t *testing.T, a *app) slack.Applier {
	t.Helper()
	a.cfg.SlackAppToken, a.cfg.SlackBotToken = testAppToken, testBotToken
	a.cfg.SlackUsers = []config.SlackUser{{Label: "brendan", SlackID: testBrendan}}
	sock := &fakeSocket{}
	old := newSocketClient
	newSocketClient = func(_ *config.Config, _ *store.DB, apply slack.Applier, _ *slog.Logger, _ func(string) string) (socketClient, error) {
		sock.apply = apply
		return sock, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done, err := a.startSlack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-done; newSocketClient = old })
	if sock.apply == nil {
		t.Fatal("Slack got no Applier")
	}
	return sock.apply
}

func answer(t *testing.T, apply slack.Applier) error {
	t.Helper()
	_, err := apply.Apply(context.Background(), writer.Batch{PlanID: testPlan,
		Writes: []writer.Write{{TxnID: "txn-1", CategoryID: "cat-1", Approve: true}}}, "brendan", "slack")
	return err
}

func TestCategorize_SlackAnswerDryRunWithoutHTTP(t *testing.T) {
	m := fakeModels(t)
	a, _ := classifyApp(t, config.Classify{Mode: config.ClassifyDryRun, JevAPIKey: testJevKey})
	c, err := a.newCategorizer()
	if err != nil {
		t.Fatal(err)
	}
	apply := slackApplier(t, a)
	if apply != slack.Applier(a.writer) {
		t.Error("Slack's Applier isn't the pipeline's writer")
	}
	if err := answer(t, apply); !errors.Is(err, writer.ErrDryRun) {
		t.Errorf("before a run: %v, want ErrDryRun", err)
	}
	c.After(context.Background(), time.Now())
	if err := answer(t, apply); !errors.Is(err, writer.ErrDryRun) {
		t.Errorf("after a dry_run run: %v, want ErrDryRun", err)
	}
	if m.patches.Load() != 0 {
		t.Errorf("%d PATCHes in dry_run", m.patches.Load())
	}
}

func TestCategorize_AutoDryRunUntilSetAndAfterDowngrade(t *testing.T) {
	m := fakeModels(t)
	old := currentCalibration
	currentCalibration = func() classify.Calibration { return classify.Calibration{} } // not auto-eligible
	t.Cleanup(func() { currentCalibration = old })
	a, logs := classifyApp(t, config.Classify{Mode: config.ClassifyAuto, JevAPIKey: testJevKey, AutoSince: "2026-11-15"})
	c, err := a.newCategorizer()
	if err != nil {
		t.Fatal(err)
	}
	apply := slackApplier(t, a)
	if err := answer(t, apply); !errors.Is(err, writer.ErrDryRun) {
		t.Errorf("auto before the first run: %v, want ErrDryRun", err)
	}
	a.gate.Set(writer.ModeAuto) // as if an earlier run was effective auto
	c.After(context.Background(), time.Now())
	if a.gate.Mode() != writer.ModeDryRun {
		t.Fatalf("gate after a downgraded run = %s", a.gate.Mode())
	}
	if err := answer(t, apply); !errors.Is(err, writer.ErrDryRun) {
		t.Errorf("auto after a downgrade: %v, want ErrDryRun", err)
	}
	if m.patches.Load() != 0 {
		t.Errorf("%d PATCHes", m.patches.Load())
	}
	if !strings.Contains(logs.String(), "auto downgraded") || !strings.Contains(logs.String(), `"effective_mode":"dry_run"`) {
		t.Errorf("no downgrade log line: %s", logs.String())
	}
}

func TestSecrets_CoverClassify(t *testing.T) {
	cfg, err := config.LoadFrom(func(k string) string {
		return map[string]string{
			"YNAB_ACCESS_TOKEN": "tok", "YNAB_BUDGET_ID": "plan",
			"CLASSIFY_MODE": "dry_run", "JEV_API_KEY": testJevKey, "CLAUDE_CODE_OAUTH_TOKEN": testClaudeTok,
		}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	got := scrub("jev said "+testJevKey+"; claude said "+testClaudeTok, secrets(cfg)...)
	for _, s := range []string{testJevKey, testClaudeTok} {
		if strings.Contains(got, s) {
			t.Errorf("scrub leaked %q: %q", s, got)
		}
	}
}

type fakeRunner struct{ err error }

func (f *fakeRunner) Probe(context.Context) error { return f.err }

// recAlerter records Send and Clear.
type recAlerter struct {
	mu     sync.Mutex
	sent   []alerts.Alert
	clears []string
}

func (r *recAlerter) Freshness(context.Context, bool, []finsync.AccountFreshness) error { return nil }
func (r *recAlerter) Anomalies(context.Context) error                                   { return nil }
func (r *recAlerter) Send(_ context.Context, al alerts.Alert) (alerts.Outcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, al)
	return alerts.Outcome(0), nil
}
func (r *recAlerter) Clear(_ context.Context, kind, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clears = append(r.clears, kind)
	return nil
}

func TestProbeClaude_FailureSendsOneCriticalAlert(t *testing.T) {
	a, logs := classifyApp(t, claudeOn)
	rec := &recAlerter{}
	a.alerter = rec
	runner := &fakeRunner{err: &llm.CallError{Reason: "call failed", ExitCode: 1, APIErrorStatus: 401}}
	a.claude = runner
	if err := a.probeClaude(context.Background()); err == nil {
		t.Fatal("probe failure not returned")
	}
	if len(rec.sent) != 1 {
		t.Fatalf("sent %d alerts, want 1", len(rec.sent))
	}
	al := rec.sent[0]
	if al.Kind != alerts.KindClaudeAuth || !alerts.Critical(al.Kind) || !strings.Contains(al.Text, "401") {
		t.Errorf("alert %+v (critical %v)", al, alerts.Critical(al.Kind))
	}
	if strings.Contains(al.Text+logs.String(), testClaudeTok) {
		t.Error("token leaked")
	}
	runner.err = nil
	if err := a.probeClaude(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(rec.sent) != 1 || len(rec.clears) != 1 || rec.clears[0] != alerts.KindClaudeAuth {
		t.Errorf("after a pass: sent %d, clears %v", len(rec.sent), rec.clears)
	}
}

func TestScheduler_ProbesAtStartupAndDaily(t *testing.T) {
	var probes, syncs atomic.Int32
	tk := &tickers{}
	stop := start(t, &Scheduler{
		Sync:       func(context.Context) error { syncs.Add(1); return nil },
		SyncEvery:  syncEvery,
		Probe:      func(context.Context) error { probes.Add(1); return errors.New("down") },
		ProbeEvery: 24 * time.Hour, NewTicker: tk.new,
	})
	defer stop()
	waitFor(t, func() bool { return probes.Load() == 1 && syncs.Load() == 1 })
	tickUntil(t, tk.get(24*time.Hour), func() bool { return probes.Load() >= 2 })
}

func TestCategorize_HistoryIncludesArchivedDiscretionaryPlans(t *testing.T) {
	fakeModels(t)
	a, _ := classifyApp(t, config.Classify{Mode: config.ClassifyDryRun, JevAPIKey: testJevKey})
	ctx := context.Background()
	str := func(s string) *string { return &s }
	seed := func(plan string, txns []store.Transaction) {
		err := a.db.Update(ctx, func(tx *store.Tx) error {
			if err := tx.UpsertAccounts(plan, []store.Account{{ID: "a1", Name: "Checking", Type: "checking", OnBudget: true}}); err != nil {
				return err
			}
			if err := tx.UpsertCategoryGroups(plan, []store.CategoryGroup{{ID: "g1", Name: "Everyday"}}); err != nil {
				return err
			}
			if err := tx.UpsertCategories(plan, []store.Category{
				{ID: "dine", CategoryGroupID: "g1", Name: "Dining"},
				{ID: "groc", CategoryGroupID: "g1", Name: "Groceries"},
				{ID: "disc", CategoryGroupID: "g1", Name: "Discretionary"},
			}); err != nil {
				return err
			}
			return tx.UpsertTransactions(plan, txns, time.Now())
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	today := time.Now().Format(time.DateOnly)
	seed(testPlan, []store.Transaction{{ID: "new", Date: today, Amount: -5000, Cleared: "cleared", AccountID: "a1",
		ImportPayeeName: str("Cafe")}})
	var old []store.Transaction
	for i, d := range []string{"2025-01-01", "2025-02-01", "2025-03-01"} {
		old = append(old, store.Transaction{ID: "o" + strconv.Itoa(i), Date: d, Amount: -5000, Cleared: "cleared",
			Approved: true, AccountID: "a1", ImportPayeeName: str("Cafe"), CategoryID: str("dine")})
	}
	seed("archived", old)

	c, err := a.newCategorizer()
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Eligible != 1 || res.Rule != 0 {
		t.Fatalf("before the scan: eligible %d, rule %d; want 1 and 0 (active plan alone has no labels)", res.Eligible, res.Rule)
	}

	if err := a.db.ReplacePlanScans(ctx, []store.PlanScan{
		{PlanID: "archived", Name: "Old", HasDiscretionary: true, ScannedAt: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	// A decision from the first run would block a re-decide; use a new txn.
	seed(testPlan, []store.Transaction{{ID: "new2", Date: today, Amount: -6000, Cleared: "cleared", AccountID: "a1",
		ImportPayeeName: str("Cafe")}})
	if res, err = c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if res.Rule < 1 {
		t.Errorf("after the scan: rule %d; the archived plan's Cafe → Dining labels didn't reach the pipeline (%+v)", res.Rule, res)
	}
}
