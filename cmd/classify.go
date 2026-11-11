package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/alerts"
	"github.com/deasa/YNAB_AutoCategorizer/calibrate"
	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/classify/jev"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/llm"
	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/writer"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

var (
	// classifyTimeout bounds everything that runs after one sync (Reconcile,
	// the pipeline, CloseStale), so sync ticks keep flowing.
	classifyTimeout = 5 * time.Minute
	// probeEvery is how often the claude auth probe runs (also once at
	// startup); claudeProbeTimeout bounds one probe, its retry included.
	probeEvery         = 24 * time.Hour
	claudeProbeTimeout = 5 * time.Minute
)

// secondOpinionBatch is how many transactions go in one claude call (§5a).
const secondOpinionBatch = 10

// newClassifyModel builds the pipeline's model and its pinned id (tests
// replace it, so a serve test never reaches Jev).
var newClassifyModel = func(apiKey string) (classify.Model, string, error) {
	c, err := jev.New(apiKey)
	return c, jev.PinnedModel, err
}

// claudeRunner is what serve needs from the claude CLI; *llm.Runner meets it.
type claudeRunner interface {
	Probe(ctx context.Context) error
}

// newClaude builds the claude runner and the second opinion over it (tests
// replace it, so no test runs claude).
var newClaude = func(cfg llm.Config) (claudeRunner, classify.Model, error) {
	r, err := llm.New(cfg)
	if err != nil {
		return nil, nil, err
	}
	return r, llm.NewClassifier(r, secondOpinionBatch), nil
}

// newWriterAPI builds the writer's YNAB client (tests replace it, so a test
// never PATCHes YNAB).
var newWriterAPI = func(token, planID string, l *log.Logger, gov *ynab.Governor) writer.API {
	return ynab.NewClient(token, planID, l, ynab.WithGovernor(gov))
}

// currentCalibration is the τ and model the pipeline gates on (tests replace
// it).
var currentCalibration = func() classify.Calibration {
	c := calibrate.Current()
	return classify.Calibration{Model: c.Model, PolicyHash: c.PolicyHash, CandidatesHash: c.CandidatesHash,
		Tau: c.Tau, AutoEligible: c.AutoEligible}
}

// questionAsker is what serve needs from Slack's questions; *slack.Questions
// meets it.
type questionAsker interface {
	classify.Asker
	CloseStale(ctx context.Context, planID string) (int, error)
}

// errSlackOff: a question can't be posted while Socket Mode is off; the
// pipeline leaves it pending.
var errSlackOff = errors.New("slack socket mode is off")

// lateAsker resolves Slack's questions when called, since Slack starts after
// the pipeline is built.
type lateAsker struct{ a *app }

func (l lateAsker) questions() questionAsker {
	if l.a.socket == nil {
		return nil
	}
	return l.a.socket.Questions()
}

func (l lateAsker) Ask(ctx context.Context, id int64) error {
	if q := l.questions(); q != nil {
		return q.Ask(ctx, id)
	}
	return errSlackOff
}

func (l lateAsker) Closed(ctx context.Context, qu store.Question, reason string) error {
	if q := l.questions(); q != nil {
		return q.Closed(ctx, qu, reason)
	}
	return nil
}

// classifyWriter returns serve's one writer, shared by the pipeline and
// Slack's answers, with its Gate (dry_run until a pipeline run sets it).
func (a *app) classifyWriter() *writer.Writer {
	if a.writer == nil {
		cfg := a.cfg
		a.gate = writer.NewGate()
		a.writer = writer.New(writer.Config{
			PlanID:    cfg.YNABBudgetID,
			AutoSince: cfg.Classify.AutoSince,
			Gate:      a.gate,
			API:       newWriterAPI(cfg.YNABAccessToken, cfg.YNABBudgetID, a.ynabLog, a.gov),
			DB:        a.db,
			Resync: func(ctx context.Context) error {
				_, err := a.syncPlan(ctx, cfg.YNABBudgetID, "active")
				return err
			},
		})
	}
	return a.writer
}

// categorizer runs after each successful sync of the active plan, in order:
// Reconcile, the pipeline (nil when CLASSIFY_MODE is off), CloseStale.
type categorizer struct {
	Mode       string
	Reconcile  func(ctx context.Context, syncStart time.Time) error
	Run        func(ctx context.Context) (classify.Result, error)
	CloseStale func(ctx context.Context) (int, error)
	Timeout    time.Duration
	a          *app
}

