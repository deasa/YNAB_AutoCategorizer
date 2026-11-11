package classify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
)

// Calibration is what Run needs from calibrate.Current(): the measured τ and
// the model, policy and candidate list it was measured on. cmd/ copies it in,
// since calibrate imports classify.
type Calibration struct {
	Model          string
	PolicyHash     string
	CandidatesHash string
	Tau            *float64 // nil when none was measured (the placeholder)
	AutoEligible   bool
}

// Defaults for Config's zero values.
const (
	DefaultTau         = 0.8 // dry-run τ while the calibration is the placeholder
	DefaultBacklogDays = 45
	DefaultMaxPerRun   = 50
	DefaultQuestionCap = 5
	DefaultRunTimeout  = 10 * time.Minute
)

// Close reasons Run writes.
const (
	ReasonSuperseded       = "superseded"
	ReasonSupersededByAuto = "superseded_by_auto"
)

// Skip reasons beyond the guardrails, logged in a decision's features.
const SkipBeforeAutoSince Skip = "before_auto_since"

// Applier writes categorizations to YNAB; *writer.Writer meets it.
type Applier interface {
	Apply(ctx context.Context, batch writer.Batch, actor, via string) ([]writer.Outcome, error)
}

// Asker posts questions to people. Ask posts pending question id and marks it
// open. Closed edits a posted question's message after Run closed it with
// reason; it is never called for a question that was never posted.
type Asker interface {
	Ask(ctx context.Context, id int64) error
	Closed(ctx context.Context, q store.Question, reason string) error
}

// Store is what Run reads and writes; *store.DB meets it.
type Store interface {
	EligibleTransactions(ctx context.Context, planID, since string, limit int) ([]store.TransactionDetail, error)
	CategoriesWithGroups(ctx context.Context, planID string) ([]store.CategoryWithGroup, error)
	Accounts(ctx context.Context, planID string) ([]store.AccountSummary, error)
	Locks(ctx context.Context, planID string, txnIDs []string) (map[string][]store.HumanLock, error)
	LabeledHistory(ctx context.Context, planIDs, activeCategoryNames, excludedNames []string) ([]store.LabeledRow, error)
	HasCurrentDecision(ctx context.Context, planID, txnID string, key store.DecisionKey) (bool, error)
	InsertDecision(ctx context.Context, dec store.Decision) (int64, error)
	InsertQuestion(ctx context.Context, q store.Question) (int64, error)
	CloseQuestion(ctx context.Context, id int64, reason string) (bool, error)
	QuestionsBySubject(ctx context.Context, planID, subjectID string) ([]store.Question, error)
	OpenQuestions(ctx context.Context, planID string) ([]store.Question, error)
	PendingQuestions(ctx context.Context, planID string) ([]store.Question, error)
}

// Config sets up a Pipeline. Zero numbers take the Default* values.
type Config struct {
	PlanID string
	// HistoryPlanIDs are the plans whose human labels feed the rules and payee
	// history. Empty means PlanID alone.
	HistoryPlanIDs []string
	// Mode is the configured mode. auto is effective only with a matching,
	// auto-eligible Calibration; anything else but off runs as dry_run.
	Mode   writer.Mode
	Policy Policy
	// ModelID is Model's pinned id. Decisions are keyed by it, and auto needs it
	// to equal the calibration's model and every Prediction.Model.
	ModelID string
	Model   Model
	// SecondOpinion is advisory: asked on the τ-miss band, logged and shown as
	// a hint, never applied. Optional.
	SecondOpinion Model
	// SecondOpinionModelID, when set, is recorded instead of the
	// Prediction.Model the second opinion reports.
	SecondOpinionModelID string
	Store                Store
	Applier              Applier
	Asker                Asker
	// SetMode publishes the effective mode (writer.Gate.Set) at the start of
	// every Run, before anything can apply. Required.
	SetMode func(writer.Mode)
	// AutoSince ("YYYY-MM-DD"): in effective auto, an older transaction is
	// never written or asked about. Empty means no bound here.
	AutoSince   string
	BacklogDays int
	MaxPerRun   int
	QuestionCap int
	RunTimeout  time.Duration
	Now         func() time.Time
	Log         *slog.Logger
}

// Pipeline categorizes eligible transactions once per sync.
type Pipeline struct{ cfg Config }

