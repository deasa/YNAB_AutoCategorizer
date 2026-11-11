package calibrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// Fixture ids, payees and memos that must never reach the output.
const (
	rpPlan  = "plan-7f3c9a1e-SECRET"
	rpPayee = "Joe's Secret Cafe"
	rpMemo  = "memo: gift for Sam"
)

var rp0 = time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)

type fakeReportStore struct {
	decs    []store.Decision
	answers []store.Question
	mirror  map[string]store.HumanCategory
	open    int
	pending int
	writes  int
}

func (f *fakeReportStore) ReportPlans(context.Context) ([]string, error) {
	return []string{rpPlan}, nil
}
func (f *fakeReportStore) DecisionsBetween(_ context.Context, plan string, _, _ time.Time) ([]store.Decision, error) {
	return f.decs, nil
}
func (f *fakeReportStore) AnsweredQuestions(_ context.Context, _, kind string, _, _ time.Time) ([]store.Question, error) {
	if kind != store.QuestionDryRunCheck {
		return nil, fmt.Errorf("kind %q", kind)
	}
	return f.answers, nil
}
func (f *fakeReportStore) CurrentHumanCategory(_ context.Context, _, txn string) (store.HumanCategory, bool, error) {
	h, ok := f.mirror[txn]
	return h, ok, nil
}
func (f *fakeReportStore) CountQuestions(context.Context, string) (int, int, error) {
	return f.open, f.pending, nil
}
func (f *fakeReportStore) CountWriteLog(context.Context, string, time.Time, time.Time) (int, error) {
	return f.writes, nil
}

func catID(name string) string { return "cat-" + strings.ToLower(name) + "-4b1d9e" }
func txnID(i int) string       { return fmt.Sprintf("txn-%04d-a9e3c1", i) }

func rpDecision(i int, outcome, cat string, top ...string) store.Decision {
	f, _ := json.Marshal(map[string]any{"payee": rpPayee, "memo": rpMemo, "top": top})
	return store.Decision{ID: int64(i), PlanID: rpPlan, TxnID: txnID(i), CategoryID: sp(catID(cat)), CategoryName: sp(cat),
		Method: store.MethodModel, Model: "jev-1.14.0", Mode: store.ModeDryRun, Outcome: outcome, Features: f,
		CreatedAt: rp0.Add(time.Duration(i) * time.Hour)}
}

// rpAnswer is a dry_run_check answered with cat at the given time.
func rpAnswer(i int, cat string, at time.Time) store.Question {
	return store.Question{PlanID: rpPlan, Kind: store.QuestionDryRunCheck, SubjectID: txnID(i),
		Options: []store.QuestionOption{{CategoryID: catID(cat), CategoryName: cat}},
		Status:  store.QuestionAnswered, Answer: sp(catID(cat)), AnsweredBy: sp(store.UserBrendan), AnsweredAt: &at}
}

func human(cat string, seen time.Time) store.HumanCategory {
	return store.HumanCategory{HumanSet: true, Approved: true, CategoryID: sp(catID(cat)), CategoryName: sp(cat),
		FirstSeenAt: seen, SeenAt: seen}
}

