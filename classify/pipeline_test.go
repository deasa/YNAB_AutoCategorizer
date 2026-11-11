package classify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
)

const (
	tPlan    = "plan-1"
	tModel   = "jev-test-1"
	tAcct    = "acct-checking"
	tOther   = "acct-savings"
	tGroup   = "grp-everyday"
	tToday   = "2026-11-10"
	tSince   = "2026-10-01"
	tOldDate = "2026-09-28" // within the backlog, before tSince
)

var tCats = []string{"Groceries", "Dining", "Fuel", "Discretionary"}

func catID(name string) string { return "cat-" + strings.ToLower(name) }


// --- fakes ---

// events is the order of SetMode and Apply calls across fakes.
type events struct{ log []string }

func (e *events) add(s string) { e.log = append(e.log, s) }

type fakeModel struct {
	id    string
	preds map[string]map[string]float64 // payee → probs
	err   error
	calls int
	sent  []string // payees, every call
}

func (m *fakeModel) Classify(_ context.Context, txns []Txn, _ []Candidate) ([]Prediction, error) {
	m.calls++
	for _, t := range txns {
		m.sent = append(m.sent, t.Payee)
	}
	if m.err != nil {
		return nil, m.err
	}
	out := make([]Prediction, len(txns))
	for i, t := range txns {
		out[i] = Prediction{Probs: m.preds[t.Payee], Model: m.id}
	}
	return out, nil
}

type fakeApplier struct {
	ev      *events
	batches []writer.Batch
	actor   []string
}

func (a *fakeApplier) Apply(_ context.Context, b writer.Batch, actor, via string) ([]writer.Outcome, error) {
	a.ev.add("apply")
	a.batches = append(a.batches, b)
	a.actor = append(a.actor, actor+"/"+via)
	out := make([]writer.Outcome, len(b.Writes))
	for i, w := range b.Writes {
		out[i] = writer.Outcome{TxnID: w.TxnID, Status: writer.StatusDone}
	}
	return out, nil
}

func (a *fakeApplier) writes() []writer.Write {
	var out []writer.Write
	for _, b := range a.batches {
		out = append(out, b.Writes...)
	}
	return out
}

type fakeAsker struct {
	db     *store.DB
	n      int
	asked  []store.Question
	closed []string // kind:reason
}

func (a *fakeAsker) Ask(ctx context.Context, id int64) error {
	q, _, err := a.db.QuestionByID(ctx, id)
	if err != nil {
		return err
	}
	a.n++
	a.asked = append(a.asked, q)
	return a.db.MarkQuestionPosted(ctx, id, "C1", fmt.Sprintf("ts-%d", a.n))
}

func (a *fakeAsker) Closed(_ context.Context, q store.Question, reason string) error {
	a.closed = append(a.closed, q.Kind+":"+reason)
	return nil
}

// --- fixture ---

type fixture struct {
	t      *testing.T
	ctx    context.Context
	db     *store.DB
	ev     *events
	model  *fakeModel
	second *fakeModel
	apply  *fakeApplier
	asker  *fakeAsker
	modes  []writer.Mode
	cfg    Config
	n      int
}

func newFixture(t *testing.T, mode writer.Mode) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &fixture{t: t, ctx: ctx, db: db, ev: &events{},
		model: &fakeModel{id: tModel, preds: map[string]map[string]float64{}}}
	f.apply = &fakeApplier{ev: f.ev}
	f.asker = &fakeAsker{db: db}

	err = db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertAccounts(tPlan, []store.Account{
			{ID: tAcct, Name: "Checking", Type: "checking", OnBudget: true},
			{ID: tOther, Name: "Savings", Type: "savings", OnBudget: true},
		}); err != nil {
			return err
		}
		if err := tx.UpsertCategoryGroups(tPlan, []store.CategoryGroup{{ID: tGroup, Name: "Everyday"}}); err != nil {
			return err
		}
		var cats []store.Category
		for _, n := range tCats {
			cats = append(cats, store.Category{ID: catID(n), CategoryGroupID: tGroup, Name: n})
		}
		return tx.UpsertCategories(tPlan, cats)
	})
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.DateOnly, tToday)
	f.cfg = Config{PlanID: tPlan, Mode: mode, Policy: Policy{Version: 1}, ModelID: tModel, Model: f.model,
		Store: db, Applier: f.apply, Asker: f.asker, AutoSince: tSince,
		SetMode: func(m writer.Mode) { f.modes = append(f.modes, m); f.ev.add("mode:" + string(m)) },
		Now:     func() time.Time { return now.Add(12 * time.Hour) }}
	return f
}

