package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CategoryWithGroup is a category and the group it sits in.
type CategoryWithGroup struct {
	ID           string
	Name         string
	GroupID      string
	GroupName    string // "" when the group isn't in the mirror
	Note         *string
	Hidden       bool
	Deleted      bool
	GroupHidden  bool
	GroupDeleted bool // also true when the group isn't in the mirror: full exports omit deleted groups
}

// CategoriesWithGroups returns every category of planID, target or not, hidden
// and deleted ones included, by group and name.
func (d *DB) CategoriesWithGroups(ctx context.Context, planID string) ([]CategoryWithGroup, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT c.id, c.name, c.category_group_id, COALESCE(g.name, ''), c.note, c.hidden, c.deleted,
       COALESCE(g.hidden, 0), COALESCE(g.deleted, 1)
FROM categories c
LEFT JOIN category_groups g ON g.plan_id = c.plan_id AND g.id = c.category_group_id
WHERE c.plan_id = ?
ORDER BY lower(COALESCE(g.name, '')), lower(c.name), c.id`, planID)
	if err != nil {
		return nil, fmt.Errorf("store: categories with groups: %w", err)
	}
	defer rows.Close()
	var out []CategoryWithGroup
	for rows.Next() {
		var c CategoryWithGroup
		if err := rows.Scan(&c.ID, &c.Name, &c.GroupID, &c.GroupName, &c.Note, &c.Hidden, &c.Deleted,
			&c.GroupHidden, &c.GroupDeleted); err != nil {
			return nil, fmt.Errorf("store: scan category: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: categories with groups: %w", err)
	}
	return out, nil
}

// TransactionDetail is a mirrored transaction with its names resolved.
type TransactionDetail struct {
	Transaction
	AccountName  *string
	PayeeName    *string
	CategoryName *string // nil when uncategorized, and for a split's sentinel category
	IsSplit      bool    // has at least one live subtransaction
	FirstSeenAt  time.Time
}

const transactionDetailQuery = `
SELECT t.id, t.date, t.amount, t.memo, t.cleared, t.approved, t.flag_color, t.flag_name, t.account_id, t.payee_id,
       t.category_id, t.transfer_account_id, t.transfer_transaction_id, t.matched_transaction_id, t.import_id,
       t.import_payee_name, t.import_payee_name_original, t.debt_transaction_type, t.deleted, t.first_seen_at,
       a.name, p.name, c.name,
       EXISTS (SELECT 1 FROM subtransactions s
               WHERE s.plan_id = t.plan_id AND s.transaction_id = t.id AND s.deleted = 0)
FROM transactions t
LEFT JOIN accounts a   ON a.plan_id = t.plan_id AND a.id = t.account_id
LEFT JOIN payees p     ON p.plan_id = t.plan_id AND p.id = t.payee_id
LEFT JOIN categories c ON c.plan_id = t.plan_id AND c.id = t.category_id
`

func scanTransactionDetail(scan func(...any) error) (TransactionDetail, error) {
	var (
		r         TransactionDetail
		firstSeen string
	)
	if err := scan(&r.ID, &r.Date, &r.Amount, &r.Memo, &r.Cleared, &r.Approved, &r.FlagColor, &r.FlagName, &r.AccountID, &r.PayeeID,
		&r.CategoryID, &r.TransferAccountID, &r.TransferTransactionID, &r.MatchedTransactionID, &r.ImportID,
		&r.ImportPayeeName, &r.ImportPayeeNameOriginal, &r.DebtTransactionType, &r.Deleted, &firstSeen,
		&r.AccountName, &r.PayeeName, &r.CategoryName, &r.IsSplit); err != nil {
		return TransactionDetail{}, err
	}
	at, err := time.Parse(timeLayout, firstSeen)
	if err != nil {
		return TransactionDetail{}, fmt.Errorf("parse first_seen_at: %w", err)
	}
	r.FirstSeenAt = at
	return r, nil
}

// TransactionByID returns one transaction, deleted or not. ok is false if the
// mirror doesn't have it.
func (d *DB) TransactionByID(ctx context.Context, planID, txnID string) (r TransactionDetail, ok bool, err error) {
	r, err = scanTransactionDetail(d.r.QueryRowContext(ctx, transactionDetailQuery+`
WHERE t.plan_id = ? AND t.id = ?`, planID, txnID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return TransactionDetail{}, false, nil
	}
	if err != nil {
		return TransactionDetail{}, false, fmt.Errorf("store: transaction by id: %w", err)
	}
	return r, true, nil
}

// EligibleTransactions returns planID's live transactions dated on or after
// since ("YYYY-MM-DD") that are uncategorized or unapproved, newest first, at
// most limit of them (limit <= 0 means all). YNAB can mark a transaction
// uncategorized with no category or with its internal Uncategorized category.
func (d *DB) EligibleTransactions(ctx context.Context, planID, since string, limit int) ([]TransactionDetail, error) {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	rows, err := d.r.QueryContext(ctx, transactionDetailQuery+`
WHERE t.plan_id = ? AND t.deleted = 0 AND t.date >= ?
  AND (t.approved = 0 OR t.category_id IS NULL OR (c.internal = 1 AND c.name = 'Uncategorized'))
ORDER BY t.date DESC, t.id
LIMIT ?`, planID, since, limit)
	if err != nil {
		return nil, fmt.Errorf("store: eligible transactions: %w", err)
	}
	defer rows.Close()
	var out []TransactionDetail
	for rows.Next() {
		r, err := scanTransactionDetail(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan transaction: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: eligible transactions: %w", err)
	}
	return out, nil
}
