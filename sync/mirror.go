package sync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

// stitch ties subtransactions to their parents before a batch is written
// (spike F5: the plan endpoint returns them as a flat collection):
//   - a subtransaction whose parent is in neither the batch nor the mirror is
//     dropped (it would fail the deferred foreign key and sink the whole
//     tick); the same goes for scheduled subtransactions;
//   - when a parent arrives as a tombstone, its mirrored children that the
//     batch doesn't mention are tombstoned with it.
//
// It returns the number of orphans dropped.
func (s *Syncer) stitch(ctx context.Context, planID string, b *batch) (int, error) {
	r := s.db.Reader()
	parents := make(map[string]bool, len(b.transactions))
	for _, t := range b.transactions {
		parents[t.ID] = true
	}
	orphans := 0
	subs := b.subs[:0]
	seen := make(map[string]bool, len(b.subs))
	for _, sub := range b.subs {
		ok, err := has(ctx, r, parents, "transactions", planID, sub.TransactionID)
		if err != nil {
			return 0, err
		}
		if !ok {
			orphans++
			continue
		}
		seen[sub.ID] = true
		subs = append(subs, sub)
	}
	b.subs = subs

	for _, t := range b.transactions {
		if !t.Deleted {
			continue
		}
		children, err := liveSubs(ctx, r, planID, t.ID)
		if err != nil {
			return 0, err
		}
		for _, c := range children {
			if !seen[c.ID] {
				c.Deleted = true
				b.subs = append(b.subs, c)
			}
		}
	}

	sched := make(map[string]bool, len(b.scheduled))
	for _, t := range b.scheduled {
		sched[t.ID] = true
	}
	ssubs := b.scheduledSubs[:0]
	for _, sub := range b.scheduledSubs {
		ok, err := has(ctx, r, sched, "scheduled_transactions", planID, sub.ScheduledTransactionID)
		if err != nil {
			return 0, err
		}
		if !ok {
			orphans++
			continue
		}
		ssubs = append(ssubs, sub)
	}
	b.scheduledSubs = ssubs
	return orphans, nil
}

// has reports whether id is in batch or already mirrored in table.
func has(ctx context.Context, r *sql.DB, batch map[string]bool, table, planID, id string) (bool, error) {
	if batch[id] {
		return true, nil
	}
	var one int
	err := r.QueryRowContext(ctx, "SELECT 1 FROM "+table+" WHERE plan_id = ? AND id = ?", planID, id).Scan(&one)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("sync: look up %s parent: %w", table, err)
	}
	batch[id] = true
	return true, nil
}

