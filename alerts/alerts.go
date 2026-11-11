// Package alerts sends finbot's Slack alerts (R11a, §3.7) and enforces the
// alert budget in code (G5): critical alerts always send; a non-critical alert
// past the second sent in a calendar month is stored as rolled_up for the
// monthly report instead.
//
// Send is the one entry point. Every alert carries a dedupe key, and the
// store's alert history makes it go out at most once. Freshness turns the
// mirror's per-account import state into link-error and stale-import alerts;
// Anomalies runs the duplicate, price-change and large-charge rules over new
// transactions.
//
// Alert text names accounts by name only; dedupe keys may hold ids and are
// never logged or posted.
package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// Alert kinds. The critical ones bypass the budget.
const (
	KindLinkError       = "link_error"        // an account's bank link went into error
	KindStaleImport     = "stale_import"      // a linked account hasn't imported for StaleAfterDays
	KindClaudeAuth      = "claude_auth"       // the daily claude auth probe failed
	KindSocketSelfCheck = "socket_self_check" // the Slack Socket Mode self-check failed

	// kindBaseline marks that the link-error baseline of a plan was taken.
	// It is stored, never posted.
	kindBaseline = "freshness_baseline"
)

// critical are the kinds that always send. Criticality is decided here, by
// kind, so no caller can push a noisy alert past the budget.
var critical = map[string]bool{
	KindLinkError:       true,
	KindStaleImport:     true,
	KindClaudeAuth:      true,
	KindSocketSelfCheck: true,
}

// Critical reports whether alerts of kind bypass the budget.
func Critical(kind string) bool { return critical[kind] }

// MonthlyBudget is how many non-critical alerts may be sent per calendar month
// (in the clock's zone, Denver on hoid); the rest are rolled up.
const MonthlyBudget = 2

// postTimeout caps one Slack post.
const postTimeout = 20 * time.Second

// Alert is one alert to raise.
type Alert struct {
	Kind string
	// DedupeKey, with the plan and Kind, identifies the alert: it is sent at
	// most once until the key is cleared. It may hold ids; it is never logged.
	DedupeKey string
	// Text is the Slack message. It must not hold ids.
	Text string
}

// Outcome is what Send did with an alert.
type Outcome int

const (
	// Sent: posted to Slack and stored.
	Sent Outcome = iota
	// RolledUp: over the month's budget; stored for the monthly report, not posted.
	RolledUp
	// Duplicate: already raised (sent or rolled up); nothing done.
	Duplicate
	// Unsent: the post failed; nothing is stored, so the next Send retries.
	Unsent
)

func (o Outcome) String() string {
	switch o {
	case Sent:
		return "sent"
	case RolledUp:
		return "rolled_up"
	case Duplicate:
		return "duplicate"
	case Unsent:
		return "unsent"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

// Poster posts one message to the alert channel.
type Poster interface {
	Post(ctx context.Context, text string) error
}

// Store is the alert history; *store.DB implements it.
type Store interface {
	InsertAlert(ctx context.Context, a store.Alert) (id int64, inserted bool, err error)
	HasAlert(ctx context.Context, planID, kind, dedupeKey string) (bool, error)
	DeleteAlert(ctx context.Context, planID, kind, dedupeKey string) (bool, error)
	CountSentNonCritical(ctx context.Context, planID string, from, to time.Time) (int, error)
}

// Config configures an Alerter.
type Config struct {
	Store  Store
	Poster Poster
	PlanID string
	Mirror Mirror           // needed by Anomalies only
	Now    func() time.Time // default time.Now; its zone sets the budget's months
	Logger *slog.Logger     // default slog.Default()
}

// Alerter sends alerts for one plan. It is not safe for concurrent use; the
// scheduler runs every job on one goroutine.
type Alerter struct {
	store  Store
	poster Poster
	mirror Mirror
	planID string
	now    func() time.Time
	log    *slog.Logger
}

// New returns an Alerter.
func New(cfg Config) *Alerter {
	a := &Alerter{store: cfg.Store, poster: cfg.Poster, mirror: cfg.Mirror, planID: cfg.PlanID, now: cfg.Now, log: cfg.Logger}
	if a.now == nil {
		a.now = time.Now
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a
}

// Send raises al. A duplicate does nothing. A non-critical alert over the
// month's budget is stored as rolled_up. Otherwise the alert is recorded,
// then posted; if the post fails, the failure is logged and the record is
// removed, so the alert stays unsent and the next Send retries it (Unsent, nil
// error). Recording first means a store failure can never follow a successful
// post, so a retry never double-sends. The error is a store failure only.
func (a *Alerter) Send(ctx context.Context, al Alert) (Outcome, error) {
	if al.Kind == "" || al.DedupeKey == "" {
		return 0, fmt.Errorf("alerts: alert needs a kind and a dedupe key")
	}
	if dup, err := a.store.HasAlert(ctx, a.planID, al.Kind, al.DedupeKey); err != nil {
		return 0, fmt.Errorf("alerts: %w", err)
	} else if dup {
		return Duplicate, nil
	}
	now := a.now()
	crit := Critical(al.Kind)
	status := store.AlertSent
	if !crit {
		from, to := monthOf(now)
		n, err := a.store.CountSentNonCritical(ctx, a.planID, from, to)
		if err != nil {
			return 0, fmt.Errorf("alerts: %w", err)
		}
		if n >= MonthlyBudget {
			status = store.AlertRolledUp
		}
	}
	_, inserted, err := a.store.InsertAlert(ctx, store.Alert{
		PlanID: a.planID, Kind: al.Kind, DedupeKey: al.DedupeKey, Critical: crit,
		Status: status, Body: al.Text, CreatedAt: now,
	})
	if err != nil {
		return 0, fmt.Errorf("alerts: %w", err)
	}
	if !inserted {
		return Duplicate, nil
	}
	if status == store.AlertRolledUp {
		a.log.Info("alert rolled up", "kind", al.Kind)
		return RolledUp, nil
	}
	pctx, cancel := context.WithTimeout(ctx, postTimeout)
	err = a.poster.Post(pctx, al.Text)
	cancel()
	if err == nil {
		a.log.Info("alert sent", "kind", al.Kind, "critical", crit)
		return Sent, nil
	}
	a.log.Warn("alert post failed; will retry", "kind", al.Kind, "err", err)
	// The run's ctx may be what ended the post; the un-record must still land.
	dctx, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer dcancel()
	if _, derr := a.store.DeleteAlert(dctx, a.planID, al.Kind, al.DedupeKey); derr != nil {
		a.log.Error("alert unsent but still recorded; it will not retry", "kind", al.Kind, "err", derr)
		return Unsent, fmt.Errorf("alerts: %w", derr)
	}
	return Unsent, nil
}

// Clear forgets the alert (kind, dedupeKey), so the same key can alert
// again: the episode it reported is over.
func (a *Alerter) Clear(ctx context.Context, kind, dedupeKey string) error {
	if _, err := a.store.DeleteAlert(ctx, a.planID, kind, dedupeKey); err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	return nil
}

// monthOf returns the calendar month holding t, in t's zone, as [from, to).
func monthOf(t time.Time) (from, to time.Time) {
	y, m, _ := t.Date()
	from = time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
	return from, from.AddDate(0, 1, 0)
}
