package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Transaction reads work on lines: an unsplit transaction is one line, and a
// split is one line per live subtransaction, never the parent with its "Split"
// sentinel category (architecture.md §2). So every category read and total
// sees each dollar exactly once, under the category it was really assigned to.
// Deleted transactions and subtransactions are never lines.
//
// A transaction counts as a split while it has at least one live
// subtransaction. linesCTE takes the plan id as its one parameter.
const linesCTE = `
WITH lines AS (
    SELECT t.id AS transaction_id, '' AS sub_id, t.date, t.amount, t.memo,
           t.account_id, t.payee_id, t.category_id, t.transfer_account_id,
           t.approved, t.cleared, t.import_payee_name, t.flag_name
    FROM transactions t
    WHERE t.plan_id = ?1 AND t.deleted = 0
      AND NOT EXISTS (SELECT 1 FROM subtransactions s
                      WHERE s.plan_id = t.plan_id AND s.transaction_id = t.id AND s.deleted = 0)
    UNION ALL
    SELECT t.id, s.id, t.date, s.amount, COALESCE(s.memo, t.memo),
           t.account_id, COALESCE(s.payee_id, t.payee_id), s.category_id, s.transfer_account_id,
           t.approved, t.cleared, t.import_payee_name, t.flag_name
    FROM subtransactions s
    JOIN transactions t ON t.plan_id = s.plan_id AND t.id = s.transaction_id
    WHERE s.plan_id = ?1 AND s.deleted = 0 AND t.deleted = 0
)
`

// linesJoins names each line's account, payee, category and group.
const linesJoins = `
FROM lines l
LEFT JOIN accounts a        ON a.plan_id = ?1 AND a.id = l.account_id
LEFT JOIN payees p          ON p.plan_id = ?1 AND p.id = l.payee_id
LEFT JOIN categories c      ON c.plan_id = ?1 AND c.id = l.category_id
LEFT JOIN category_groups g ON g.plan_id = ?1 AND g.id = c.category_group_id
`

// Line is one categorized piece of a transaction.
type Line struct {
	TransactionID    string
	SubtransactionID string // "" unless the line is part of a split
	Date             string
	Amount           int64 // milliunits; negative is an outflow
	Memo             *string
	AccountID        string
	AccountName      *string
	PayeeID          *string
	PayeeName        *string
	ImportPayeeName  *string
	CategoryID       *string
	CategoryName     *string
	CategoryGroup    *string
	TransferAccount  *string // id of the other account, for a transfer
	Approved         bool
	Cleared          string
	FlagName         *string
}

// LineKey is a line's position in QueryLines order: newest date first, then
// transaction id, then subtransaction id.
type LineKey struct {
	Date             string `json:"d"`
	TransactionID    string `json:"t"`
	SubtransactionID string `json:"s"`
}

// Key returns the line's paging key.
func (l Line) Key() LineKey {
	return LineKey{Date: l.Date, TransactionID: l.TransactionID, SubtransactionID: l.SubtransactionID}
}

// LineFilter selects lines. Zero fields don't filter.
type LineFilter struct {
	Start, End string // inclusive "YYYY-MM-DD"
	Account    string // an account id, or its name (any case)
	Category   string // a category id, or its name (any case)
	PayeeText  string // a substring of the payee or imported payee name (any case)
	MinAmount  *int64 // milliunits, signed, inclusive
	MaxAmount  *int64
	Approved   *bool
}