// liveSubs returns the mirrored, not yet deleted children of a transaction.
func liveSubs(ctx context.Context, r *sql.DB, planID, txnID string) ([]store.Subtransaction, error) {
	rows, err := r.QueryContext(ctx, `SELECT id, amount, memo, payee_id, category_id,
		transfer_account_id, transfer_transaction_id
		FROM subtransactions WHERE plan_id = ? AND transaction_id = ? AND deleted = 0`, planID, txnID)
	if err != nil {
		return nil, fmt.Errorf("sync: read subtransactions: %w", err)
	}
	defer rows.Close()
	var out []store.Subtransaction
	for rows.Next() {
		c := store.Subtransaction{TransactionID: txnID}
		if err := rows.Scan(&c.ID, &c.Amount, &c.Memo, &c.PayeeID, &c.CategoryID,
			&c.TransferAccountID, &c.TransferTransactionID); err != nil {
			return nil, fmt.Errorf("sync: read subtransactions: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// linkEvents compares each incoming account's direct_import_in_error with
// the mirror. An account new to the mirror counts as previously healthy.
// Deleted accounts emit nothing.
func (s *Syncer) linkEvents(ctx context.Context, planID string, accounts []store.Account) ([]LinkEvent, error) {
	rows, err := s.db.Reader().QueryContext(ctx,
		"SELECT id, direct_import_in_error FROM accounts WHERE plan_id = ?", planID)
	if err != nil {
		return nil, fmt.Errorf("sync: read link state: %w", err)
	}
	defer rows.Close()
	prior := map[string]bool{}
	for rows.Next() {
		var id string
		var inErr bool
		if err := rows.Scan(&id, &inErr); err != nil {
			return nil, fmt.Errorf("sync: read link state: %w", err)
		}
		prior[id] = inErr
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: read link state: %w", err)
	}
	var out []LinkEvent
	for _, a := range accounts {
		if a.Deleted || a.DirectImportInError == prior[a.ID] {
			continue
		}
		out = append(out, LinkEvent{AccountID: a.ID, AccountName: a.Name, InError: a.DirectImportInError})
	}
	return out, nil
}

// AccountFreshness is how current one account's bank import is (§3.1 step 4).
// YNAB exposes no import timestamp, so it is derived from the mirror.
type AccountFreshness struct {
	AccountID string
	Name      string
	Closed    bool
	Linked    bool
	InError   bool
	// NewestImportDate is the newest date ("YYYY-MM-DD") of a live
	// transaction with an import_id; empty if there is none.
	NewestImportDate string
	// LastImportSeenAt is when this mirror first saw the newest import_id in
	// the account; nil if none. Rows present at the initial load all carry
	// that load's time, so this only becomes informative after the first
	// import arrives through a delta.
	LastImportSeenAt *time.Time
	// SyncedAt is when the plan last synced successfully.
	SyncedAt time.Time
}

// Freshness returns per-account import freshness for planID's live
// accounts, ordered by name.
func (s *Syncer) Freshness(ctx context.Context, planID string) ([]AccountFreshness, error) {
	st, _, err := s.db.GetSyncState(ctx, planID)
	if err != nil {
		return nil, fmt.Errorf("sync: freshness: %w", err)
	}
	rows, err := s.db.Reader().QueryContext(ctx, `
		SELECT a.id, a.name, a.closed, a.direct_import_linked, a.direct_import_in_error,
			MAX(t.date), MAX(t.first_seen_at)
		FROM accounts a
		LEFT JOIN transactions t ON t.plan_id = a.plan_id AND t.account_id = a.id
			AND t.import_id IS NOT NULL AND t.deleted = 0
		WHERE a.plan_id = ? AND a.deleted = 0
		GROUP BY a.id
		ORDER BY a.name, a.id`, planID)
	if err != nil {
		return nil, fmt.Errorf("sync: freshness: %w", err)
	}
	defer rows.Close()
	var out []AccountFreshness
	for rows.Next() {
		f := AccountFreshness{SyncedAt: st.SyncedAt}
		var newest, seen sql.NullString
		if err := rows.Scan(&f.AccountID, &f.Name, &f.Closed, &f.Linked, &f.InError, &newest, &seen); err != nil {
			return nil, fmt.Errorf("sync: freshness: %w", err)
		}
		f.NewestImportDate = newest.String
		if seen.Valid {
			t, err := time.Parse(time.RFC3339Nano, seen.String)
			if err != nil {
				return nil, fmt.Errorf("sync: freshness: parse first_seen_at: %w", err)
			}
			f.LastImportSeenAt = &t
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DiscretionaryName is the category that marks a plan as labelled history
// (R8c): plans that have it used the current category scheme.
const DiscretionaryName = "Discretionary"

// DiscretionaryPlans lists every plan the token can reach and returns those
// with a live category named "Discretionary" (case-insensitive). It costs one
// GET for the list plus one per plan.
func DiscretionaryPlans(ctx context.Context, lister PlanLister, client ClientFunc) ([]ynab.PlanSummary, error) {
	plans, err := lister.Plans(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: list plans: %w", err)
	}
	var out []ynab.PlanSummary
	for _, p := range plans {
		groups, err := client(p.ID).GetCategories(ctx)
		if err != nil {
			return nil, fmt.Errorf("sync: plan categories: %w", err)
		}
		if hasDiscretionary(groups) {
			out = append(out, p)
		}
	}
	return out, nil
}

func hasDiscretionary(groups []*ynab.CategoryGroup) bool {
	for _, g := range groups {
		if g.Deleted {
			continue
		}
		for _, c := range g.Categories {
			if !c.Deleted && strings.EqualFold(strings.TrimSpace(c.Name), DiscretionaryName) {
				return true
			}
		}
	}
	return false
}
