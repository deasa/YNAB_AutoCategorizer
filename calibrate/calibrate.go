// Package calibrate measures the rules + model stack on labelled history and
// fits τ, the p_top threshold above which the model's answer is auto-applied
// (architecture §3.3, R6). The result is checked in as calibration.json and
// embedded; Current returns it.
package calibrate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	// FormatVersion is calibration.json's schema version.
	FormatVersion = 1
	// TargetPrecision is R6's bar.
	TargetPrecision = 0.85
	// MinTuneModelRows is how many model-band rows the tuning set needs before
	// τ can be trusted for auto-apply.
	MinTuneModelRows = 200
	// TuneFraction is the oldest share of rows that tunes τ; the rest is holdout.
	TuneFraction = 0.70
	// DefaultBatch is how many transactions go to the Model per call. The Model
	// may split further (Jev sends 10 per request).
	DefaultBatch = 50

	tauGridMin, tauGridMax = 50, 99 // τ grid in hundredths
	wilsonZ                = 1.96   // 95%
)

// Source is the layer of the stack that handled a row.
type Source string

const (
	SourceGuard Source = "guard" // a guardrail kept it from the classifier
	SourceRule  Source = "rule"  // a unanimous payee rule decided it
	SourceModel Source = "model" // it reached the model
)

// Options configure Run.
type Options struct {
	Model      classify.Model
	ModelID    string // the id expected from Model; a prediction naming another fails the run
	Policy     classify.Policy
	Candidates []classify.Candidate
	// Limit evaluates only the newest Limit rows; older rows still serve as
	// history. 0 means all.
	Limit int
	Batch int              // transactions per Model call; 0 means DefaultBatch
	Now   func() time.Time // defaults to time.Now
}

// Calibration is calibration.json. It holds names and aggregates only: no ids,
// payees or memos.
type Calibration struct {
	Version        int    `json:"version"`
	Model          string `json:"model"`
	PolicyHash     string `json:"policy_hash"`
	CandidatesHash string `json:"candidates_hash"`
	// Tau is nil when no grid value meets the precision bar on the tuning set.
	Tau *float64 `json:"tau"`
	// Precision and Coverage are the holdout's model band at τ: the share of
	// auto-applied rows that were right, and the share of model-band rows
	// auto-applied.
	Precision    float64   `json:"precision"`
	Coverage     float64   `json:"coverage"`
	N            int       `json:"n"` // labelled rows evaluated
	AutoEligible bool      `json:"auto_eligible"`
	CreatedAt    time.Time `json:"created_at"`
	Report       *Report   `json:"report,omitempty"`
}

// Report is the detail behind the headline numbers.
type Report struct {
	TuneRows         int            `json:"tune_rows"`
	HoldoutRows      int            `json:"holdout_rows"`
	TuneModelRows    int            `json:"tune_model_rows"`
	HoldoutModelRows int            `json:"holdout_model_rows"`
	Sources          map[Source]int `json:"sources"`               // rows per layer, all evaluated rows
	GuardSkips       map[string]int `json:"guard_skips,omitempty"` // by classify.Skip
	RuleApplied      int            `json:"rule_applied"`
	RuleCorrect      int            `json:"rule_correct"`
	RulePrecision    float64        `json:"rule_precision"`
	TuneApplied      int            `json:"tune_applied"` // model band at τ
	TuneCorrect      int            `json:"tune_correct"`
	TuneWilsonLower  float64        `json:"tune_wilson_lower"` // of tune_correct/tune_applied
	HoldoutApplied   int            `json:"holdout_applied"`   // model band at τ
	HoldoutCorrect   int            `json:"holdout_correct"`
	// Top3HitRate is over every model-band row: the label is in the model's top 3.
	Top3HitRate float64 `json:"top3_hit_rate"`
	// HardNegatives are rows whose label isn't a candidate; the auto-applied
	// ones were applied by a rule or by the model at τ, and are all wrong.
	HardNegatives            int            `json:"hard_negatives"`
	HardNegativesAutoApplied int            `json:"hard_negatives_auto_applied"`
	PerCategory              []CategoryStat `json:"per_category"`
}

// CategoryStat is the stack's precision for one predicted category over every
// row it auto-applied (rules, and the model at τ).
type CategoryStat struct {
	Category  string  `json:"category"`
	Applied   int     `json:"applied"`
	Correct   int     `json:"correct"`
	Precision float64 `json:"precision"`
}

// row is one evaluated labelled transaction.
type row struct {
	label    store.LabeledRow
	source   Source
	skip     classify.Skip
	predict  string // the rule's or the model's top answer
	pTop     float64
	top      []string
	modelIdx int // index into the model queue
}

// correct reports whether answer matches the human label. A label outside the
// candidates (a hard negative) has no correct answer.
func (r row) correct(answer string) bool {
	return r.label.InActiveList && strings.EqualFold(answer, r.label.Category)
}