// New returns a Pipeline. It panics if a required dependency is missing.
func New(cfg Config) *Pipeline {
	if cfg.Store == nil || cfg.Model == nil || cfg.Applier == nil || cfg.Asker == nil || cfg.SetMode == nil {
		panic("classify: New needs Store, Model, Applier, Asker and SetMode")
	}
	if len(cfg.HistoryPlanIDs) == 0 {
		cfg.HistoryPlanIDs = []string{cfg.PlanID}
	}
	if cfg.BacklogDays <= 0 {
		cfg.BacklogDays = DefaultBacklogDays
	}
	if cfg.MaxPerRun <= 0 {
		cfg.MaxPerRun = DefaultMaxPerRun
	}
	if cfg.QuestionCap <= 0 {
		cfg.QuestionCap = DefaultQuestionCap
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = DefaultRunTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Pipeline{cfg: cfg}
}

// Result counts what one Run did. Warnings are for the status message.
type Result struct {
	Mode     writer.Mode
	Warnings []string

	Eligible        int // transactions read
	AlreadyDecided  int // had a current decision, not re-decided
	Guarded         int // skipped by a guardrail
	BeforeAutoSince int // effective auto, dated before AutoSince
	Rule            int // decided by a payee rule
	TauPass         int // model p_top ≥ τ
	TauMiss         int // model p_top < τ
	ModelFailures   int // no decision; retried next run
	SecondOpinions  int // second opinions obtained

	Applied      int // written (or left for Reconcile) by the Applier
	WouldApply   int // dry-run apply-path decisions
	ApplySkipped int // the writer declined; logged as skipped
	ApplyFailed  int // no decision; retried next run
	Asked        int // questions created

	Superseded       int // live questions replaced by a newer one
	SupersededByAuto int // dry_run_check questions closed on entering auto
	Posted           int // questions posted this run
	AskFailures      int // posts that failed; still pending
	Pending          int // questions waiting for a slot after this run
}

// features is the model input and context logged with a decision.
type features struct {
	Payee       string   `json:"payee"`
	Memo        string   `json:"memo,omitempty"`
	Account     string   `json:"account,omitempty"`
	Date        string   `json:"date"`
	AmountMilli int64    `json:"amount_milli"`
	History     []string `json:"history,omitempty"`
	Skip        Skip     `json:"skip,omitempty"`
	Tau         *float64 `json:"tau,omitempty"`
	Top         []string `json:"top,omitempty"`
	PredModel   string   `json:"pred_model,omitempty"` // the Prediction's own id, if it differs from ModelID
	WriteStatus string   `json:"write_status,omitempty"`
	WriteReason string   `json:"write_reason,omitempty"`
}

// secondOpinion is a decision's second_opinion column.
type secondOpinion struct {
	Model    string  `json:"model"`
	Category string  `json:"category,omitempty"`
	P        float64 `json:"p,omitempty"`
	Failed   bool    `json:"failed,omitempty"`
}

// item is one transaction moving through a run.
type item struct {
	d     store.TransactionDetail
	txn   Txn
	feat  features
	pred  *Prediction
	top   []string // ranked candidate names
	pTop  float64
	rule  string // the rule's category, if a rule decided
	hint  *secondOpinion
}

// run is the state of one Run.
type run struct {
	*Pipeline
	mode     writer.Mode
	tau      float64
	key      store.DecisionKey
	catIDs   map[string]string // folded candidate name → category id
	catNames map[string]string // folded candidate name → candidate name
	cands    []Candidate
	res      *Result
}

// Run takes eligible transactions through guardrails, rules and the model, logs
// a decision for each, and applies (effective auto), asks, or in dry_run turns
// every decision into a dry_run_check question. cal is calibrate.Current().
func (p *Pipeline) Run(ctx context.Context, cal Calibration) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.RunTimeout)
	defer cancel()
	res := Result{}

	if p.cfg.Mode == writer.ModeOff {
		res.Mode = writer.ModeOff
		p.cfg.SetMode(writer.ModeOff)
		return res, nil
	}

	cats, err := p.cfg.Store.CategoriesWithGroups(ctx, p.cfg.PlanID)
	if err != nil {
		p.cfg.SetMode(writer.ModeDryRun) // fail safe: nothing may apply
		return res, err
	}
	set := Candidates(cats, p.cfg.Policy)
	for _, w := range set.Warnings {
		p.cfg.Log.Warn("classify: policy warning", "warning", w)
	}
	policyHash, candsHash := PolicyHash(p.cfg.Policy), CandidatesHash(set.Candidates)

	r := &run{Pipeline: p, res: &res, cands: set.Candidates,
		catIDs: map[string]string{}, catNames: map[string]string{}}
	var warn string
	r.mode, r.tau, warn = effectiveMode(p.cfg.Mode, p.cfg.ModelID, policyHash, candsHash, cal)
	res.Mode = r.mode
	p.cfg.SetMode(r.mode) // before anything below can reach the Applier
	if warn != "" {
		res.Warnings = append(res.Warnings, warn)
		p.cfg.Log.Warn("classify: " + warn)
	}
	r.key = store.DecisionKey{Model: p.cfg.ModelID, PolicyHash: policyHash, CandidatesHash: candsHash, Mode: string(r.mode)}

	for _, c := range set.Candidates {
		r.catNames[fold(c.Name)] = c.Name
	}
	for _, c := range cats {
		if _, ok := r.catNames[fold(c.Name)]; ok && !c.Deleted {
			r.catIDs[fold(c.Name)] = c.ID
		}
	}

	if r.mode == writer.ModeAuto {
		if err := r.closeDryRunChecks(ctx); err != nil {
			return res, err
		}
	}
	if err := r.decide(ctx, set, cats); err != nil {
		return res, err
	}
	if err := r.post(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// effectiveMode is the mode this run acts in, its τ, and a warning when a
// configured auto runs as dry_run.
func effectiveMode(configured writer.Mode, model, policyHash, candsHash string, cal Calibration) (writer.Mode, float64, string) {
	tau := DefaultTau
	if cal.Tau != nil {
		tau = *cal.Tau
	}
	switch configured {
	case writer.ModeDryRun:
		return writer.ModeDryRun, tau, ""
	case writer.ModeAuto:
	default:
		return writer.ModeDryRun, tau, fmt.Sprintf("unknown mode %q, running as dry_run", configured)
	}
	switch {
	case !cal.AutoEligible || cal.Tau == nil:
		return writer.ModeDryRun, tau, "auto running as dry_run: calibration is not auto-eligible"
	case cal.Model != model:
		return writer.ModeDryRun, tau, "auto running as dry_run: calibration was measured on another model"
	case cal.PolicyHash != policyHash:
		return writer.ModeDryRun, tau, "auto running as dry_run: policy changed since calibration"
	case cal.CandidatesHash != candsHash:
		return writer.ModeDryRun, tau, "auto running as dry_run: category list changed since calibration"
	}
	return writer.ModeAuto, tau, ""
}

// closeDryRunChecks closes every live dry_run_check question on entering auto.
func (r *run) closeDryRunChecks(ctx context.Context) error {
	for _, list := range []func(context.Context, string) ([]store.Question, error){
		r.cfg.Store.OpenQuestions, r.cfg.Store.PendingQuestions,
	} {
		qs, err := list(ctx, r.cfg.PlanID)
		if err != nil {
			return err
		}
		for _, q := range qs {
			if q.Kind != store.QuestionDryRunCheck {
				continue
			}
			if err := r.close(ctx, q, ReasonSupersededByAuto); err != nil {
				return err
			}
			r.res.SupersededByAuto++
		}
	}
	return nil
}

// close closes q and, if it was posted, edits its message.
func (r *run) close(ctx context.Context, q store.Question, reason string) error {
	ok, err := r.cfg.Store.CloseQuestion(ctx, q.ID, reason)
	if err != nil || !ok || q.MessageTS == nil {
		return err
	}
	if err := r.cfg.Asker.Closed(ctx, q, reason); err != nil {
		r.cfg.Log.Error("classify: question message update failed", "err", err)
	}
	return nil
}

// decide reads the eligible transactions and logs a decision for each one
// without a current decision.
func (r *run) decide(ctx context.Context, set CandidateSet, cats []store.CategoryWithGroup) error {
	st := r.cfg.Store
	since := r.cfg.Now().AddDate(0, 0, -r.cfg.BacklogDays).Format(time.DateOnly)
	rows, err := st.EligibleTransactions(ctx, r.cfg.PlanID, since, r.cfg.MaxPerRun)
	if err != nil {
		return err
	}
	r.res.Eligible = len(rows)
	if len(rows) == 0 {
		return nil
	}
	accounts, err := st.Accounts(ctx, r.cfg.PlanID)
	if err != nil {
		return err
	}
	accountType := map[string]string{}
	for _, a := range accounts {
		accountType[a.ID] = a.Type
	}
	groupOf := map[string]string{}
	for _, c := range cats {
		groupOf[c.ID] = c.GroupName
	}
	ids := make([]string, len(rows))
	for i, d := range rows {
		ids[i] = d.ID
	}
	locks, err := st.Locks(ctx, r.cfg.PlanID, ids)
	if err != nil {
		return err
	}
	names := make([]string, len(set.Candidates))
	for i, c := range set.Candidates {
		names[i] = c.Name
	}
	history, err := st.LabeledHistory(ctx, r.cfg.HistoryPlanIDs, names, set.SetAsides)
	if err != nil {
		return err
	}
	rules := NewRules(history, set.Candidates)

	var applyPath, toModel []*item
	for _, d := range rows {
		current, err := st.HasCurrentDecision(ctx, r.cfg.PlanID, d.ID, r.key)
		if err != nil {
			return err
		}
		if current {
			r.res.AlreadyDecided++
			continue
		}
		it := &item{d: d, txn: rules.NewTxn(d)}
		it.feat = features{Payee: it.txn.Payee, Memo: it.txn.Memo, Account: it.txn.Account, Date: d.Date,
			AmountMilli: d.Amount, History: it.txn.PayeeHistory}

		if r.mode == writer.ModeAuto && r.cfg.AutoSince != "" && d.Date < r.cfg.AutoSince {
			r.res.BeforeAutoSince++
			if err := r.skipped(ctx, it, SkipBeforeAutoSince); err != nil {
				return err
			}
			continue
		}
		row := Row{Detail: d, AccountType: accountType[d.AccountID], Locks: locks[d.ID]}
		if d.CategoryID != nil {
			row.CategoryGroup = groupOf[*d.CategoryID]
		}
		if skip := Guard(row); skip != SkipNone {
			r.res.Guarded++
			if err := r.skipped(ctx, it, skip); err != nil {
				return err
			}
			continue
		}
		if cat, ok := rules.Decide(it.txn.Payee); ok {
			if _, ok := r.catIDs[fold(cat)]; ok {
				it.rule = cat
				r.res.Rule++
				applyPath = append(applyPath, it)
				continue
			}
		}
		toModel = append(toModel, it)
	}

	asks, more, err := r.classify(ctx, toModel)
	if err != nil {
		return err
	}
	applyPath = append(applyPath, more...)
	if err := r.apply(ctx, applyPath); err != nil {
		return err
	}
	for _, it := range asks {
		if err := r.ask(ctx, it); err != nil {
			return err
		}
	}
	return nil
}

// classify runs the model over items and splits them into the ask and apply
// paths. Items with a failed or null prediction get no decision. The second
// opinion runs on the ask path only.
func (r *run) classify(ctx context.Context, items []*item) (asks, applies []*item, err error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	txns := make([]Txn, len(items))
	for i, it := range items {
		txns[i] = it.txn
	}
	preds, err := r.cfg.Model.Classify(ctx, txns, r.cands)
	if err == nil && len(preds) != len(items) {
		err = fmt.Errorf("classify: model returned %d predictions for %d transactions", len(preds), len(items))
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		r.res.ModelFailures += len(items)
		r.cfg.Log.Error("classify: model call failed; retrying next run", "transactions", len(items), "err", err)
		return nil, nil, nil
	}
	for i, it := range items {
		pred := preds[i]
		top := r.rank(pred)
		if len(top) == 0 {
			r.res.ModelFailures++
			continue
		}
		it.pred, it.top, it.pTop = &pred, top, pred.Probs[top[0]]
		it.feat.Top = top
		tau := r.tau
		it.feat.Tau = &tau
		if pred.Model != r.cfg.ModelID {
			it.feat.PredModel = pred.Model
		}
		// In auto, τ is valid only for the model it was measured on.
		pass := it.pTop >= r.tau && (r.mode != writer.ModeAuto || pred.Model == r.cfg.ModelID)
		if pass {
			r.res.TauPass++
			applies = append(applies, it)
		} else {
			r.res.TauMiss++
			asks = append(asks, it)
		}
	}
	r.secondOpinion(ctx, asks)
	return asks, applies, nil
}

// rank orders a prediction's candidates by probability, best first, keeping
// only names in this run's candidate list. Empty means a null result.
func (r *run) rank(pred Prediction) []string {
	type scored struct {
		name string
		p    float64
	}
	var all []scored
	for name, p := range pred.Probs {
		if c, ok := r.catNames[fold(name)]; ok {
			all = append(all, scored{c, p})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].p != all[j].p {
			return all[i].p > all[j].p
		}
		return all[i].name < all[j].name
	})
	out := make([]string, len(all))
	for i, s := range all {
		out[i] = s.name
	}
	return out
}

