package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Reads behind the financial snapshot, goals and income tools (R9a), and the
// tag-account command. None of them write.

// AccountSummary is one live (not deleted) account with its local tax tag.
type AccountSummary struct {
	ID        string
	Name      string
	Type      string
	OnBudget  bool
	Closed    bool
	Balance   int64   // milliunits
	TaxBucket *string // nil while untagged
	// Loan terms, keyed by effective date "YYYY-MM-DD"; interest rates are
	// percent × 1000. Empty for an account without them.
	DebtOriginalBalance *int64
	DebtInterestRates   map[string]int64
	DebtMinimumPayments map[string]int64
	DebtEscrowAmounts   map[string]int64
}

// Accounts returns planID's live accounts, closed ones included, by name.
func (d *DB) Accounts(ctx context.Context, planID string) ([]AccountSummary, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, name, type, on_budget, closed, balance, tax_bucket, debt_original_balance,
       debt_interest_rates, debt_minimum_payments, debt_escrow_amounts
FROM accounts WHERE plan_id = ? AND deleted = 0
ORDER BY lower(name), id`, planID)
	if err != nil {
		return nil, fmt.Errorf("store: accounts: %w", err)
	}
	defer rows.Close()
	var out []AccountSummary
	for rows.Next() {
		var a AccountSummary
		var rates, mins, escrow sql.NullString
		if err := rows.Scan(&a.ID, &a.Name, &a.Type, &a.OnBudget, &a.Closed, &a.Balance, &a.TaxBucket,
			&a.DebtOriginalBalance, &rates, &mins, &escrow); err != nil {
			return nil, fmt.Errorf("store: scan account: %w", err)
		}
		for _, p := range []struct {
			s   sql.NullString
			dst *map[string]int64
		}{{rates, &a.DebtInterestRates}, {mins, &a.DebtMinimumPayments}, {escrow, &a.DebtEscrowAmounts}} {
			if p.s.Valid {
				if err := json.Unmarshal([]byte(p.s.String), p.dst); err != nil {
					return nil, fmt.Errorf("store: account loan terms: %w", err)
				}
			}
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: accounts: %w", err)
	}
	return out, nil
}

// MonthFlow is one calendar month's on-budget money flow.
type MonthFlow struct {
	Month         string // "YYYY-MM"
	Income        int64  // net inflow to an internal "Inflow:" category
	Categorized   int64  // net of categorized lines; negative is net spending
	Uncategorized int64  // net of lines with no category that aren't transfers
	UncatCount    int
}

// MonthlyFlows sums on-budget lines per month for dates start..end, inclusive,
// skipping months with no such lines. The split into income, categorized and
// uncategorized follows CategoryTotals: on-budget transfers, off-budget
// accounts and transfers in from off-budget accounts are neither income nor
// spending.
func (d *DB) MonthlyFlows(ctx context.Context, planID, start, end string) ([]MonthFlow, error) {
	rows, err := d.r.QueryContext(ctx, linesCTE+`, kinds AS (
    SELECT substr(l.date, 1, 7) AS month, l.amount,
           CASE WHEN COALESCE(c.internal, 0) = 1 AND c.name LIKE 'Inflow:%' THEN 'i'
                WHEN l.category_id IS NULL THEN 'u'
                ELSE 'c' END AS kind`+linesJoins+`
    WHERE l.date >= ?2 AND l.date <= ?3
      AND a.on_budget = 1
      AND NOT (l.category_id IS NULL AND l.transfer_account_id IS NOT NULL)
      -- Money in from an off-budget account is categorized to Ready to Assign,
      -- but it's moved savings, not income.
      AND NOT (l.transfer_account_id IS NOT NULL AND COALESCE(c.internal, 0) = 1 AND c.name LIKE 'Inflow:%')
)
SELECT month,
       COALESCE(sum(CASE WHEN kind = 'i' THEN amount END), 0),
       COALESCE(sum(CASE WHEN kind = 'c' THEN amount END), 0),
       COALESCE(sum(CASE WHEN kind = 'u' THEN amount END), 0),
       count(CASE WHEN kind = 'u' THEN 1 END)
FROM kinds GROUP BY month ORDER BY month`, planID, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: monthly flows: %w", err)
	}
	defer rows.Close()
	var out []MonthFlow
	for rows.Next() {
		var m MonthFlow
		if err := rows.Scan(&m.Month, &m.Income, &m.Categorized, &m.Uncategorized, &m.UncatCount); err != nil {
			return nil, fmt.Errorf("store: scan month flow: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: monthly flows: %w", err)
	}
	return out, nil
}

// HistoryRange returns the first and last dates of planID's live transactions,
// or ok false when it has none.
func (d *DB) HistoryRange(ctx context.Context, planID string) (first, last string, ok bool, err error) {
	var f, l sql.NullString
	err = d.r.QueryRowContext(ctx, `SELECT min(date), max(date) FROM transactions WHERE plan_id = ? AND deleted = 0`,
		planID).Scan(&f, &l)
	if err != nil {
		return "", "", false, fmt.Errorf("store: history range: %w", err)
	}
	return f.String, l.String, f.Valid, nil
}

// Goal is a live category that has a target.
type Goal struct {
	CategoryID       string
	Category         string
	CategoryGroup    string // "" when the group isn't in the mirror
	Hidden           bool
	Balance          int64
	Budgeted         int64 // this month
	GoalType         string
	Target           *int64
	TargetMonth      *string
	TargetDate       *string
	PercentComplete  *int64
	MonthsToBudget   *int64
	UnderFunded      *int64
	OverallFunded    *int64
	OverallLeft      *int64
	Snoozed          bool
	NeedsWholeAmount *bool
	Cadence          *int64
	CadenceFrequency *int64
}

// Goals returns planID's live categories that have a target, by group and name.
func (d *DB) Goals(ctx context.Context, planID string) ([]Goal, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT c.id, c.name, COALESCE(g.name, ''), c.hidden, c.balance, c.budgeted, c.goal_type, c.goal_target,
       c.goal_target_month, c.goal_target_date, c.goal_percentage_complete, c.goal_months_to_budget,
       c.goal_under_funded, c.goal_overall_funded, c.goal_overall_left, c.goal_snoozed_at IS NOT NULL,
       c.goal_needs_whole_amount, c.goal_cadence, c.goal_cadence_frequency
FROM categories c
LEFT JOIN category_groups g ON g.plan_id = c.plan_id AND g.id = c.category_group_id
WHERE c.plan_id = ? AND c.deleted = 0 AND c.goal_type IS NOT NULL AND c.goal_type != ''
ORDER BY lower(COALESCE(g.name, '')), lower(c.name), c.id`, planID)
	if err != nil {
		return nil, fmt.Errorf("store: goals: %w", err)
	}
	defer rows.Close()
	var out []Goal
	for rows.Next() {
		var g Goal
		if err := rows.Scan(&g.CategoryID, &g.Category, &g.CategoryGroup, &g.Hidden, &g.Balance, &g.Budgeted, &g.GoalType,
			&g.Target, &g.TargetMonth, &g.TargetDate, &g.PercentComplete, &g.MonthsToBudget, &g.UnderFunded,
			&g.OverallFunded, &g.OverallLeft, &g.Snoozed, &g.NeedsWholeAmount, &g.Cadence, &g.CadenceFrequency); err != nil {
			return nil, fmt.Errorf("store: scan goal: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: goals: %w", err)
	}
	return out, nil
}

