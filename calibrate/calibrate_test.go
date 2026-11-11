package calibrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const testModel = "jev-test"

var cands = []classify.Candidate{
	{Name: "Dining", Group: "Everyday"},
	{Name: "Fuel", Group: "Auto"},
	{Name: "Discretionary", Group: "Fun"},
}

// answer is the fake model's reply for one payee.
type answer struct {
	top string
	p   float64
}

// fakeModel answers by payee and records what it saw.
type fakeModel struct {
	answers map[string]answer
	seen    []classify.Txn
	calls   int
	err     error
}

func (f *fakeModel) Classify(_ context.Context, txns []classify.Txn, cs []classify.Candidate) ([]classify.Prediction, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	f.seen = append(f.seen, txns...)
	out := make([]classify.Prediction, len(txns))
	for i, t := range txns {
		a, ok := f.answers[t.Payee]
		if !ok {
			a = answer{top: cs[0].Name, p: 0.4}
		}
		probs := map[string]float64{a.top: a.p}
		top := []string{a.top}
		rest := (1 - a.p) / float64(len(cs)-1)
		for _, c := range cs {
			if c.Name != a.top {
				probs[c.Name] = rest
				top = append(top, c.Name)
			}
		}
		out[i] = classify.Prediction{Probs: probs, Top: top, Model: testModel}
	}
	return out, nil
}

// group is n rows the model answers with p; right says whether the label
// matches its answer. label overrides the label (a hard negative, say).
type group struct {
	n     int
	p     float64
	right bool
	label string
}

type fixture struct {
	rows  []store.LabeledRow
	model *fakeModel
	day   int
}

// add appends groups, one row per day, each with its own payee so no rule fires.
func (f *fixture) add(gs ...group) {
	if f.model == nil {
		f.model = &fakeModel{answers: map[string]answer{}}
	}
	for _, g := range gs {
		for i := 0; i < g.n; i++ {
			f.day++
			payee := fmt.Sprintf("merchant %04d", f.day)
			label, active := "Dining", true
			if !g.right {
				label = "Fuel"
			}
			if g.label != "" {
				label, active = g.label, false
			}
			f.model.answers[payee] = answer{top: "Dining", p: g.p}
			f.rows = append(f.rows, store.LabeledRow{
				Date: date(f.day), Amount: -12340, Payee: payee, Memo: fmt.Sprintf("memo %04d", f.day),
				Account: "Checking", Category: label, InActiveList: active, PlanID: "plan-secret-id",
			})
		}
	}
}

func date(day int) string {
	return time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day).Format(time.DateOnly)
}

func policy(t *testing.T) classify.Policy {
	t.Helper()
	p, err := classify.LoadPolicy([]byte(`{"version":1,"set_aside_keywords":["Christmas","Birthday"],"descriptions":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, f *fixture) *Calibration {
	t.Helper()
	// LabeledHistory returns newest first; Run must not depend on the order.
	rows := make([]store.LabeledRow, len(f.rows))
	for i, r := range f.rows {
		rows[len(rows)-1-i] = r
	}
	c, err := Run(context.Background(), rows, Options{
		Model: f.model, ModelID: testModel, Policy: policy(t), Candidates: cands, Batch: 7,
		Now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return c
}

// eligibleTune is 210 tuning rows: half right at 0.97, half wrong at 0.70. At
// τ ≤ 0.70 precision is 50%; above it, 105/105 (Wilson lower ≈ 0.965). So τ = 0.71.
func eligibleTune() []group {
	return []group{{n: 105, p: 0.97, right: true}, {n: 105, p: 0.70}}
}

func TestRunFitsTauPrecisionCoverage(t *testing.T) {
	var f fixture
	f.add(eligibleTune()...)
	// Holdout (90 rows, 30% of 300): 45 right at 0.9, 5 wrong at 0.8, 40 below τ.
	f.add(group{n: 45, p: 0.9, right: true}, group{n: 5, p: 0.8}, group{n: 40, p: 0.6, right: true})
	c := run(t, &f)

	if c.Tau == nil || *c.Tau != 0.71 {
		t.Fatalf("tau = %v, want 0.71", c.Tau)
	}
	if c.Precision != 0.9 { // 45 / 50
		t.Errorf("precision = %v, want 0.9", c.Precision)
	}
	if c.Coverage != 0.5556 { // 50 / 90
		t.Errorf("coverage = %v, want 0.5556", c.Coverage)
	}
	if c.N != 300 || c.Report.TuneRows != 210 || c.Report.HoldoutRows != 90 || c.Report.TuneModelRows != 210 {
		t.Errorf("n=%d tune=%d holdout=%d tuneModel=%d", c.N, c.Report.TuneRows, c.Report.HoldoutRows, c.Report.TuneModelRows)
	}
	if !c.AutoEligible {
		t.Error("auto_eligible = false, want true")
	}
	if c.Model != testModel || c.Version != FormatVersion || c.PolicyHash != classify.PolicyHash(policy(t)) ||
		c.CandidatesHash != classify.CandidatesHash(cands) {
		t.Errorf("identity = %q v%d %q %q", c.Model, c.Version, c.PolicyHash, c.CandidatesHash)
	}
	if c.Report.Sources[SourceModel] != 300 || c.Report.Sources[SourceRule] != 0 {
		t.Errorf("sources = %v", c.Report.Sources)
	}
	// Top-3 holds every label: the fake ranks all three candidates.
	if c.Report.Top3HitRate != 1 {
		t.Errorf("top3 = %v", c.Report.Top3HitRate)
	}
	if f.model.calls != 43 { // ceil(300 / 7)
		t.Errorf("model calls = %d, want 43 batches", f.model.calls)
	}
	if !c.Applies(testModel, c.PolicyHash, c.CandidatesHash, 0.71) || c.Applies(testModel, c.PolicyHash, c.CandidatesHash, 0.70) ||
		c.Applies("jev-other", c.PolicyHash, c.CandidatesHash, 0.99) || c.Applies(testModel, c.PolicyHash, "other", 0.99) {
		t.Error("Applies gate wrong")
	}
}

func TestRunHardNegativeAboveTauLowersPrecision(t *testing.T) {
	var base, withNeg fixture
	base.add(eligibleTune()...)
	base.add(group{n: 90, p: 0.9, right: true})
	withNeg.add(eligibleTune()...)
	withNeg.add(group{n: 80, p: 0.9, right: true}, group{n: 10, p: 0.95, label: "Groceries"})

	b, n := run(t, &base), run(t, &withNeg)
	if b.Precision != 1 {
		t.Fatalf("base precision = %v", b.Precision)
	}
	if n.Precision != 0.8889 { // 80 / 90
		t.Errorf("precision with hard negatives = %v, want 0.8889", n.Precision)
	}
	if n.Report.HardNegatives != 10 || n.Report.HardNegativesAutoApplied != 10 {
		t.Errorf("hard negatives %d, auto-applied %d", n.Report.HardNegatives, n.Report.HardNegativesAutoApplied)
	}
	// A hard negative is never a top-3 hit, though the name is in no candidate.
	if n.Report.Top3HitRate != 0.9667 { // 290 / 300
		t.Errorf("top3 = %v", n.Report.Top3HitRate)
	}
}

func TestRunDropsSetAsideLabels(t *testing.T) {
	var f fixture
	f.add(eligibleTune()...)
	f.add(group{n: 90, p: 0.9, right: true})
	// A historical Christmas label (a set-aside under the policy) the model would get "wrong".
	f.add(group{n: 5, p: 0.99, label: "Holiday - Christmas"})
	c := run(t, &f)
	if c.N != 300 || c.Report.HardNegatives != 0 {
		t.Errorf("n = %d, hard negatives = %d: set-aside labels entered the set", c.N, c.Report.HardNegatives)
	}
	for _, s := range f.model.seen {
		if s.Date.After(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 300)) {
			t.Fatalf("model saw a set-aside row dated %v", s.Date)
		}
	}
}

func TestRunNoLeakage(t *testing.T) {
	// One payee labelled every day. A rule needs 3 prior unanimous labels.
	var rows []store.LabeledRow
	for day := 1; day <= 6; day++ {
		cat := "Dining"
		if day == 5 {
			cat = "Fuel"
		}
		rows = append(rows, store.LabeledRow{Date: date(day), Payee: "corner cafe", Category: cat, InActiveList: true})
		// Same day, same payee, other label: must not be visible to its sibling.
		if day == 2 {
			rows = append(rows, store.LabeledRow{Date: date(day), Payee: "corner cafe", Category: "Discretionary", InActiveList: true})
		}
	}
	m := &fakeModel{answers: map[string]answer{}}
	c, err := Run(context.Background(), rows, Options{Model: m, Policy: policy(t), Candidates: cands})
	if err != nil {
		t.Fatal(err)
	}
	// History each model call should have seen, newest first, keyed by date.
	want := map[string]string{
		date(1): "",
		date(2): "Dining",
		date(3): "Dining,Discretionary,Dining", // day 2's two labels (input order kept on a tie), then day 1
		date(4): "Dining,Dining,Discretionary,Dining",
		date(5): "Dining,Dining,Dining,Discretionary,Dining",
		date(6): "Fuel,Dining,Dining,Dining,Discretionary",
	}
	for _, s := range m.seen {
		d := s.Date.Format(time.DateOnly)
		if got := strings.Join(s.PayeeHistory, ","); got != want[d] {
			t.Errorf("%s: history %q, want %q", d, got, want[d])
		}
		if s.ID != "" {
			t.Errorf("model saw an id %q", s.ID)
		}
	}
	// Day 2's labels disagree, so no rule ever fires.
	if c.Report.Sources[SourceRule] != 0 || len(m.seen) != 7 {
		t.Errorf("rules=%d model=%d", c.Report.Sources[SourceRule], len(m.seen))
	}
}

func TestRunRulesSeeOnlyEarlierLabels(t *testing.T) {
	var rows []store.LabeledRow
	for day := 1; day <= 5; day++ {
		rows = append(rows, store.LabeledRow{Date: date(day), Payee: "shell", Category: "Fuel", InActiveList: true})
	}
	// A same-day sibling on day 3 can't make day 3 a rule.
	rows = append(rows, store.LabeledRow{Date: date(3), Payee: "shell", Category: "Fuel", InActiveList: true})
	m := &fakeModel{answers: map[string]answer{}}
	c, err := Run(context.Background(), rows, Options{Model: m, Policy: policy(t), Candidates: cands})
	if err != nil {
		t.Fatal(err)
	}
	// Days 1-3 (4 rows) reach the model; days 4-5 have ≥ 3 earlier labels.
	if c.Report.Sources[SourceModel] != 4 || c.Report.Sources[SourceRule] != 2 {
		t.Errorf("sources = %v", c.Report.Sources)
	}
	if c.Report.RuleApplied != 2 || c.Report.RuleCorrect != 2 || c.Report.RulePrecision != 1 {
		t.Errorf("rule %d/%d %v", c.Report.RuleCorrect, c.Report.RuleApplied, c.Report.RulePrecision)
	}
}

func TestRunGuardrailsSkipP2P(t *testing.T) {
	rows := []store.LabeledRow{
		{Date: date(1), Payee: "venmo", Category: "Dining", InActiveList: true},
		{Date: date(2), Payee: "cafe", Category: "Dining", InActiveList: true},
	}
	m := &fakeModel{answers: map[string]answer{}}
	c, err := Run(context.Background(), rows, Options{Model: m, Policy: policy(t), Candidates: cands})
	if err != nil {
		t.Fatal(err)
	}
	if c.Report.Sources[SourceGuard] != 1 || c.Report.GuardSkips[string(classify.SkipP2P)] != 1 || len(m.seen) != 1 {
		t.Errorf("sources %v skips %v seen %d", c.Report.Sources, c.Report.GuardSkips, len(m.seen))
	}
}

func TestRunNotEligibleUnder200TuneRows(t *testing.T) {
	var f fixture
	// 140 tune rows (70% of 200), τ still fits; holdout perfect.
	f.add(group{n: 70, p: 0.97, right: true}, group{n: 70, p: 0.70})
	f.add(group{n: 60, p: 0.9, right: true})
	c := run(t, &f)
	if c.Tau == nil || *c.Tau != 0.71 || c.Precision != 1 {
		t.Fatalf("tau %v precision %v", c.Tau, c.Precision)
	}
	if c.AutoEligible {
		t.Error("auto_eligible with 140 tuning rows")
	}
}

func TestRunNotEligibleHoldoutUnderTarget(t *testing.T) {
	var f fixture
	f.add(eligibleTune()...)
	f.add(group{n: 40, p: 0.9, right: true}, group{n: 10, p: 0.9}, group{n: 40, p: 0.5, right: true})
	c := run(t, &f)
	if c.Precision != 0.8 {
		t.Fatalf("precision = %v, want 0.8", c.Precision)
	}
	if c.AutoEligible {
		t.Error("auto_eligible with holdout precision 0.8")
	}
}

func TestRunNoTau(t *testing.T) {
	var f fixture
	f.add(group{n: 105, p: 0.97, right: true}, group{n: 105, p: 0.97}, group{n: 90, p: 0.97, right: true})
	c := run(t, &f)
	if c.Tau != nil || c.AutoEligible || c.Precision != 0 || c.Coverage != 0 {
		t.Errorf("tau %v eligible %v precision %v coverage %v", c.Tau, c.AutoEligible, c.Precision, c.Coverage)
	}
	if c.Applies(testModel, c.PolicyHash, c.CandidatesHash, 1) {
		t.Error("Applies with no τ")
	}
}

func TestRunLimitKeepsOlderHistory(t *testing.T) {
	var rows []store.LabeledRow
	for day := 1; day <= 4; day++ {
		rows = append(rows, store.LabeledRow{Date: date(day), Payee: "shell", Category: "Fuel", InActiveList: true})
	}
	m := &fakeModel{answers: map[string]answer{}}
	c, err := Run(context.Background(), rows, Options{Model: m, Policy: policy(t), Candidates: cands, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if c.N != 1 || c.Report.Sources[SourceRule] != 1 || len(m.seen) != 0 {
		t.Errorf("n %d sources %v seen %d", c.N, c.Report.Sources, len(m.seen))
	}
}

func TestRunModelErrorFails(t *testing.T) {
	m := &fakeModel{err: errors.New("boom")}
	rows := []store.LabeledRow{{Date: date(1), Payee: "cafe", Category: "Dining", InActiveList: true}}
	if _, err := Run(context.Background(), rows, Options{Model: m, Policy: policy(t), Candidates: cands}); err == nil {
		t.Fatal("want error")
	}
}

func TestOutputHasNoIdsPayeesOrMemos(t *testing.T) {
	var f fixture
	f.add(eligibleTune()...)
	f.add(group{n: 80, p: 0.9, right: true}, group{n: 10, p: 0.95, label: "Groceries"})
	b, err := Marshal(run(t, &f))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, bad := range []string{"plan-secret-id", "merchant", "memo 0", "Checking", "Groceries", "-12340", "12.34"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q", bad)
		}
	}
	if !strings.Contains(out, `"tau": 0.71`) || !strings.Contains(out, `"auto_eligible": true`) {
		t.Errorf("output missing fields:\n%s", out)
	}
	if _, err := Parse(b); err != nil {
		t.Errorf("round trip: %v", err)
	}
}

func TestWilsonLower(t *testing.T) {
	for _, tc := range []struct {
		k, n int
		want float64
	}{{0, 0, 0}, {9, 9, 0.7008}, {22, 22, 0.8513}, {21, 21, 0.8454}, {90, 100, 0.8256}} {
		if got := round4(wilsonLower(tc.k, tc.n)); got != tc.want {
			t.Errorf("wilsonLower(%d,%d) = %v, want %v", tc.k, tc.n, got, tc.want)
		}
	}
}

func TestCurrentPlaceholder(t *testing.T) {
	c := Current()
	if c.Version != FormatVersion || c.AutoEligible || c.Tau != nil {
		t.Errorf("placeholder = %+v", c)
	}
	var raw map[string]any
	if err := json.Unmarshal(calibrationJSON, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"model", "policy_hash", "candidates_hash", "tau", "precision", "coverage", "n", "auto_eligible", "created_at"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("placeholder lacks %q", k)
		}
	}
}