// secondOpinion asks the advisory model about the τ-miss band. Its answer is a
// hint and never changes an item's path; a failure is logged and recorded.
func (r *run) secondOpinion(ctx context.Context, items []*item) {
	if r.cfg.SecondOpinion == nil || len(items) == 0 {
		return
	}
	txns := make([]Txn, len(items))
	for i, it := range items {
		txns[i] = it.txn
	}
	preds, err := r.cfg.SecondOpinion.Classify(ctx, txns, r.cands)
	if err == nil && len(preds) != len(items) {
		err = errors.New("classify: second opinion returned the wrong number of predictions")
	}
	for i, it := range items {
		so := &secondOpinion{Model: r.cfg.SecondOpinionModelID}
		it.hint = so
		if err != nil {
			so.Failed = true
			continue
		}
		if so.Model == "" {
			so.Model = preds[i].Model
		}
		if top := r.rank(preds[i]); len(top) > 0 {
			so.Category, so.P = top[0], preds[i].Probs[top[0]]
			r.res.SecondOpinions++
		} else {
			so.Failed = true
		}
	}
	if err != nil {
		r.cfg.Log.Error("classify: second opinion failed; asking anyway", "transactions", len(items), "err", err)
	}
}

// apply takes the apply path: in auto, one batch to the Applier and a decision
// per outcome; in dry_run, a would_apply decision and a dry_run_check question.
func (r *run) apply(ctx context.Context, items []*item) error {
	if len(items) == 0 {
		return nil
	}
	if r.mode != writer.ModeAuto {
		for _, it := range items {
			if _, err := r.record(ctx, it, store.OutcomeWouldApply); err != nil {
				return err
			}
			r.res.WouldApply++
			if err := r.question(ctx, it, store.QuestionDryRunCheck, "would apply"); err != nil {
				return err
			}
		}
		return nil
	}
	batch := writer.Batch{PlanID: r.cfg.PlanID}
	for _, it := range items {
		batch.Writes = append(batch.Writes, writer.Write{TxnID: it.d.ID, CategoryID: r.catIDs[fold(it.category())],
			Approve: true, DecidedCategoryID: it.d.CategoryID})
	}
	outs, err := r.cfg.Applier.Apply(ctx, batch, store.ActorSystem, store.ViaScheduler)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.res.ApplyFailed += len(items)
		r.cfg.Log.Error("classify: apply failed; retrying next run", "transactions", len(items), "err", err)
		return nil
	}
	byTxn := map[string]writer.Outcome{}
	for _, o := range outs {
		byTxn[o.TxnID] = o
	}
	for _, it := range items {
		o, ok := byTxn[it.d.ID]
		if !ok {
			r.res.ApplyFailed++
			continue
		}
		it.feat.WriteStatus, it.feat.WriteReason = string(o.Status), o.Reason
		var outcome string
		switch o.Status {
		case writer.StatusDone, writer.StatusUnknown: // unknown: Reconcile finishes it
			outcome = store.OutcomeApplied
			r.res.Applied++
		case writer.StatusSkipped:
			outcome = store.OutcomeSkipped
			r.res.ApplySkipped++
		default:
			r.res.ApplyFailed++
			continue
		}
		if _, err := r.record(ctx, it, outcome); err != nil {
			return err
		}
	}
	return nil
}