// IncomeLine is one inflow to Ready to Assign.
type IncomeLine struct {
	TransactionID    string
	SubtransactionID string
	Date             string
	Amount           int64
	Account          string
	Payee            *string
	ImportPayee      *string
	Memo             *string
}

// IncomeLines returns the positive lines categorized to an internal "Inflow:"
// category for dates start..end, inclusive (either may be ""), newest first.
// Transfers are left out even when YNAB categorizes them as inflow (money in
// from an off-budget account), and so are deleted rows; a refund is under its
// spending category, so it's never one of these.
func (d *DB) IncomeLines(ctx context.Context, planID, start, end string) ([]IncomeLine, error) {
	rows, err := d.r.QueryContext(ctx, linesCTE+`
SELECT l.transaction_id, l.sub_id, l.date, l.amount, COALESCE(a.name, ''), p.name, l.import_payee_name, l.memo`+linesJoins+`
WHERE (?2 = '' OR l.date >= ?2) AND (?3 = '' OR l.date <= ?3)
  AND l.amount > 0
  AND COALESCE(c.internal, 0) = 1 AND c.name LIKE 'Inflow:%'
  AND l.transfer_account_id IS NULL
  AND COALESCE(p.transfer_account_id, '') = ''
ORDER BY l.date DESC, l.transaction_id, l.sub_id`, planID, start, end)
	if err != nil {
		return nil, fmt.Errorf("store: income lines: %w", err)
	}
	defer rows.Close()
	var out []IncomeLine
	for rows.Next() {
		var l IncomeLine
		if err := rows.Scan(&l.TransactionID, &l.SubtransactionID, &l.Date, &l.Amount, &l.Account, &l.Payee,
			&l.ImportPayee, &l.Memo); err != nil {
			return nil, fmt.Errorf("store: scan income line: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: income lines: %w", err)
	}
	return out, nil
}
