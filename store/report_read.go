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

// HumanCategory is a transaction's category as it stands in the mirror, and
// whether a person set it.
type HumanCategory struct {
	HumanSet     bool    // approved or locked, and a split or a non-internal category
	CategoryID   *string // nil unless HumanSet and not a split
	CategoryName *string // nil unless HumanSet and not a split, or the category isn't in the mirror
	IsSplit      bool
	Approved     bool
	Deleted      bool
	Locked       bool       // a human lock holds the category field
	LockedAt     *time.Time // when that lock was made; nil if not locked
	FirstSeenAt  time.Time  // transactions.first_seen_at
	// SeenAt is the earliest time the mirror can show the label existed:
	// LockedAt if locked, else FirstSeenAt. A label judges a decision only if
	// SeenAt is after it. An unlocked label has no time of its own, so it falls
	// back to FirstSeenAt, which precedes any decision on the row: it never
	// judges one (conservative).
	SeenAt time.Time
}

// CurrentHumanCategory returns the transaction's current category, whether a
// person set it, and when the mirror first saw that label. An unapproved row's
// category (YNAB's payee-memory guess) and an internal category (Uncategorized,
// Ready to Assign) are not human-set unless a human lock holds the field. ok
// is false if the mirror doesn't have the transaction.
func (d *DB) CurrentHumanCategory(ctx context.Context, planID, txnID string) (h HumanCategory, ok bool, err error) {
	var (
		catID, catName *string
		internal       bool
		firstSeen      string
		lockedAt       *string
	)
	err = d.r.QueryRowContext(ctx, `
SELECT t.category_id, c.name, COALESCE(c.internal, 0), t.approved, t.deleted, t.first_seen_at,
       EXISTS (SELECT 1 FROM subtransactions s
               WHERE s.plan_id = t.plan_id AND s.transaction_id = t.id AND s.deleted = 0),
       hl.created_at
FROM transactions t
LEFT JOIN categories c   ON c.plan_id = t.plan_id AND c.id = t.category_id
LEFT JOIN human_locks hl ON hl.plan_id = t.plan_id AND hl.txn_id = t.id AND hl.field = ?
WHERE t.plan_id = ? AND t.id = ?`, LockFieldCategory, planID, txnID).
		Scan(&catID, &catName, &internal, &h.Approved, &h.Deleted, &firstSeen, &h.IsSplit, &lockedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return HumanCategory{}, false, nil
	}
	if err != nil {
		return HumanCategory{}, false, fmt.Errorf("store: human category: %w", err)
	}
	if h.FirstSeenAt, err = time.Parse(timeLayout, firstSeen); err != nil {
		return HumanCategory{}, false, fmt.Errorf("store: parse first_seen_at: %w", err)
	}
	h.SeenAt = h.FirstSeenAt
	if lockedAt != nil {
		at, err := time.Parse(timeLayout, *lockedAt)
		if err != nil {
			return HumanCategory{}, false, fmt.Errorf("store: parse lock created_at: %w", err)
		}
		h.Locked, h.LockedAt, h.SeenAt = true, &at, at
	}
	categorized := h.IsSplit || (catID != nil && !internal)
	h.HumanSet = categorized && (h.Approved || h.Locked)
	if h.HumanSet && !h.IsSplit {
		h.CategoryID, h.CategoryName = catID, catName
	}
	return h, true, nil
}

// ReportPlans returns every plan id that has a decision, a question or a
// write_log row, sorted, so the dry-run report needs no configured plan.
func (d *DB) ReportPlans(ctx context.Context) ([]string, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT plan_id FROM decisions UNION SELECT plan_id FROM questions UNION SELECT plan_id FROM write_log
ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("store: report plans: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: scan report plan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: report plans: %w", err)
	}
	return out, nil
}