// ask logs an asked decision and queues a question with the top-3.
func (r *run) ask(ctx context.Context, it *item) error {
	if _, err := r.record(ctx, it, store.OutcomeAsked); err != nil {
		return err
	}
	kind := store.QuestionCategorize
	if r.mode != writer.ModeAuto {
		kind = store.QuestionDryRunCheck
	}
	return r.question(ctx, it, kind, "would ask")
}

// question closes any live question on the transaction and queues a new one as
// pending; post decides when it goes out. verdict labels a dry_run_check's
// first option.
func (r *run) question(ctx context.Context, it *item, kind, verdict string) error {
	qs, err := r.cfg.Store.QuestionsBySubject(ctx, r.cfg.PlanID, it.d.ID)
	if err != nil {
		return err
	}
	for _, q := range qs {
		if q.Status == store.QuestionOpen || q.Status == store.QuestionPending {
			if err := r.close(ctx, q, ReasonSuperseded); err != nil {
				return err
			}
			r.res.Superseded++
		}
	}
	names := it.top
	if it.rule != "" {
		names = []string{it.rule}
	}
	if len(names) > 3 {
		names = names[:3]
	}
	var opts []store.QuestionOption
	hinted := false
	for i, n := range names {
		o := store.QuestionOption{CategoryID: r.catIDs[fold(n)], CategoryName: n}
		var notes []string
		if i == 0 && kind == store.QuestionDryRunCheck {
			notes = append(notes, verdict)
		}
		if it.hint != nil && it.hint.Category == n {
			notes, hinted = append(notes, "second opinion"), true
		}
		if len(notes) > 0 {
			o.Label = n + " (" + joinNotes(notes) + ")"
		}
		opts = append(opts, o)
	}
	if it.hint != nil && it.hint.Category != "" && !hinted {
		opts = append(opts, store.QuestionOption{CategoryID: r.catIDs[fold(it.hint.Category)],
			CategoryName: it.hint.Category, Label: it.hint.Category + " (second opinion)"})
	}
	if _, err := r.cfg.Store.InsertQuestion(ctx, store.Question{PlanID: r.cfg.PlanID, Kind: kind, SubjectID: it.d.ID,
		Options: opts, CreatedAt: r.cfg.Now()}); err != nil {
		return err
	}
	r.res.Asked++
	return nil
}