func render(t *testing.T, st ReportStore) (DryRunReport, string) {
	t.Helper()
	r, err := BuildDryRunReport(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := r.Write(&b, time.UTC); err != nil {
		t.Fatal(err)
	}
	return r, b.String()
}

// workedExample: 10 judged would_apply (9 right, 1 wrong), 2 judged asked (1
// with the label third in its top-3), one rule decision, one skip, one failed
// second opinion.
func workedExample() *fakeReportStore {
	f := &fakeReportStore{mirror: map[string]store.HumanCategory{}, open: 2, pending: 3}
	for i := 1; i <= 10; i++ {
		d := rpDecision(i, store.OutcomeWouldApply, "Dining", "Dining", "Fuel")
		f.decs = append(f.decs, d)
		switch {
		case i == 10: // wrong, judged by the mirror
			f.mirror[d.TxnID] = human("Groceries", d.CreatedAt.Add(time.Minute))
		case i%2 == 0: // right, judged by an answer
			f.answers = append(f.answers, rpAnswer(i, "Dining", d.CreatedAt.Add(time.Minute)))
		default: // right, judged by the mirror
			f.mirror[d.TxnID] = human("Dining", d.CreatedAt.Add(time.Minute))
		}
	}
	hit := rpDecision(11, store.OutcomeAsked, "Fuel", "Fuel", "Dining", "Gifts")
	miss := rpDecision(12, store.OutcomeAsked, "Fuel", "Fuel", "Dining", "Gifts", "Medical")
	f.decs = append(f.decs, hit, miss)
	f.answers = append(f.answers, rpAnswer(11, "Gifts", hit.CreatedAt.Add(time.Minute)),
		rpAnswer(12, "Medical", miss.CreatedAt.Add(time.Minute)))

	rule := rpDecision(13, store.OutcomeWouldApply, "Dining")
	rule.Method = store.MethodRule
	rule.SecondOpinion = json.RawMessage(`{"model":"claude","failed":true}`)
	skip := rpDecision(14, store.OutcomeSkipped, "Dining")
	skip.CategoryID, skip.CategoryName = nil, nil
	f.decs = append(f.decs, rule, skip)
	return f
}

func TestDryRunReport_WorkedExample(t *testing.T) {
	r, out := render(t, workedExample())
	if r.WouldApplyJudged != 10 || r.WouldApplyCorrect != 9 || r.Judged != 12 || r.AskedJudged != 2 || r.AskedTop3 != 1 {
		t.Fatalf("counts: %+v", r)
	}
	// 9/10 at z = 1.96: p̂ = 0.9; centre = 0.9 + 3.8416/20 = 1.09208; margin =
	// 1.96·√(0.009 + 3.8416/400) = 0.267338; ÷ (1 + 0.38416) → 0.59585 to 0.98213.
	if math.Abs(r.PrecisionLow-0.59585) > 5e-5 || math.Abs(r.PrecisionHigh-0.98213) > 5e-5 {
		t.Errorf("interval = %.5f to %.5f, want 0.59585 to 0.98213", r.PrecisionLow, r.PrecisionHigh)
	}
	for _, want := range []string{
		"decisions: 14 (first 2026-11-03, last 2026-11-04)",
		"by method: rule 1, model 13",
		"by outcome: would_apply 11, asked 2, skipped 1, applied 0",
		"would_apply: 11",
		"judged: 12 (would_apply 10, asked 2)",
		"precision (would_apply): 9/10 = 90.0% (Wilson 95%: 59.6% to 98.2%; target 85.0%)",
		"top-3 hit rate (asked): 1/2 = 50.0%",
		"  Dining → Groceries: 1",
		"questions: 2 open, 3 pending",
		"model errors: 1 ",
		"judged < 30: thin sample",
	} {
		if !strings.Contains(out, want+"\n") && !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\nwrites: 0\n") {
		t.Errorf("last line isn't writes: 0:\n%s", out)
	}
}

func TestDryRunReport_LabelOlderThanDecisionDoesNotCount(t *testing.T) {
	d := rpDecision(1, store.OutcomeWouldApply, "Dining")
	asked := rpDecision(2, store.OutcomeAsked, "Fuel", "Fuel", "Dining")
	f := &fakeReportStore{
		decs: []store.Decision{d, asked},
		// An answer and a human label, both before their decisions (and one at
		// the same instant): neither judges it.
		answers: []store.Question{rpAnswer(1, "Dining", d.CreatedAt.Add(-time.Minute)), rpAnswer(2, "Dining", asked.CreatedAt)},
		mirror: map[string]store.HumanCategory{
			d.TxnID:     human("Groceries", d.CreatedAt.Add(-time.Hour)),
			asked.TxnID: human("Fuel", asked.CreatedAt.Add(-time.Second)),
		},
	}
	r, out := render(t, f)
	if r.Judged != 0 || r.WouldApplyJudged != 0 || r.AskedJudged != 0 {
		t.Errorf("judged %d (would_apply %d, asked %d), want 0: %s", r.Judged, r.WouldApplyJudged, r.AskedJudged, out)
	}

	// The earliest later label wins: a wrong mirror label before a right answer.
	f.mirror[d.TxnID] = human("Groceries", d.CreatedAt.Add(time.Minute))
	f.answers = []store.Question{rpAnswer(1, "Dining", d.CreatedAt.Add(time.Hour))}
	if r, _ = render(t, f); r.WouldApplyJudged != 1 || r.WouldApplyCorrect != 0 {
		t.Errorf("first label: judged %d, correct %d; want 1, 0", r.WouldApplyJudged, r.WouldApplyCorrect)
	}
	// An unapproved guess, a deleted row and a split are handled: only the split judges (as wrong).
	f.answers = nil
	h := human("Dining", d.CreatedAt.Add(time.Minute))
	h.HumanSet = false
	f.mirror[d.TxnID] = h
	if r, _ = render(t, f); r.WouldApplyJudged != 0 {
		t.Errorf("not human-set: judged %d", r.WouldApplyJudged)
	}
	h.HumanSet, h.Deleted = true, true
	f.mirror[d.TxnID] = h
	if r, _ = render(t, f); r.WouldApplyJudged != 0 {
		t.Errorf("deleted: judged %d", r.WouldApplyJudged)
	}
	f.mirror[d.TxnID] = store.HumanCategory{HumanSet: true, IsSplit: true, SeenAt: d.CreatedAt.Add(time.Minute)}
	if r, out = render(t, f); r.WouldApplyJudged != 1 || r.WouldApplyCorrect != 0 || !strings.Contains(out, "Dining → (split): 1") {
		t.Errorf("split: judged %d, correct %d\n%s", r.WouldApplyJudged, r.WouldApplyCorrect, out)
	}
}

func TestDryRunReport_ThinSampleWarning(t *testing.T) {
	f := &fakeReportStore{mirror: map[string]store.HumanCategory{}}
	add := func(n int) {
		for i := len(f.decs) + 1; len(f.decs) < n; i++ {
			d := rpDecision(i, store.OutcomeWouldApply, "Dining")
			f.decs = append(f.decs, d)
			f.mirror[d.TxnID] = human("Dining", d.CreatedAt.Add(time.Minute))
		}
	}
	add(29)
	if _, out := render(t, f); !strings.Contains(out, "\njudged < 30: thin sample\n") {
		t.Errorf("29 judged: no warning:\n%s", out)
	}
	add(30)
	if _, out := render(t, f); strings.Contains(out, "thin sample") {
		t.Errorf("30 judged: warned:\n%s", out)
	}
	// 30 judged, but only 20 in the would_apply band: the precision n is thin.
	f.decs = f.decs[:20]
	for i := 100; i < 110; i++ {
		d := rpDecision(i, store.OutcomeAsked, "Dining", "Dining")
		f.decs = append(f.decs, d)
		f.mirror[d.TxnID] = human("Dining", d.CreatedAt.Add(time.Minute))
	}
	if _, out := render(t, f); !strings.Contains(out, "\nwould_apply judged < 30: thin sample\n") ||
		strings.Contains(out, "\njudged < 30") {
		t.Errorf("20 would_apply of 30 judged:\n%s", out)
	}
}

func TestDryRunReport_NoIDsPayeesOrMemos(t *testing.T) {
	f := workedExample()
	f.writes = 2
	_, out := render(t, f)
	banned := []string{rpPlan, "plan-", rpPayee, "Secret", rpMemo, "Sam", "txn-", "cat-", "a9e3c1", "4b1d9e", "jev-1.14"}
	for _, s := range banned {
		if strings.Contains(out, s) {
			t.Errorf("output contains %q:\n%s", s, out)
		}
	}
	if !strings.HasSuffix(out, "\nwrites: 2\n") {
		t.Errorf("last line:\n%s", out)
	}
}

func TestDryRunReport_CleanDB(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, out := render(t, db)
	for _, want := range []string{"decisions: 0 (first -, last -)", "judged: 0", "no judged decisions",
		"questions: 0 open, 0 pending", "judged < 30: thin sample"} {
		if !strings.Contains(out, want) {
			t.Errorf("clean DB output lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasSuffix(out, "\nwrites: 0\n") {
		t.Errorf("clean DB: last line isn't writes: 0:\n%s", out)
	}
}