// Run measures the stack on history (store.LabeledHistory with the candidate
// names as activeCategoryNames and the set-asides as excludedNames) and fits τ.
// Labels that are set-asides under the policy are dropped: spending they
// tracked is Discretionary now, so no answer for them is fairly right or wrong.
func Run(ctx context.Context, history []store.LabeledRow, opt Options) (*Calibration, error) {
	if opt.Model == nil {
		return nil, errors.New("calibrate: no model")
	}
	if len(opt.Candidates) == 0 {
		return nil, errors.New("calibrate: no candidates")
	}
	batch := opt.Batch
	if batch <= 0 {
		batch = DefaultBatch
	}
	now := time.Now
	if opt.Now != nil {
		now = opt.Now
	}

	all := make([]store.LabeledRow, 0, len(history))
	for _, h := range history {
		if !opt.Policy.IsSetAside(h.Category) {
			all = append(all, h)
		}
	}
	// Oldest first; the input's order is kept within a date.
	sort.SliceStable(all, func(i, j int) bool { return all[i].Date < all[j].Date })

	start := 0
	if opt.Limit > 0 && opt.Limit < len(all) {
		start = len(all) - opt.Limit
	}
	rows, txns := route(all, start, opt.Candidates)

	preds, err := classifyAll(ctx, opt.Model, txns, opt.Candidates, batch)
	if err != nil {
		return nil, err
	}
	modelID := opt.ModelID
	for i := range rows {
		r := &rows[i]
		if r.source != SourceModel {
			continue
		}
		p := preds[r.modelIdx]
		if len(p.Top) == 0 {
			return nil, errors.New("calibrate: model returned an empty ranking")
		}
		if modelID == "" {
			modelID = p.Model
		}
		if p.Model != modelID {
			return nil, fmt.Errorf("calibrate: model %q answered, expected %q", p.Model, modelID)
		}
		r.predict, r.pTop, r.top = p.Top[0], p.Probs[p.Top[0]], p.Top
	}

	c := &Calibration{
		Version:        FormatVersion,
		Model:          modelID,
		PolicyHash:     classify.PolicyHash(opt.Policy),
		CandidatesHash: classify.CandidatesHash(opt.Candidates),
		N:              len(rows),
		CreatedAt:      now().UTC().Truncate(time.Second),
	}
	c.Report = measure(c, rows)
	return c, nil
}

// route sends each row from start on through the guardrails and the rules,
// each seeing only labels dated strictly before it, and queues the rest for
// the model. all is oldest first.
func route(all []store.LabeledRow, start int, cands []classify.Candidate) ([]row, []classify.Txn) {
	var (
		rows   []row
		txns   []classify.Txn
		rules  *classify.Rules
		ruleAt = ""
		before = 0 // all[:before] is dated before the current row
	)
	for i := start; i < len(all); i++ {
		l := all[i]
		if rules == nil || l.Date != ruleAt {
			for before < len(all) && all[before].Date < l.Date {
				before++
			}
			rules = classify.NewRules(all[:before:before], cands)
			ruleAt = l.Date
		}
		r := row{label: l}
		d := detail(l)
		switch skip := classify.Guard(classify.Row{Detail: d}); {
		case skip != classify.SkipNone:
			r.source, r.skip = SourceGuard, skip
		default:
			if name, ok := rules.Decide(classify.PayeeName(d)); ok {
				r.source, r.predict = SourceRule, name
			} else {
				r.source, r.modelIdx = SourceModel, len(txns)
				txns = append(txns, rules.NewTxn(d))
			}
		}
		rows = append(rows, r)
	}
	return rows, txns
}

// detail is the mirror row the runtime would classify for a labelled row:
// approved, uncategorized (the label is the answer, so the model never sees
// it), and with no id.
func detail(l store.LabeledRow) store.TransactionDetail {
	d := store.TransactionDetail{}
	d.Date = l.Date
	d.Amount = l.Amount
	d.Approved = true
	if l.Memo != "" {
		memo := l.Memo
		d.Memo = &memo
	}
	if l.Payee != "" {
		payee := l.Payee
		d.PayeeName = &payee
	}
	if l.Account != "" {
		account := l.Account
		d.AccountName = &account
	}
	return d
}

func classifyAll(ctx context.Context, m classify.Model, txns []classify.Txn, cands []classify.Candidate, batch int) ([]classify.Prediction, error) {
	out := make([]classify.Prediction, 0, len(txns))
	for lo := 0; lo < len(txns); lo += batch {
		hi := min(lo+batch, len(txns))
		preds, err := m.Classify(ctx, txns[lo:hi], cands)
		if err != nil {
			return nil, fmt.Errorf("calibrate: model: %w", err)
		}
		if len(preds) != hi-lo {
			return nil, fmt.Errorf("calibrate: model returned %d predictions for %d transactions", len(preds), hi-lo)
		}
		out = append(out, preds...)
	}
	return out, nil
}