// newCategorizer wires categorization for serve. Mode off builds no pipeline
// and no model. A bad Jev or claude setup fails startup.
func (a *app) newCategorizer() (*categorizer, error) {
	cfg := a.cfg
	w := a.classifyWriter()
	c := &categorizer{
		Mode:      cfg.Classify.Mode,
		Reconcile: w.Reconcile,
		CloseStale: func(ctx context.Context) (int, error) {
			q := lateAsker{a}.questions()
			if q == nil {
				return 0, nil
			}
			return q.CloseStale(ctx, cfg.YNABBudgetID)
		},
		Timeout: classifyTimeout,
		a:       a,
	}
	if cfg.Classify.Mode == config.ClassifyOff || cfg.Classify.Mode == "" {
		a.log.Info("categorize off")
		return c, nil
	}
	model, modelID, err := newClassifyModel(cfg.Classify.JevAPIKey)
	if err != nil {
		return nil, fmt.Errorf("categorize: jev: %w", err)
	}
	policy, err := classify.DefaultPolicy()
	if err != nil {
		return nil, fmt.Errorf("categorize: policy: %w", err)
	}
	pc := classify.Config{
		PlanID:      cfg.YNABBudgetID,
		Mode:        writer.Mode(cfg.Classify.Mode),
		Policy:      policy,
		ModelID:     modelID,
		Model:       redactModel{model, a.redact},
		Store:       a.db,
		Applier:     redactApplier{w, a.redact},
		Asker:       redactAsker{lateAsker{a}, a.redact},
		SetMode:     a.gate.Set,
		AutoSince:   cfg.Classify.AutoSince,
		BacklogDays: cfg.Classify.BacklogDays,
		QuestionCap: cfg.Classify.MaxOpenQuestions,
		RunTimeout:  classifyTimeout,
		Log:         a.log,
	}
	if cfg.Classify.Claude() {
		runner, second, err := newClaude(a.claudeConfig())
		if err != nil {
			return nil, fmt.Errorf("categorize: claude: %w", err)
		}
		a.claude = runner
		pc.SecondOpinion, pc.SecondOpinionModelID = redactModel{second, a.redact}, llm.ModelID
	}
	c.Run = func(ctx context.Context) (classify.Result, error) {
		// The same plans calibration labelled from, read each run, since a
		// `finbot sync --archived` scan can change them after startup.
		plans, err := calibrate.LabelPlans(ctx, a.db, cfg.YNABBudgetID)
		if err != nil {
			return classify.Result{}, err
		}
		run := pc
		run.HistoryPlanIDs = plans
		return classify.New(run).Run(ctx, currentCalibration())
	}
	a.log.Info("categorize enabled", "mode", cfg.Classify.Mode, "model", modelID,
		"second_opinion", cfg.Classify.Claude())
	return c, nil
}

// claudeConfig is the runner's config: the pinned binary, the token, and an
// empty work directory beside the database.
func (a *app) claudeConfig() llm.Config {
	cc := a.cfg.Classify
	dir, err := filepath.Abs(filepath.Join(filepath.Dir(a.cfg.DBPath), "claude-work"))
	if err != nil {
		dir = "" // llm.New rejects it
	}
	return llm.Config{
		Binary:  cc.ClaudeBin,
		Token:   cc.ClaudeToken,
		WorkDir: dir,
		Getenv: func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return cc.ClaudeConfigDir
			}
			return os.Getenv(k)
		},
		RetryDelay: 5 * time.Second,
	}
}

// closeStaleReserve is the part of Timeout kept for CloseStale, so it still
// runs when the pipeline used up its share.
const closeStaleReserve = 30 * time.Second

// After runs one categorization pass after a successful sync that started at
// syncStart, bounded by Timeout in all: Reconcile and the pipeline share
// Timeout less closeStaleReserve, and CloseStale gets its own reserve after.
// Every step is only logged: nothing here fails the sync job or touches
// Health.
func (c *categorizer) After(parent context.Context, syncStart time.Time) {
	a := c.a
	reserve := min(closeStaleReserve, c.Timeout/2)
	ctx, cancel := context.WithTimeout(parent, c.Timeout-reserve)
	defer cancel()
	if err := c.Reconcile(ctx, syncStart); err != nil {
		a.log.Error("categorize reconcile failed", "err", a.redact(err.Error()))
	}
	if c.Run != nil {
		res, err := c.Run(ctx)
		c.logRun(res, err)
	}
	cancel()
	if parent.Err() != nil {
		return
	}
	ctx, cancel = context.WithTimeout(parent, reserve)
	defer cancel()
	if n, err := c.CloseStale(ctx); err != nil {
		a.log.Error("categorize close stale questions failed", "err", a.redact(err.Error()))
	} else if n > 0 {
		a.log.Info("categorize closed stale questions", "closed", n)
	}
}

// logRun logs one line per pipeline run: modes, counts and warnings, no ids.
// A configured auto that ran as anything else is a warning.
func (c *categorizer) logRun(res classify.Result, err error) {
	a := c.a
	warnings := make([]string, len(res.Warnings))
	for i, w := range res.Warnings {
		warnings[i] = a.redact(w)
	}
	attrs := []any{"mode", c.Mode, "effective_mode", string(res.Mode),
		"counts", map[string]int{
			"eligible": res.Eligible, "already_decided": res.AlreadyDecided, "guarded": res.Guarded,
			"before_auto_since": res.BeforeAutoSince, "rule": res.Rule, "tau_pass": res.TauPass,
			"tau_miss": res.TauMiss, "model_failures": res.ModelFailures, "second_opinions": res.SecondOpinions,
			"applied": res.Applied, "would_apply": res.WouldApply, "apply_skipped": res.ApplySkipped,
			"apply_failed": res.ApplyFailed, "asked": res.Asked, "superseded": res.Superseded,
			"superseded_by_auto": res.SupersededByAuto, "posted": res.Posted, "ask_failures": res.AskFailures,
			"pending": res.Pending,
		},
		"warnings", warnings}
	switch {
	case err != nil:
		a.log.Error("categorize run failed", append(attrs, "err", a.redact(err.Error()))...)
	case c.Mode == config.ClassifyAuto && res.Mode != writer.ModeAuto:
		a.log.Warn("categorize run (auto downgraded)", attrs...)
	default:
		a.log.Info("categorize run", attrs...)
	}
}

// claudeProbeKey is the dedupe key of the claude auth alert.
const claudeProbeKey = "probe"

// probeClaude runs the daily claude auth probe. A failure is logged and raises
// the critical claude_auth alert (sent once until a pass clears it). It never
// touches Health.
func (a *app) probeClaude(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, claudeProbeTimeout)
	err := a.claude.Probe(pctx)
	cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		a.log.Info("claude probe ok")
		if a.alerter != nil {
			if cerr := a.alerter.Clear(ctx, alerts.KindClaudeAuth, claudeProbeKey); cerr != nil {
				a.log.Error("clear claude probe alert", "err", a.redact(cerr.Error()))
			}
		}
		return nil
	}
	status := 0
	var ce *llm.CallError
	if errors.As(err, &ce) {
		status = ce.APIErrorStatus
	}
	a.log.Error("claude probe failed", "api_error_status", status, "err", a.redact(err.Error()))
	if a.alerter == nil {
		return err
	}
	text := "finbot: the daily claude auth probe failed"
	if status != 0 {
		text += fmt.Sprintf(" (api_error_status %d)", status)
	}
	text += ". The claude second opinion is down until it passes; if the token expired, run " +
		"`DISABLE_UPDATES=1 claude setup-token` and update CLAUDE_CODE_OAUTH_TOKEN on hoid."
	out, aerr := a.alerter.Send(ctx, alerts.Alert{Kind: alerts.KindClaudeAuth, DedupeKey: claudeProbeKey, Text: text})
	if aerr != nil {
		a.log.Error("claude probe alert failed", "err", a.redact(aerr.Error()))
	} else {
		a.log.Info("claude probe alert", "outcome", out.String())
	}
	return err
}

// redactedError carries a redacted message but keeps the chain, so the
// pipeline's errors.Is checks still work. Defence in depth: the packages
// below already keep ids and secrets out of their errors.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

func redactErr(err error, redact func(string) string) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: redact(err.Error()), err: err}
}

// redactModel redacts a classify.Model's errors before the pipeline logs them.
type redactModel struct {
	m      classify.Model
	redact func(string) string
}

func (r redactModel) Classify(ctx context.Context, txns []classify.Txn, cands []classify.Candidate) ([]classify.Prediction, error) {
	p, err := r.m.Classify(ctx, txns, cands)
	return p, redactErr(err, r.redact)
}

// redactApplier redacts the writer's errors before the pipeline logs them.
type redactApplier struct {
	w      classify.Applier
	redact func(string) string
}

func (r redactApplier) Apply(ctx context.Context, b writer.Batch, actor, via string) ([]writer.Outcome, error) {
	out, err := r.w.Apply(ctx, b, actor, via)
	return out, redactErr(err, r.redact)
}

// redactAsker redacts Slack's errors before the pipeline logs them.
type redactAsker struct {
	a      classify.Asker
	redact func(string) string
}

func (r redactAsker) Ask(ctx context.Context, id int64) error {
	return redactErr(r.a.Ask(ctx, id), r.redact)
}

func (r redactAsker) Closed(ctx context.Context, q store.Question, reason string) error {
	return redactErr(r.a.Closed(ctx, q, reason), r.redact)
}
