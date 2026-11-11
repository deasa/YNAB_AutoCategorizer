package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Reads for the dry-run report. Answered dry_run_check questions come from
// AnsweredQuestions.

// DecisionsBetween returns planID's decisions created in [from, to), superseded
// ones included, in insertion order.
func (d *DB) DecisionsBetween(ctx context.Context, planID string, from, to time.Time) ([]Decision, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT `+decisionColumns+`
FROM decisions WHERE plan_id = ? AND created_at >= ? AND created_at < ? ORDER BY id`,
		planID, formatTime(from), formatTime(to))
	if err != nil {
		return nil, fmt.Errorf("store: decisions between: %w", err)
	}
	defer rows.Close()
	var out []Decision
	for rows.Next() {
		dec, err := scanDecision(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan decision: %w", err)
		}
		out = append(out, dec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: decisions between: %w", err)
	}
	return out, nil
}

// CountWriteLog returns how many write_log rows of planID, of any status, were
// created in [from, to). A dry run expects zero.
func (d *DB) CountWriteLog(ctx context.Context, planID string, from, to time.Time) (int, error) {
	var n int
	err := d.r.QueryRowContext(ctx, `
SELECT count(*) FROM write_log WHERE plan_id = ? AND created_at >= ? AND created_at < ?`,
		planID, formatTime(from), formatTime(to)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count write log: %w", err)
	}
	return n, nil
}

// HumanCategory is a transaction's category as it stands in the mirror, which
// in a dry run only a person can have set.
type HumanCategory struct {
	CategoryID   *string // nil when uncategorized; the sentinel for a split
	CategoryName *string // nil when uncategorized, and for a split
	IsSplit      bool
	Deleted      bool
	Locked       bool // a human lock holds the category field
	FirstSeenAt  time.Time
}

// CurrentHumanCategory returns the transaction's current category and when the
// mirror first saw it. ok is false if the mirror doesn't have it.
func (d *DB) CurrentHumanCategory(ctx context.Context, planID, txnID string) (h HumanCategory, ok bool, err error) {
	t, ok, err := d.TransactionByID(ctx, planID, txnID)
	if err != nil || !ok {
		return HumanCategory{}, ok, err
	}
	err = d.r.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM human_locks WHERE plan_id = ? AND txn_id = ? AND field = ?)`,
		planID, txnID, LockFieldCategory).Scan(&h.Locked)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return HumanCategory{}, false, fmt.Errorf("store: human category lock: %w", err)
	}
	h.CategoryID, h.CategoryName, h.IsSplit, h.Deleted, h.FirstSeenAt =
		t.CategoryID, t.CategoryName, t.IsSplit, t.Deleted, t.FirstSeenAt
	return h, true, nil
}
