package alerts

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
)

// StaleAfterDays is the import age past which a linked account alerts: the
// MCP's no_recent_import rule.
const StaleAfterDays = 14

// baselineKey is the dedupe key of the plan's link-error baseline marker.
const baselineKey = "link_error"

// Freshness raises and re-arms the freshness alerts for the plan's accounts,
// from the mirror's state after a sync (sync.Syncer.Freshness). Run it after
// every successful sync; initial is the tick's sync.Result.Initial.
//
// It works from state, not from the tick's LinkEvents: an account in error
// with no stored alert is sent again each tick until a post succeeds, so a
// Slack failure retries even though its LinkEvent fired only once.
//
//   - Link error: a live account whose link is in error alerts once. The
//     baseline (a plan's first sync, or the first run against an existing
//     mirror) records the accounts already in error without posting. When the
//     error clears, the alert is cleared, so a later error alerts again.
//   - Stale import: a linked, open account not in link error whose newest
//     import is over StaleAfterDays old alerts once per episode; an import
//     inside the window ends the episode. An account in link error keeps its
//     stale state as it was, so a broken link alerts once, as a link error.
//
// Accounts that never imported are not stale here (the MCP reports them as
// never_imported). Store failures are returned joined after every account was
// tried; a failed post is not an error.
func (a *Alerter) Freshness(ctx context.Context, initial bool, accounts []finsync.AccountFreshness) error {
	baselined, err := a.store.HasAlert(ctx, a.planID, kindBaseline, baselineKey)
	if err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	baseline := initial || !baselined
	now := a.now()
	var errs []error
	for _, acc := range accounts {
		if acc.Closed {
			continue
		}
		if err := a.linkError(ctx, acc, baseline); err != nil {
			errs = append(errs, err)
		}
		if err := a.staleImport(ctx, acc, now); err != nil {
			errs = append(errs, err)
		}
	}
	if baseline && !baselined && len(errs) == 0 {
		// Only once every in-error account is recorded: a partial baseline
		// would let the rest alert on the next tick.
		if _, _, err := a.store.InsertAlert(ctx, baselineMarker(a.planID, now)); err != nil {
			errs = append(errs, fmt.Errorf("alerts: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (a *Alerter) linkError(ctx context.Context, acc finsync.AccountFreshness, baseline bool) error {
	if !acc.InError {
		return a.clearIfSet(ctx, KindLinkError, acc.AccountID)
	}
	al := Alert{
		Kind: KindLinkError, DedupeKey: acc.AccountID,
		Text: fmt.Sprintf(":warning: *%s*: the bank link is in error, so its transactions have stopped importing. "+
			"Reconnect it in YNAB.", acc.Name),
	}
	if baseline {
		return a.record(ctx, al)
	}
	_, err := a.Send(ctx, al)
	return err
}

func (a *Alerter) staleImport(ctx context.Context, acc finsync.AccountFreshness, now time.Time) error {
	if acc.InError {
		return nil
	}
	days := -1
	if acc.Linked && acc.NewestImportDate != "" {
		d, err := daysSince(acc.NewestImportDate, now)
		if err != nil {
			return fmt.Errorf("alerts: account import date: %w", err)
		}
		days = d
	}
	if days <= StaleAfterDays {
		return a.clearIfSet(ctx, KindStaleImport, acc.AccountID)
	}
	_, err := a.Send(ctx, Alert{
		Kind: KindStaleImport, DedupeKey: acc.AccountID,
		Text: fmt.Sprintf(":hourglass: *%s*: no bank import for %d days (newest is from %s). "+
			"The link may be broken, or the account may just have had no activity.", acc.Name, days, acc.NewestImportDate),
	})
	return err
}

// record stores al as already raised without posting it: the baseline.
func (a *Alerter) record(ctx context.Context, al Alert) error {
	_, _, err := a.store.InsertAlert(ctx, baselineAlert(a.planID, al, a.now()))
	if err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	return nil
}

// clearIfSet clears (kind, key) if stored; the read first spares a write per
// healthy account per tick.
func (a *Alerter) clearIfSet(ctx context.Context, kind, key string) error {
	has, err := a.store.HasAlert(ctx, a.planID, kind, key)
	if err != nil {
		return fmt.Errorf("alerts: %w", err)
	}
	if !has {
		return nil
	}
	if err := a.Clear(ctx, kind, key); err != nil {
		return err
	}
	a.log.Info("alert re-armed", "kind", kind)
	return nil
}

// daysSince counts calendar days from date to now in now's zone; a
// future-dated import counts as today. It matches the MCP's rule.
func daysSince(date string, now time.Time) (int, error) {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return 0, err
	}
	y, m, day := now.Date()
	today := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	return max(0, int(today.Sub(d)/(24*time.Hour))), nil
}

// baselineAlert is al stored as already raised, never posted. It is
// critical, so it spends none of the budget, and status sent, the only
// non-rolled-up status, so the monthly report's rolled-up list skips it.
func baselineAlert(planID string, al Alert, now time.Time) store.Alert {
	return store.Alert{
		PlanID: planID, Kind: al.Kind, DedupeKey: al.DedupeKey, Critical: true, Status: store.AlertSent,
		Body: "(baseline, not posted) " + al.Text, CreatedAt: now,
	}
}

// baselineMarker records that the plan's link-error baseline was taken.
func baselineMarker(planID string, now time.Time) store.Alert {
	return baselineAlert(planID, Alert{Kind: kindBaseline, DedupeKey: baselineKey,
		Text: "link errors present at the first run were recorded without alerting"}, now)
}