// split returns the index where the holdout starts: after the oldest
// TuneFraction of rows, moved forward so no date straddles the cut.
func split(rows []row) int {
	cut := int(float64(len(rows)) * TuneFraction)
	for cut > 0 && cut < len(rows) && rows[cut].label.Date == rows[cut-1].label.Date {
		cut++
	}
	return cut
}

// fitTau returns the smallest grid τ whose model-band Wilson lower bound on
// precision is at least TargetPrecision.
func fitTau(rows []row) (float64, bool) {
	for g := tauGridMin; g <= tauGridMax; g++ {
		tau := float64(g) / 100
		applied, correct := modelAt(rows, tau)
		if applied > 0 && wilsonLower(correct, applied) >= TargetPrecision {
			return tau, true
		}
	}
	return 0, false
}

// modelAt counts model-band rows with p_top ≥ τ and how many were right.
func modelAt(rows []row, tau float64) (applied, correct int) {
	for _, r := range rows {
		if r.source == SourceModel && r.pTop >= tau {
			applied++
			if r.correct(r.predict) {
				correct++
			}
		}
	}
	return applied, correct
}

func measure(c *Calibration, rows []row) *Report {
	cut := split(rows)
	tune, hold := rows[:cut], rows[cut:]
	rep := &Report{
		TuneRows: len(tune), HoldoutRows: len(hold),
		TuneModelRows: countSource(tune, SourceModel), HoldoutModelRows: countSource(hold, SourceModel),
		Sources: map[Source]int{SourceGuard: 0, SourceRule: 0, SourceModel: 0},
	}

	tau, found := fitTau(tune)
	if found {
		c.Tau = &tau
		rep.TuneApplied, rep.TuneCorrect = modelAt(tune, tau)
		rep.TuneWilsonLower = round4(wilsonLower(rep.TuneCorrect, rep.TuneApplied))
		rep.HoldoutApplied, rep.HoldoutCorrect = modelAt(hold, tau)
		c.Precision = ratio(rep.HoldoutCorrect, rep.HoldoutApplied)
		c.Coverage = ratio(rep.HoldoutApplied, rep.HoldoutModelRows)
	}
	c.AutoEligible = found && rep.TuneModelRows >= MinTuneModelRows &&
		rep.HoldoutApplied > 0 && float64(rep.HoldoutCorrect) >= TargetPrecision*float64(rep.HoldoutApplied)

	auto := func(r row) bool {
		return r.source == SourceRule || (r.source == SourceModel && found && r.pTop >= tau)
	}
	perCat := map[string]*CategoryStat{}
	var modelRows, top3 int
	for _, r := range rows {
		rep.Sources[r.source]++
		if r.source == SourceGuard {
			if rep.GuardSkips == nil {
				rep.GuardSkips = map[string]int{}
			}
			rep.GuardSkips[string(r.skip)]++
		}
		if r.source == SourceRule {
			rep.RuleApplied++
			if r.correct(r.predict) {
				rep.RuleCorrect++
			}
		}
		if r.source == SourceModel {
			modelRows++
			for _, t := range r.top[:min(3, len(r.top))] {
				if r.correct(t) {
					top3++
					break
				}
			}
		}
		if !r.label.InActiveList {
			rep.HardNegatives++
			if auto(r) {
				rep.HardNegativesAutoApplied++
			}
		}
		if auto(r) {
			s := perCat[r.predict]
			if s == nil {
				s = &CategoryStat{Category: r.predict}
				perCat[r.predict] = s
			}
			s.Applied++
			if r.correct(r.predict) {
				s.Correct++
			}
		}
	}
	rep.RulePrecision = ratio(rep.RuleCorrect, rep.RuleApplied)
	rep.Top3HitRate = ratio(top3, modelRows)
	for _, s := range perCat {
		s.Precision = ratio(s.Correct, s.Applied)
		rep.PerCategory = append(rep.PerCategory, *s)
	}
	sort.Slice(rep.PerCategory, func(i, j int) bool { return rep.PerCategory[i].Category < rep.PerCategory[j].Category })
	return rep
}

func countSource(rows []row, s Source) int {
	n := 0
	for _, r := range rows {
		if r.source == s {
			n++
		}
	}
	return n
}

// wilsonLower is the lower end of the Wilson 95% interval for k of n.
func wilsonLower(k, n int) float64 {
	if n == 0 {
		return 0
	}
	p, nf, z2 := float64(k)/float64(n), float64(n), wilsonZ*wilsonZ
	centre := p + z2/(2*nf)
	margin := wilsonZ * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	return (centre - margin) / (1 + z2/nf)
}

// ratio is k/n rounded to 4 places, 0 when n is 0.
func ratio(k, n int) float64 {
	if n == 0 {
		return 0
	}
	return round4(float64(k) / float64(n))
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }
