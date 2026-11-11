package calibrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// The dry-run report (`finbot dry-run-report`): how the dry run did, and how
// sure that number is, from the local DB alone. It reads decisions, questions,
// write_log and the mirror, and prints category names and counts only.

// ThinSample is the judged count below which the report warns.
const ThinSample = 30

// ReportStore is what the report reads (*store.DB).
type ReportStore interface {
	ReportPlans(ctx context.Context) ([]string, error)
	DecisionsBetween(ctx context.Context, planID string, from, to time.Time) ([]store.Decision, error)
	AnsweredQuestions(ctx context.Context, planID, kind string, from, to time.Time) ([]store.Question, error)
	CurrentHumanCategory(ctx context.Context, planID, txnID string) (store.HumanCategory, bool, error)
	CountQuestions(ctx context.Context, planID string) (open, pending int, err error)
	CountWriteLog(ctx context.Context, planID string, from, to time.Time) (int, error)
}

// DryRunReport is the dry-run report's numbers.
type DryRunReport struct {
	Decisions   int
	First, Last time.Time // created_at of the first and last decision; zero if none
	ByMethod    map[string]int
	ByOutcome   map[string]int

	Judged int // would_apply and asked decisions with a label later than the decision

	// The would_apply band: judged decisions, how many were right, and the
	// Wilson 95% interval on that precision.
	WouldApplyJudged, WouldApplyCorrect int
	PrecisionLow, PrecisionHigh         float64

	// Asked rows: judged decisions, and how many had the label in their top-3.
	AskedJudged, AskedTop3 int

	// Misses counts wrong would_apply decisions by "predicted → label" names.
	Misses map[string]int

	OpenQuestions, PendingQuestions int
	ModelErrors                     int // decisions whose second opinion failed
	Writes                          int // write_log rows, any status
}

// label is the category a person gave a transaction, and when it was first seen.
type label struct {
	id, name string // name is "" when unknown
	split    bool
	at       time.Time
}

var (
	reportFrom = time.Time{}
	reportTo   = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
)

// BuildDryRunReport reads every plan that has a decision, question or write.
func BuildDryRunReport(ctx context.Context, st ReportStore) (DryRunReport, error) {
	r := DryRunReport{ByMethod: map[string]int{}, ByOutcome: map[string]int{}, Misses: map[string]int{}}
	plans, err := st.ReportPlans(ctx)
	if err != nil {
		return DryRunReport{}, err
	}
	for _, plan := range plans {
		if err := r.addPlan(ctx, st, plan); err != nil {
			return DryRunReport{}, err
		}
	}
	r.PrecisionLow, r.PrecisionHigh = wilsonInterval(r.WouldApplyCorrect, r.WouldApplyJudged)
	return r, nil
}

func (r *DryRunReport) addPlan(ctx context.Context, st ReportStore, plan string) error {
	open, pending, err := st.CountQuestions(ctx, plan)
	if err != nil {
		return err
	}
	r.OpenQuestions += open
	r.PendingQuestions += pending
	writes, err := st.CountWriteLog(ctx, plan, reportFrom, reportTo)
	if err != nil {
		return err
	}
	r.Writes += writes

	decs, err := st.DecisionsBetween(ctx, plan, reportFrom, reportTo)
	if err != nil {
		return err
	}
	answered, err := st.AnsweredQuestions(ctx, plan, store.QuestionDryRunCheck, reportFrom, reportTo)
	if err != nil {
		return err
	}
	answers := map[string][]label{} // by txn, in answer order
	for _, q := range answered {
		if q.Answer == nil || q.AnsweredAt == nil {
			continue
		}
		l := label{id: *q.Answer, at: *q.AnsweredAt}
		for _, o := range q.Options {
			if o.CategoryID == l.id {
				l.name = o.CategoryName
				break
			}
		}
		answers[q.SubjectID] = append(answers[q.SubjectID], l)
	}
	mirror := map[string]*label{} // by txn; nil when there's no human label
	for _, d := range decs {
		r.Decisions++
		if r.First.IsZero() || d.CreatedAt.Before(r.First) {
			r.First = d.CreatedAt
		}
		if d.CreatedAt.After(r.Last) {
			r.Last = d.CreatedAt
		}
		r.ByMethod[d.Method]++
		r.ByOutcome[d.Outcome]++
		if secondOpinionFailed(d.SecondOpinion) {
			r.ModelErrors++
		}
		if d.Outcome != store.OutcomeWouldApply && d.Outcome != store.OutcomeAsked {
			continue
		}
		m, seen := mirror[d.TxnID]
		if !seen {
			if m, err = mirrorLabel(ctx, st, plan, d.TxnID); err != nil {
				return err
			}
			mirror[d.TxnID] = m
		}
		l, ok := judge(d.CreatedAt, answers[d.TxnID], m)
		if !ok {
			continue
		}
		r.Judged++
		if d.Outcome == store.OutcomeWouldApply {
			r.WouldApplyJudged++
			if matches(d, l) {
				r.WouldApplyCorrect++
			} else {
				r.Misses[missKey(d, l)]++
			}
			continue
		}
		r.AskedJudged++
		if inTop3(d, l) {
			r.AskedTop3++
		}
	}
	return nil
}

// mirrorLabel is the transaction's human-set category in the mirror, if any.
func mirrorLabel(ctx context.Context, st ReportStore, plan, txn string) (*label, error) {
	h, ok, err := st.CurrentHumanCategory(ctx, plan, txn)
	if err != nil || !ok || !h.HumanSet || h.Deleted {
		return nil, err
	}
	l := &label{split: h.IsSplit, at: h.SeenAt}
	if h.CategoryID != nil {
		l.id = *h.CategoryID
	}
	if h.CategoryName != nil {
		l.name = *h.CategoryName
	}
	return l, nil
}