// where returns the filter's SQL conditions over linesJoins and their args.
func (f LineFilter) where() (string, []any) {
	conds := []string{"1=1"}
	var args []any
	add := func(cond string, a ...any) { conds = append(conds, cond); args = append(args, a...) }
	if f.Start != "" {
		add("l.date >= ?", f.Start)
	}
	if f.End != "" {
		add("l.date <= ?", f.End)
	}
	if f.Account != "" {
		add("(l.account_id = ? OR lower(a.name) = lower(?))", f.Account, f.Account)
	}
	if f.Category != "" {
		add("(l.category_id = ? OR lower(c.name) = lower(?))", f.Category, f.Category)
	}
	if f.PayeeText != "" {
		pat := "%" + escapeLike(strings.ToLower(f.PayeeText)) + "%"
		add(`(lower(p.name) LIKE ? ESCAPE '\' OR lower(l.import_payee_name) LIKE ? ESCAPE '\')`, pat, pat)
	}
	if f.MinAmount != nil {
		add("l.amount >= ?", *f.MinAmount)
	}
	if f.MaxAmount != nil {
		add("l.amount <= ?", *f.MaxAmount)
	}
	if f.Approved != nil {
		add("l.approved = ?", *f.Approved)
	}
	return strings.Join(conds, " AND "), args
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// LineSummary totals every line a filter matches, not just one page.
type LineSummary struct {
	Count int
	Total int64 // milliunits
}

// QueryLines returns up to limit lines matching f that come after the key
// after (nil starts at the top), plus a summary of all of f's lines.
func (d *DB) QueryLines(ctx context.Context, planID string, f LineFilter, after *LineKey, limit int) ([]Line, LineSummary, error) {
	where, args := f.where()
	all := append([]any{planID}, args...)

	var sum LineSummary
	err := d.r.QueryRowContext(ctx, linesCTE+`SELECT count(*), COALESCE(sum(l.amount), 0)`+linesJoins+`WHERE `+where, all...).
		Scan(&sum.Count, &sum.Total)
	if err != nil {
		return nil, LineSummary{}, fmt.Errorf("store: summarize lines: %w", err)
	}

	page, pageArgs := where, all
	if after != nil {
		page += ` AND (l.date < ? OR (l.date = ? AND (l.transaction_id > ? OR (l.transaction_id = ? AND l.sub_id > ?))))`
		pageArgs = append(append([]any{}, all...), after.Date, after.Date,
			after.TransactionID, after.TransactionID, after.SubtransactionID)
	}
	pageArgs = append(pageArgs, limit)
	rows, err := d.r.QueryContext(ctx, linesCTE+`
SELECT l.transaction_id, l.sub_id, l.date, l.amount, l.memo, l.account_id, a.name,
       l.payee_id, p.name, l.import_payee_name, l.category_id, c.name, g.name,
       l.transfer_account_id, l.approved, l.cleared, l.flag_name`+linesJoins+`
WHERE `+page+`
ORDER BY l.date DESC, l.transaction_id, l.sub_id
LIMIT ?`, pageArgs...)
	if err != nil {
		return nil, LineSummary{}, fmt.Errorf("store: query lines: %w", err)
	}
	defer rows.Close()
	lines := []Line{}
	for rows.Next() {
		var l Line
		if err := rows.Scan(&l.TransactionID, &l.SubtransactionID, &l.Date, &l.Amount, &l.Memo, &l.AccountID, &l.AccountName,
			&l.PayeeID, &l.PayeeName, &l.ImportPayeeName, &l.CategoryID, &l.CategoryName, &l.CategoryGroup,
			&l.TransferAccount, &l.Approved, &l.Cleared, &l.FlagName); err != nil {
			return nil, LineSummary{}, fmt.Errorf("store: scan line: %w", err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, LineSummary{}, fmt.Errorf("store: query lines: %w", err)
	}
	return lines, sum, nil
}

// CategoryTotal is one category's net amount over a date range.
type CategoryTotal struct {
	CategoryID    string // "" for uncategorized lines
	CategoryName  string
	CategoryGroup string
	Total         int64 // milliunits; negative is net spending
	Count         int
}

// CategoryTotals sums on-budget lines per category for dates start..end,
// inclusive. Refunds net against their category. Transfers between on-budget
// accounts (no category), lines in off-budget accounts, and income to an
// internal "Inflow:" category are not spending, so they're left out.
func (d *DB) CategoryTotals(ctx context.Context, planID, start, end string) ([]CategoryTotal, error) {
	rows, err := d.r.QueryContext(ctx, linesCTE+`
SELECT COALESCE(l.category_id, ''), COALESCE(c.name, ''), COALESCE(g.name, ''), sum(l.amount), count(*)`+linesJoins+`
WHERE l.date >= ?2 AND l.date <= ?3
  AND a.on_budget = 1
  AND NOT (l.category_id IS NULL AND l.transfer_account_id IS NOT NULL)
  AND NOT (COALESCE(c.internal, 0) = 1 AND c.name LIKE 'Inflow:%')
GROUP BY 1, 2, 3
ORDER BY 3, 2, 1`, planID, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: category totals: %w", err)
	}
	defer rows.Close()
	var out []CategoryTotal
	for rows.Next() {
		var c CategoryTotal
		if err := rows.Scan(&c.CategoryID, &c.CategoryName, &c.CategoryGroup, &c.Total, &c.Count); err != nil {
			return nil, fmt.Errorf("store: scan category total: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: category totals: %w", err)
	}
	return out, nil
}

// NameMatches reports whether planID has an account (table "accounts") or a
// category ("categories") whose id is s or whose name is s in any case.
// Deleted rows count, so old transactions stay findable.
func (d *DB) NameMatches(ctx context.Context, planID, table, s string) (bool, error) {
	if table != "accounts" && table != "categories" {
		return false, fmt.Errorf("store: name match on %q", table)
	}
	var n int
	err := d.r.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE plan_id = ? AND (id = ? OR lower(name) = lower(?))`,
		planID, s, s).Scan(&n)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("store: match %s: %w", table, err)
	}
	return n > 0, nil
}