type txnOpt func(*store.Transaction)

func approvedAs(cat string) txnOpt {
	return func(t *store.Transaction) { t.Approved, t.CategoryID = true, ptr(catID(cat)) }
}
func dated(d string) txnOpt { return func(t *store.Transaction) { t.Date = d } }
func transfer() txnOpt {
	return func(t *store.Transaction) { t.TransferAccountID = ptr(tOther) }
}

// txn mirrors a new transaction (unapproved, uncategorized, dated today by
// default) for payee and returns its id.
func (f *fixture) txn(payee string, opts ...txnOpt) string {
	f.t.Helper()
	f.n++
	id := fmt.Sprintf("txn-%02d", f.n)
	tr := store.Transaction{ID: id, Date: tToday, Amount: -12340, Cleared: "uncleared", AccountID: tAcct,
		ImportPayeeName: ptr(payee)}
	for _, o := range opts {
		o(&tr)
	}
	err := f.db.Update(f.ctx, func(tx *store.Tx) error {
		return tx.UpsertTransactions(tPlan, []store.Transaction{tr}, time.Now())
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// cal is a calibration that makes auto effective for this fixture.
func (f *fixture) cal() Calibration {
	f.t.Helper()
	cats, err := f.db.CategoriesWithGroups(f.ctx, tPlan)
	if err != nil {
		f.t.Fatal(err)
	}
	return Calibration{Model: tModel, PolicyHash: PolicyHash(f.cfg.Policy),
		CandidatesHash: CandidatesHash(Candidates(cats, f.cfg.Policy).Candidates), Tau: ptr(0.8), AutoEligible: true}
}

func (f *fixture) run(cal Calibration) Result {
	f.t.Helper()
	res, err := New(f.cfg).Run(f.ctx, cal)
	if err != nil {
		f.t.Fatalf("Run: %v", err)
	}
	return res
}

func (f *fixture) decision(txnID string) (store.Decision, bool) {
	f.t.Helper()
	d, ok, err := f.db.LatestDecision(f.ctx, tPlan, txnID)
	if err != nil {
		f.t.Fatal(err)
	}
	return d, ok
}

func (f *fixture) questions(txnID string) []store.Question {
	f.t.Helper()
	qs, err := f.db.QuestionsBySubject(f.ctx, tPlan, txnID)
	if err != nil {
		f.t.Fatal(err)
	}
	return qs
}

func (f *fixture) live(txnID string) []store.Question {
	var out []store.Question
	for _, q := range f.questions(txnID) {
		if q.Status == store.QuestionOpen || q.Status == store.QuestionPending {
			out = append(out, q)
		}
	}
	return out
}

func sure(cat string) map[string]float64 { return map[string]float64{cat: 0.95, "Dining": 0.03, "Fuel": 0.02} }
func unsure() map[string]float64 {
	return map[string]float64{"Groceries": 0.5, "Dining": 0.3, "Fuel": 0.15, "Discretionary": 0.05}
}

// --- tests ---

func TestRun_Paths(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(f *fixture) string
		method   string
		outcome  string
		category string
		applied  bool
		asked    bool
	}{
		{name: "guardrail skip", setup: func(f *fixture) string { return f.txn("Move to savings", transfer()) },
			method: store.MethodRule, outcome: store.OutcomeSkipped},
		{name: "rule", setup: func(f *fixture) string {
			for i := 0; i < RuleMinExamples; i++ {
				f.txn("Shell", approvedAs("Fuel"), dated("2026-10-1"+fmt.Sprint(i)))
			}
			return f.txn("Shell")
		}, method: store.MethodRule, outcome: store.OutcomeApplied, category: "Fuel", applied: true},
		{name: "tau pass", setup: func(f *fixture) string {
			f.model.preds["Corner Market"] = sure("Groceries")
			return f.txn("Corner Market")
		}, method: store.MethodModel, outcome: store.OutcomeApplied, category: "Groceries", applied: true},
		{name: "tau miss", setup: func(f *fixture) string {
			f.model.preds["Mystery Co"] = unsure()
			return f.txn("Mystery Co")
		}, method: store.MethodModel, outcome: store.OutcomeAsked, category: "Groceries", asked: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, writer.ModeAuto)
			id := tc.setup(f)
			res := f.run(f.cal())
			if res.Mode != writer.ModeAuto || len(res.Warnings) != 0 {
				t.Fatalf("mode %s warnings %v", res.Mode, res.Warnings)
			}
			d, ok := f.decision(id)
			if !ok {
				t.Fatal("no decision")
			}
			if d.Method != tc.method || d.Outcome != tc.outcome || d.Mode != store.ModeAuto || d.Model != tModel {
				t.Errorf("decision = %s/%s/%s/%s, want %s/%s/auto", d.Method, d.Outcome, d.Mode, d.Model, tc.method, tc.outcome)
			}
			if got := deref(d.CategoryName); got != tc.category {
				t.Errorf("category = %q, want %q", got, tc.category)
			}
			if tc.category != "" && deref(d.CategoryID) != catID(tc.category) {
				t.Errorf("category id = %q", deref(d.CategoryID))
			}
			ws := f.apply.writes()
			if tc.applied {
				want := writer.Write{TxnID: id, CategoryID: catID(tc.category), Approve: true}
				if len(ws) != 1 || ws[0] != want || f.apply.actor[0] != "system/scheduler" {
					t.Errorf("writes = %+v by %v, want %+v by system/scheduler", ws, f.apply.actor, want)
				}
			} else if len(ws) != 0 {
				t.Errorf("unexpected writes %+v", ws)
			}
			qs := f.questions(id)
			if tc.asked {
				if len(qs) != 1 || qs[0].Kind != store.QuestionCategorize || qs[0].Status != store.QuestionOpen {
					t.Fatalf("questions = %+v", qs)
				}
				var names []string
				for _, o := range qs[0].Options {
					names = append(names, o.CategoryName)
				}
				if !slices.Equal(names, []string{"Groceries", "Dining", "Fuel"}) {
					t.Errorf("options = %v, want top-3", names)
				}
			} else if len(qs) != 0 {
				t.Errorf("unexpected questions %+v", qs)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestRun_ModelErrorRetriesNextRun(t *testing.T) {
	f := newFixture(t, writer.ModeAuto)
	f.model.preds["Corner Market"] = sure("Groceries")
	id := f.txn("Corner Market")
	f.model.err = errors.New("jev: 503")
	res := f.run(f.cal())
	if res.ModelFailures != 1 {
		t.Errorf("ModelFailures = %d", res.ModelFailures)
	}
	if _, ok := f.decision(id); ok {
		t.Error("model error wrote a decision")
	}
	if qs := f.questions(id); len(qs) != 0 || f.asker.n != 0 {
		t.Errorf("model error asked: %+v", qs)
	}
	if len(f.apply.batches) != 0 {
		t.Error("model error applied")
	}

	// A null result is a failure too.
	f.model.err = nil
	f.model.preds["Corner Market"] = nil
	if res := f.run(f.cal()); res.ModelFailures != 1 {
		t.Errorf("null result: ModelFailures = %d", res.ModelFailures)
	}
	if _, ok := f.decision(id); ok {
		t.Error("null result wrote a decision")
	}

	f.model.preds["Corner Market"] = sure("Groceries")
	f.run(f.cal())
	if d, ok := f.decision(id); !ok || d.Outcome != store.OutcomeApplied {
		t.Errorf("retry: decision = %+v, %v", d, ok)
	}
	if f.model.calls != 3 {
		t.Errorf("model calls = %d, want 3", f.model.calls)
	}
}

func TestRun_DryRunNeverApplies(t *testing.T) {
	f := newFixture(t, writer.ModeDryRun)
	for i := 0; i < RuleMinExamples; i++ {
		f.txn("Shell", approvedAs("Fuel"), dated("2026-10-1"+fmt.Sprint(i)))
	}
	rule := f.txn("Shell")
	f.model.preds["Corner Market"] = sure("Groceries")
	pass := f.txn("Corner Market")
	f.model.preds["Mystery Co"] = unsure()
	miss := f.txn("Mystery Co")

	res := f.run(Calibration{}) // the placeholder: τ 0.8
	if res.Mode != writer.ModeDryRun {
		t.Fatalf("mode = %s", res.Mode)
	}
	if len(f.apply.batches) != 0 {
		t.Fatal("dry run called the Applier")
	}
	for id, want := range map[string]string{rule: store.OutcomeWouldApply, pass: store.OutcomeWouldApply, miss: store.OutcomeAsked} {
		d, ok := f.decision(id)
		if !ok || d.Outcome != want || d.Mode != store.ModeDryRun {
			t.Errorf("%s: decision %+v, want %s/dry_run", id, d, want)
		}
		qs := f.questions(id)
		if len(qs) != 1 || qs[0].Kind != store.QuestionDryRunCheck {
			t.Fatalf("%s: questions %+v", id, qs)
		}
		verdict := "would apply"
		if want == store.OutcomeAsked {
			verdict = "would ask"
		}
		if !strings.Contains(qs[0].Options[0].Label, verdict) {
			t.Errorf("%s: first option %+v, want %q", id, qs[0].Options[0], verdict)
		}
	}
	if res.WouldApply != 2 || res.Asked != 3 {
		t.Errorf("result = %+v", res)
	}
}

func TestRun_AutoDowngrades(t *testing.T) {
	cases := map[string]func(c *Calibration){
		"not auto-eligible": func(c *Calibration) { c.AutoEligible = false },
		"placeholder tau":   func(c *Calibration) { c.Tau = nil },
		"model mismatch":    func(c *Calibration) { c.Model = "jev-other" },
		"policy mismatch":   func(c *Calibration) { c.PolicyHash = "x" },
		"candidates change": func(c *Calibration) { c.CandidatesHash = "x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, writer.ModeAuto)
			f.model.preds["Corner Market"] = sure("Groceries")
			id := f.txn("Corner Market")
			cal := f.cal()
			mutate(&cal)
			res := f.run(cal)
			if res.Mode != writer.ModeDryRun || len(res.Warnings) != 1 {
				t.Errorf("mode %s warnings %v, want dry_run with a warning", res.Mode, res.Warnings)
			}
			if !slices.Equal(f.modes, []writer.Mode{writer.ModeDryRun}) {
				t.Errorf("setter got %v", f.modes)
			}
			if len(f.apply.batches) != 0 {
				t.Error("downgraded run applied")
			}
			if d, _ := f.decision(id); d.Outcome != store.OutcomeWouldApply || d.Mode != store.ModeDryRun {
				t.Errorf("decision %+v", d)
			}
		})
	}
}

func TestRun_SecondOpinionNeverApplies(t *testing.T) {
	f := newFixture(t, writer.ModeAuto)
	f.second = &fakeModel{id: "claude-cli-x/haiku", preds: map[string]map[string]float64{
		"Mystery Co": {"Discretionary": 1.0},
	}}
	f.cfg.SecondOpinion = f.second
	f.cfg.SecondOpinionModelID = "claude-haiku-real"
	f.model.preds["Mystery Co"] = unsure()
	id := f.txn("Mystery Co")

	res := f.run(f.cal())
	if len(f.apply.batches) != 0 {
		t.Fatal("second opinion at p=1.0 caused an apply")
	}
	d, _ := f.decision(id)
	if d.Outcome != store.OutcomeAsked {
		t.Errorf("outcome %s, want asked", d.Outcome)
	}
	var so secondOpinion
	if err := json.Unmarshal(d.SecondOpinion, &so); err != nil {
		t.Fatal(err)
	}
	if so.Model != "claude-haiku-real" || so.Category != "Discretionary" || so.P != 1.0 {
		t.Errorf("second opinion = %+v", so)
	}
	qs := f.questions(id)
	if len(qs) != 1 {
		t.Fatalf("questions %+v", qs)
	}
	last := qs[0].Options[len(qs[0].Options)-1]
	if last.CategoryName != "Discretionary" || !strings.Contains(last.Label, "second opinion") {
		t.Errorf("hint option = %+v", last)
	}
	if res.SecondOpinions != 1 {
		t.Errorf("SecondOpinions = %d", res.SecondOpinions)
	}

	// A failed second opinion still asks.
	f2 := newFixture(t, writer.ModeAuto)
	f2.cfg.SecondOpinion = &fakeModel{err: errors.New("claude: exit 1")}
	f2.model.preds["Mystery Co"] = unsure()
	id2 := f2.txn("Mystery Co")
	f2.run(f2.cal())
	if qs := f2.questions(id2); len(qs) != 1 || qs[0].Status != store.QuestionOpen {
		t.Errorf("failed second opinion: questions %+v", qs)
	}
	if d, _ := f2.decision(id2); !strings.Contains(string(d.SecondOpinion), `"failed":true`) {
		t.Errorf("failed second opinion not logged: %s", d.SecondOpinion)
	}
}

func TestRun_QuestionCap(t *testing.T) {
	f := newFixture(t, writer.ModeAuto)
	var ids []string
	for i := 0; i < 6; i++ {
		payee := fmt.Sprintf("Mystery %d", i)
		f.model.preds[payee] = unsure()
		ids = append(ids, f.txn(payee, dated(fmt.Sprintf("2026-11-0%d", i+1))))
	}
	res := f.run(f.cal())
	if res.Posted != 5 || res.Pending != 1 || res.Asked != 6 {
		t.Fatalf("result = %+v", res)
	}
	pending, _ := f.db.PendingQuestions(f.ctx, tPlan)
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	// A slot frees.
	open, _ := f.db.OpenQuestions(f.ctx, tPlan)
	if ok, err := f.db.ClaimAnswer(f.ctx, open[0].ID, "Groceries", store.UserBrendan, time.Now()); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	res = f.run(f.cal())
	if res.Posted != 1 || res.Pending != 0 || res.AlreadyDecided != 6 {
		t.Errorf("second run = %+v", res)
	}
	if q, _, _ := f.db.QuestionByID(f.ctx, pending[0].ID); q.Status != store.QuestionOpen {
		t.Errorf("6th question status %s", q.Status)
	}
	if f.model.calls != 1 || len(f.model.sent) != 6 {
		t.Errorf("model calls %d sent %d, want 1 and 6", f.model.calls, len(f.model.sent))
	}
}

func TestRun_DecidedNotResent(t *testing.T) {
	f := newFixture(t, writer.ModeDryRun)
	f.model.preds["Corner Market"] = sure("Groceries")
	f.txn("Corner Market")
	f.run(Calibration{})
	res := f.run(Calibration{})
	if f.model.calls != 1 || res.AlreadyDecided != 1 {
		t.Errorf("model calls %d, already decided %d", f.model.calls, res.AlreadyDecided)
	}
}

func TestRun_AutoSince(t *testing.T) {
	f := newFixture(t, writer.ModeAuto)
	f.model.preds["Corner Market"] = sure("Groceries")
	f.model.preds["Mystery Co"] = unsure()
	old1 := f.txn("Corner Market", dated(tOldDate))
	old2 := f.txn("Mystery Co", dated(tOldDate))
	res := f.run(f.cal())
	if len(f.apply.batches) != 0 || f.asker.n != 0 || f.model.calls != 0 {
		t.Errorf("old rows reached apply %d / ask %d / model %d", len(f.apply.batches), f.asker.n, f.model.calls)
	}
	for _, id := range []string{old1, old2} {
		if qs := f.questions(id); len(qs) != 0 {
			t.Errorf("%s asked: %+v", id, qs)
		}
	}
	if res.BeforeAutoSince != 2 {
		t.Errorf("BeforeAutoSince = %d", res.BeforeAutoSince)
	}
}

func TestRun_EnteringAutoClosesDryRunChecks(t *testing.T) {
	f := newFixture(t, writer.ModeDryRun)
	for i := 0; i < 6; i++ {
		payee := fmt.Sprintf("Mystery %d", i)
		f.model.preds[payee] = unsure()
		f.txn(payee, dated(fmt.Sprintf("2026-11-0%d", i+1)))
	}
	if res := f.run(Calibration{}); res.Posted != 5 || res.Pending != 1 {
		t.Fatalf("dry run = %+v", res)
	}
	askedBefore := f.asker.n

	f.cfg.Mode = writer.ModeAuto
	res := f.run(f.cal())
	if res.SupersededByAuto != 6 {
		t.Errorf("SupersededByAuto = %d, want 6", res.SupersededByAuto)
	}
	if got := len(f.asker.closed); got != 5 {
		t.Errorf("message edits = %d, want 5 (the posted ones)", got)
	}
	for _, c := range f.asker.closed {
		if c != store.QuestionDryRunCheck+":"+ReasonSupersededByAuto {
			t.Errorf("closed %s", c)
		}
	}
	for _, q := range f.asker.asked[askedBefore:] {
		if q.Kind == store.QuestionDryRunCheck {
			t.Errorf("auto posted a dry_run_check: %+v", q)
		}
	}
	// No dry_run_check is live any more.
	for _, list := range [][]store.Question{must(f.db.OpenQuestions(f.ctx, tPlan)), must(f.db.PendingQuestions(f.ctx, tPlan))} {
		for _, q := range list {
			if q.Kind == store.QuestionDryRunCheck {
				t.Errorf("live dry_run_check %+v", q)
			}
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestRun_SetterBeforeApplier(t *testing.T) {
	f := newFixture(t, writer.ModeAuto)
	f.model.preds["Corner Market"] = sure("Groceries")
	f.txn("Corner Market")
	f.run(f.cal())
	if !slices.Equal(f.ev.log, []string{"mode:auto", "apply"}) {
		t.Errorf("events = %v", f.ev.log)
	}

	// Downgrade: the setter publishes dry_run first, and nothing applies.
	f.ev.log = nil
	f.txn("Corner Market")
	cal := f.cal()
	cal.CandidatesHash = "stale"
	f.run(cal)
	if !slices.Equal(f.ev.log, []string{"mode:dry_run"}) {
		t.Errorf("downgrade events = %v", f.ev.log)
	}

	// Off publishes off and does nothing else.
	f.ev.log = nil
	f.cfg.Mode = writer.ModeOff
	calls := f.model.calls
	if res := f.run(f.cal()); res.Mode != writer.ModeOff || f.model.calls != calls {
		t.Errorf("off: %+v", res)
	}
	if !slices.Equal(f.ev.log, []string{"mode:off"}) {
		t.Errorf("off events = %v", f.ev.log)
	}
}

func TestRun_ModeChangeRedecides(t *testing.T) {
	f := newFixture(t, writer.ModeAuto)
	f.model.preds["Mystery Co"] = unsure()
	id := f.txn("Mystery Co")

	f.run(f.cal()) // auto: asks
	first := f.live(id)
	if len(first) != 1 || first[0].Kind != store.QuestionCategorize {
		t.Fatalf("auto questions %+v", first)
	}

	down := f.cal()
	down.AutoEligible = false
	f.run(down) // downgraded: the auto decision isn't current
	second := f.live(id)
	if len(second) != 1 || second[0].Kind != store.QuestionDryRunCheck {
		t.Fatalf("dry-run questions %+v", second)
	}
	if q, _, _ := f.db.QuestionByID(f.ctx, first[0].ID); q.Status != store.QuestionClosed || deref(q.CloseReason) != ReasonSuperseded {
		t.Errorf("older question %s/%s, want closed/superseded", q.Status, deref(q.CloseReason))
	}

	f.run(f.cal()) // back to auto: the dry_run decision isn't current either
	third := f.live(id)
	if len(third) != 1 || third[0].Kind != store.QuestionCategorize || third[0].ID == first[0].ID {
		t.Fatalf("re-decided questions %+v", third)
	}
	if q, _, _ := f.db.QuestionByID(f.ctx, second[0].ID); q.Status != store.QuestionClosed {
		t.Errorf("dry-run question still %s", q.Status)
	}
	if f.model.calls != 3 {
		t.Errorf("model calls = %d, want 3", f.model.calls)
	}
	hist, _ := f.db.DecisionHistory(f.ctx, tPlan, id)
	if len(hist) != 3 {
		t.Errorf("decision history = %d rows", len(hist))
	}
}
