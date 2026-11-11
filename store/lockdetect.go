package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// LockCandidate is one live transaction as human-lock detection sees it
// (architecture §3.1 step 2, R6b): its current fields and what write_log says
// about it.
type LockCandidate struct {
	TxnID string
	// Fields holds the current value of every field a write's after_json may
	// name, keyed by YNAB's JSON field name (category_id, approved, memo, ...),
	// as the JSON types YNAB uses (string, bool, int64, or nil for null).
	Fields map[string]any
	// Approved is Fields["approved"].
	Approved bool
	// RealCategory is true when the row's category is a person's choice: a
	// category in the mirror that is not internal (Uncategorized) or is Ready to
	// Assign. A split parent's sentinel and a missing category are not.
	RealCategory bool
	// OpenIntent is true when a write to the row is still an unresolved intent.
	OpenIntent bool
	// LastDone is the after_json of the row's most recent done write, nil if it
	// has none.
	LastDone json.RawMessage
}

// LockCandidates returns the live (not deleted) transactions of planID among
// txnIDs, or every live transaction when txnIDs is nil. It reads through the
// write transaction, so it sees rows upserted earlier in the same Update.
func (t *Tx) LockCandidates(planID string, txnIDs []string) ([]LockCandidate, error) {
	var ids *string
	if txnIDs != nil {
		b, err := json.Marshal(txnIDs)
		if err != nil {
			return nil, fmt.Errorf("store: lock candidates: %w", err)
		}
		s := string(b)
		ids = &s
	}
	// json_each, because a delta can outgrow SQLite's bound-variable limit.
	rows, err := t.c.QueryContext(t.ctx, `
SELECT t.id, t.date, t.amount, t.memo, t.cleared, t.approved, t.flag_color, t.account_id, t.payee_id, t.category_id,
       EXISTS (SELECT 1 FROM categories c
               WHERE c.plan_id = t.plan_id AND c.id = t.category_id AND (c.internal = 0 OR c.name = ?)),
       EXISTS (SELECT 1 FROM write_log w
               WHERE w.plan_id = t.plan_id AND w.txn_id = t.id AND w.status = 'intent'),
       (SELECT w.after_json FROM write_log w
        WHERE w.plan_id = t.plan_id AND w.txn_id = t.id AND w.status = 'done' ORDER BY w.id DESC LIMIT 1)
FROM transactions t
WHERE t.plan_id = ? AND t.deleted = 0
  AND (? IS NULL OR t.id IN (SELECT value FROM json_each(?)))
ORDER BY t.id`, readyToAssignName, planID, ids, ids)
	if err != nil {
		return nil, fmt.Errorf("store: lock candidates: %w", err)
	}
	defer rows.Close()
	var out []LockCandidate
	for rows.Next() {
		var (
			c                                   LockCandidate
			date, cleared, account              string
			amount                              int64
			memo, flag, payee, category, lastDn *string
		)
		if err := rows.Scan(&c.TxnID, &date, &amount, &memo, &cleared, &c.Approved, &flag, &account, &payee, &category,
			&c.RealCategory, &c.OpenIntent, &lastDn); err != nil {
			return nil, fmt.Errorf("store: scan lock candidate: %w", err)
		}
		c.Fields = map[string]any{
			"date": date, "amount": amount, "memo": nullable(memo), "cleared": cleared, "approved": c.Approved,
			"flag_color": nullable(flag), "account_id": account, "payee_id": nullable(payee),
			"category_id": nullable(category),
		}
		if lastDn != nil {
			c.LastDone = json.RawMessage(*lastDn)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: lock candidates: %w", err)
	}
	return out, nil
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// LockBackfilled reports whether planID's one-time lock backfill has committed.
func (t *Tx) LockBackfilled(planID string) (bool, error) {
	var one int
	err := t.c.QueryRowContext(t.ctx, `SELECT 1 FROM lock_detection WHERE plan_id = ?`, planID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: lock backfilled: %w", err)
	}
	return true, nil
}

// MarkLockBackfilled records that planID's lock backfill ran. It commits or
// rolls back with the rest of the transaction.
func (t *Tx) MarkLockBackfilled(planID string, at time.Time) error {
	_, err := t.c.ExecContext(t.ctx, `
INSERT INTO lock_detection (plan_id, backfilled_at) VALUES (?, ?)
ON CONFLICT (plan_id) DO NOTHING`, planID, formatTime(at))
	if err != nil {
		return fmt.Errorf("store: mark lock backfilled: %w", err)
	}
	return nil
}