func joinNotes(ns []string) string {
	out := ns[0]
	for _, n := range ns[1:] {
		out += "; " + n
	}
	return out
}

// category is the item's chosen category name.
func (it *item) category() string {
	if it.rule != "" {
		return it.rule
	}
	return it.top[0]
}

// skipped logs a skip decision.
func (r *run) skipped(ctx context.Context, it *item, why Skip) error {
	it.feat.Skip = why
	_, err := r.record(ctx, it, store.OutcomeSkipped)
	return err
}

// record writes the item's decision with outcome.
func (r *run) record(ctx context.Context, it *item, outcome string) (int64, error) {
	dec := store.Decision{PlanID: r.cfg.PlanID, TxnID: it.d.ID, Method: store.MethodRule, Model: r.key.Model,
		PolicyHash: r.key.PolicyHash, CandidatesHash: r.key.CandidatesHash, Mode: r.key.Mode, Outcome: outcome,
		CreatedAt: r.cfg.Now()}
	if it.rule != "" || it.pred != nil {
		name := it.category()
		id := r.catIDs[fold(name)]
		dec.CategoryName, dec.CategoryID = &name, &id
	}
	if it.pred != nil {
		dec.Method = store.MethodModel
		p := it.pTop
		dec.Confidence = &p
		if len(it.top) > 1 {
			ru := it.top[1]
			dec.RunnerUp = &ru
		}
	}
	if it.hint != nil {
		b, err := json.Marshal(it.hint)
		if err != nil {
			return 0, err
		}
		dec.SecondOpinion = b
	}
	b, err := json.Marshal(it.feat)
	if err != nil {
		return 0, err
	}
	dec.Features = b
	return r.cfg.Store.InsertDecision(ctx, dec)
}

// post posts pending questions, oldest first, while fewer than QuestionCap are
// open. A failed post stays pending for the next run.
func (r *run) post(ctx context.Context) error {
	open, err := r.cfg.Store.OpenQuestions(ctx, r.cfg.PlanID)
	if err != nil {
		return err
	}
	pending, err := r.cfg.Store.PendingQuestions(ctx, r.cfg.PlanID)
	if err != nil {
		return err
	}
	slots := r.cfg.QuestionCap - len(open)
	for _, q := range pending {
		if slots <= 0 {
			break
		}
		if err := r.cfg.Asker.Ask(ctx, q.ID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.res.AskFailures++
			r.cfg.Log.Error("classify: posting a question failed; retrying next run", "err", err)
			continue
		}
		r.res.Posted++
		slots--
	}
	r.res.Pending = len(pending) - r.res.Posted
	return nil
}