// judge returns the first label later than the decision: a dry_run_check
// answer or the mirror's human-set category, whichever came first.
func judge(at time.Time, answers []label, mirror *label) (label, bool) {
	var best label
	found := false
	consider := func(l label) {
		if l.at.After(at) && (!found || l.at.Before(best.at)) {
			best, found = l, true
		}
	}
	for _, a := range answers {
		consider(a)
	}
	if mirror != nil {
		consider(*mirror)
	}
	return best, found
}

// matches reports whether the decision's category is the label. A split label
// matches no single category.
func matches(d store.Decision, l label) bool {
	if l.split {
		return false
	}
	if d.CategoryID != nil && l.id != "" && *d.CategoryID == l.id {
		return true
	}
	return d.CategoryName != nil && l.name != "" && strings.EqualFold(*d.CategoryName, l.name)
}

// inTop3 reports whether the label is among the decision's top three.
func inTop3(d store.Decision, l label) bool {
	if matches(d, l) {
		return true
	}
	if l.split || l.name == "" {
		return false
	}
	var f struct {
		Top []string `json:"top"`
	}
	_ = json.Unmarshal(d.Features, &f) // a decision without a top list has only its own category
	top := f.Top
	if len(top) == 0 && d.CategoryName != nil {
		top = []string{*d.CategoryName}
		if d.RunnerUp != nil {
			top = append(top, *d.RunnerUp)
		}
	}
	for i, n := range top {
		if i == 3 {
			break
		}
		if strings.EqualFold(n, l.name) {
			return true
		}
	}
	return false
}

func missKey(d store.Decision, l label) string {
	pred, got := "(none)", "(not in the mirror)"
	if d.CategoryName != nil {
		pred = *d.CategoryName
	}
	switch {
	case l.split:
		got = "(split)"
	case l.name != "":
		got = l.name
	}
	return pred + " → " + got
}

func secondOpinionFailed(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var so struct {
		Failed bool `json:"failed"`
	}
	return json.Unmarshal(raw, &so) == nil && so.Failed
}

// wilsonInterval is the Wilson score interval at 95% for k of n; 0, 0 when n is 0.
func wilsonInterval(k, n int) (low, high float64) {
	if n == 0 {
		return 0, 0
	}
	p, nf, z2 := float64(k)/float64(n), float64(n), wilsonZ*wilsonZ
	centre := p + z2/(2*nf)
	margin := wilsonZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	return (centre - margin) / (1 + z2/nf), (centre + margin) / (1 + z2/nf)
}

// Write prints the report, dates in loc. The last line is exactly
// "writes: <n>".
func (r DryRunReport) Write(w io.Writer, loc *time.Location) error {
	var b strings.Builder
	pf := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	date := func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.In(loc).Format(time.DateOnly)
	}
	pf("decisions: %d (first %s, last %s)", r.Decisions, date(r.First), date(r.Last))
	pf("by method: %s", counts(r.ByMethod, store.MethodRule, store.MethodModel))
	pf("by outcome: %s", counts(r.ByOutcome, store.OutcomeWouldApply, store.OutcomeAsked, store.OutcomeSkipped,
		store.OutcomeApplied))
	pf("would_apply: %d", r.ByOutcome[store.OutcomeWouldApply])
	pf("judged: %d (would_apply %d, asked %d)", r.Judged, r.WouldApplyJudged, r.AskedJudged)
	if r.WouldApplyJudged == 0 {
		pf("precision (would_apply): no judged decisions")
	} else {
		pf("precision (would_apply): %d/%d = %s (Wilson 95%%: %s to %s; target %s)", r.WouldApplyCorrect,
			r.WouldApplyJudged, pct(float64(r.WouldApplyCorrect)/float64(r.WouldApplyJudged)), pct(r.PrecisionLow),
			pct(r.PrecisionHigh), pct(TargetPrecision))
	}
	if r.AskedJudged == 0 {
		pf("top-3 hit rate (asked): no judged decisions")
	} else {
		pf("top-3 hit rate (asked): %d/%d = %s", r.AskedTop3, r.AskedJudged,
			pct(float64(r.AskedTop3)/float64(r.AskedJudged)))
	}
	if len(r.Misses) > 0 {
		keys := make([]string, 0, len(r.Misses))
		for k := range r.Misses {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if r.Misses[keys[i]] != r.Misses[keys[j]] {
				return r.Misses[keys[i]] > r.Misses[keys[j]]
			}
			return keys[i] < keys[j]
		})
		pf("would_apply misses (predicted → label):")
		for _, k := range keys {
			pf("  %s: %d", k, r.Misses[k])
		}
	}
	pf("questions: %d open, %d pending", r.OpenQuestions, r.PendingQuestions)
	pf("model errors: %d (failed second opinions; a failed primary call leaves no decision)", r.ModelErrors)
	if r.Judged < ThinSample {
		pf("judged < %d: thin sample", ThinSample)
	} else if r.WouldApplyJudged < ThinSample {
		pf("would_apply judged < %d: thin sample", ThinSample)
	}
	pf("writes: %d", r.Writes)
	_, err := io.WriteString(w, b.String())
	return err
}

// counts renders m as "k n, ..." with the known keys first, then any others.
func counts(m map[string]int, known ...string) string {
	var parts []string
	seen := map[string]bool{}
	for _, k := range known {
		seen[k] = true
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	var rest []string
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func pct(f float64) string { return fmt.Sprintf("%.1f%%", 100*f) }
