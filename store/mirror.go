package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Mirror row types. Field names follow YNAB's JSON; money is milliunits,
// dates are "YYYY-MM-DD", nil pointers are NULL. These types deliberately
// don't depend on the YNAB client library; sync maps library structs onto them.

type Account struct {
	ID                  string
	Name                string
	Type                string
	OnBudget            bool
	Closed              bool
	Note                *string
	Balance             int64
	ClearedBalance      int64
	UnclearedBalance    int64
	TransferPayeeID     *string
	DirectImportLinked  bool
	DirectImportInError bool
	LastReconciledAt    *time.Time
	DebtOriginalBalance *int64
	// Loan terms keyed by effective date "YYYY-MM-DD", values in milliunits
	// (interest rates are percent × 1000).
	DebtInterestRates   map[string]int64
	DebtMinimumPayments map[string]int64
	DebtEscrowAmounts   map[string]int64
	Deleted             bool
}

type CategoryGroup struct {
	ID       string
	Name     string
	Hidden   bool
	Internal bool
	Deleted  bool
}

type Category struct {
	ID                      string
	CategoryGroupID         string
	Name                    string
	Hidden                  bool
	Internal                bool
	OriginalCategoryGroupID *string
	Note                    *string
	Budgeted                int64
	Activity                int64
	Balance                 int64
	GoalType                *string
	GoalNeedsWholeAmount    *bool
	GoalDay                 *int64
	GoalCadence             *int64
	GoalCadenceFrequency    *int64
	GoalCreationMonth       *string
	GoalTarget              *int64
	GoalTargetMonth         *string
	GoalTargetDate          *string
	GoalPercentageComplete  *int64
	GoalMonthsToBudget      *int64
	GoalUnderFunded         *int64
	GoalOverallFunded       *int64
	GoalOverallLeft         *int64
	GoalSnoozedAt           *time.Time
	Deleted                 bool
}

type Payee struct {
	ID                string
	Name              string
	TransferAccountID *string
	Deleted           bool
}

// Transaction is a TransactionSummaryBase. For a split parent, CategoryID is
// YNAB's "Split" sentinel; the categories are on its Subtransactions.
type Transaction struct {
	ID                      string
	Date                    string
	Amount                  int64
	Memo                    *string
	Cleared                 string
	Approved                bool
	FlagColor               *string
	FlagName                *string
	AccountID               string
	PayeeID                 *string
	CategoryID              *string
	TransferAccountID       *string
	TransferTransactionID   *string
	MatchedTransactionID    *string
	ImportID                *string
	ImportPayeeName         *string
	ImportPayeeNameOriginal *string
	DebtTransactionType     *string
	Deleted                 bool
}

type Subtransaction struct {
	ID                    string
	TransactionID         string
	Amount                int64
	Memo                  *string
	PayeeID               *string
	CategoryID            *string
	TransferAccountID     *string
	TransferTransactionID *string
	Deleted               bool
}

type Month struct {
	Month        string // "YYYY-MM-01"
	Note         *string
	Income       int64
	Budgeted     int64
	Activity     int64
	ToBeBudgeted int64
	AgeOfMoney   *int64
	Deleted      bool
}

type MonthCategory struct {
	Month                  string
	CategoryID             string
	Budgeted               int64
	Activity               int64
	Balance                int64
	GoalTarget             *int64
	GoalUnderFunded        *int64
	GoalOverallFunded      *int64
	GoalOverallLeft        *int64
	GoalPercentageComplete *int64
	Deleted                bool
}

type ScheduledTransaction struct {
	ID                string
	DateFirst         string
	DateNext          string
	Frequency         string
	Amount            int64
	Memo              *string
	FlagColor         *string
	FlagName          *string
	AccountID         string
	PayeeID           *string
	CategoryID        *string
	TransferAccountID *string
	Deleted           bool
}

type ScheduledSubtransaction struct {
	ID                     string
	ScheduledTransactionID string
	Amount                 int64
	Memo                   *string
	PayeeID                *string
	CategoryID             *string
	TransferAccountID      *string
	Deleted                bool
}

// SyncState is the delta cursor for one plan.
type SyncState struct {
	PlanID          string
	ServerKnowledge int64
	SyncedAt        time.Time
}

// Tx is a write transaction on the writer connection. Deferred foreign keys
// are checked at commit, so upserts may run in any order within one Tx.
type Tx struct {
	ctx context.Context
	c   *sql.Conn
}

// Update runs fn in one write transaction, committing if fn returns nil and
// rolling back otherwise. A sync applies a whole delta plus its new
// sync_state in a single Update so the cursor never runs ahead of the data.
func (d *DB) Update(ctx context.Context, fn func(*Tx) error) error {
	return writeTx(ctx, d.w, func(c *sql.Conn) error { return fn(&Tx{ctx: ctx, c: c}) })
}

// upsert executes query once per row with a single prepared statement.
func upsert[T any](t *Tx, table, query string, rows []T, args func(T) []any) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := t.c.PrepareContext(t.ctx, query)
	if err != nil {
		return fmt.Errorf("store: prepare %s upsert: %w", table, err)
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.ExecContext(t.ctx, args(r)...); err != nil {
			return fmt.Errorf("store: upsert %s: %w", table, err)
		}
	}
	return nil
}

// UpsertAccounts inserts or updates accounts. The local tax_bucket column is
// never touched.
func (t *Tx) UpsertAccounts(planID string, rows []Account) error {
	return upsert(t, "accounts", `INSERT INTO accounts (
		plan_id, id, name, type, on_budget, closed, note, balance, cleared_balance,
		uncleared_balance, transfer_payee_id, direct_import_linked, direct_import_in_error,
		last_reconciled_at, debt_original_balance, debt_interest_rates, debt_minimum_payments,
		debt_escrow_amounts, deleted
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT (plan_id, id) DO UPDATE SET
		name = excluded.name, type = excluded.type, on_budget = excluded.on_budget,
		closed = excluded.closed, note = excluded.note, balance = excluded.balance,
		cleared_balance = excluded.cleared_balance, uncleared_balance = excluded.uncleared_balance,
		transfer_payee_id = excluded.transfer_payee_id,
		direct_import_linked = excluded.direct_import_linked,
		direct_import_in_error = excluded.direct_import_in_error,
		last_reconciled_at = excluded.last_reconciled_at,
		debt_original_balance = excluded.debt_original_balance,
		debt_interest_rates = excluded.debt_interest_rates,
		debt_minimum_payments = excluded.debt_minimum_payments,
		debt_escrow_amounts = excluded.debt_escrow_amounts, deleted = excluded.deleted`,
		rows, func(a Account) []any {
			return []any{planID, a.ID, a.Name, a.Type, a.OnBudget, a.Closed, a.Note, a.Balance,
				a.ClearedBalance, a.UnclearedBalance, a.TransferPayeeID, a.DirectImportLinked,
				a.DirectImportInError, formatTimePtr(a.LastReconciledAt), a.DebtOriginalBalance,
				periodicJSON(a.DebtInterestRates), periodicJSON(a.DebtMinimumPayments),
				periodicJSON(a.DebtEscrowAmounts), a.Deleted}
		})
}

// SetAccountTaxBucket sets the local tax_bucket tag (nil clears it).
func (t *Tx) SetAccountTaxBucket(planID, accountID string, bucket *string) error {
	res, err := t.c.ExecContext(t.ctx,
		`UPDATE accounts SET tax_bucket = ? WHERE plan_id = ? AND id = ?`, bucket, planID, accountID)
	if err != nil {
		return fmt.Errorf("store: set tax_bucket: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("store: set tax_bucket: account %s not found", accountID)
	}
	return nil
}

func (t *Tx) UpsertCategoryGroups(planID string, rows []CategoryGroup) error {
	return upsert(t, "category_groups", `INSERT INTO category_groups (
		plan_id, id, name, hidden, internal, deleted
	) VALUES (?,?,?,?,?,?)
	ON CONFLICT (plan_id, id) DO UPDATE SET
		name = excluded.name, hidden = excluded.hidden, internal = excluded.internal,
		deleted = excluded.deleted`,
		rows, func(g CategoryGroup) []any {
			return []any{planID, g.ID, g.Name, g.Hidden, g.Internal, g.Deleted}
		})
}

func (t *Tx) UpsertCategories(planID string, rows []Category) error {
	return upsert(t, "categories", `INSERT INTO categories (
		plan_id, id, category_group_id, name, hidden, internal, original_category_group_id,
		note, budgeted, activity, balance, goal_type, goal_needs_whole_amount, goal_day,
		goal_cadence, goal_cadence_frequency, goal_creation_month, goal_target,
		goal_target_month, goal_target_date, goal_percentage_complete, goal_months_to_budget,
		goal_under_funded, goal_overall_funded, goal_overall_left, goal_snoozed_at, deleted
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT (plan_id, id) DO UPDATE SET
		category_group_id = excluded.category_group_id, name = excluded.name,
		hidden = excluded.hidden, internal = excluded.internal,
		original_category_group_id = excluded.original_category_group_id, note = excluded.note,
		budgeted = excluded.budgeted, activity = excluded.activity, balance = excluded.balance,
		goal_type = excluded.goal_type, goal_needs_whole_amount = excluded.goal_needs_whole_amount,
		goal_day = excluded.goal_day, goal_cadence = excluded.goal_cadence,
		goal_cadence_frequency = excluded.goal_cadence_frequency,
		goal_creation_month = excluded.goal_creation_month, goal_target = excluded.goal_target,
		goal_target_month = excluded.goal_target_month, goal_target_date = excluded.goal_target_date,
		goal_percentage_complete = excluded.goal_percentage_complete,
		goal_months_to_budget = excluded.goal_months_to_budget,
		goal_under_funded = excluded.goal_under_funded,
		goal_overall_funded = excluded.goal_overall_funded,
		goal_overall_left = excluded.goal_overall_left, goal_snoozed_at = excluded.goal_snoozed_at,
		deleted = excluded.deleted`,
		rows, func(c Category) []any {
			return []any{planID, c.ID, c.CategoryGroupID, c.Name, c.Hidden, c.Internal,
				c.OriginalCategoryGroupID, c.Note, c.Budgeted, c.Activity, c.Balance, c.GoalType,
				c.GoalNeedsWholeAmount, c.GoalDay, c.GoalCadence, c.GoalCadenceFrequency,
				c.GoalCreationMonth, c.GoalTarget, c.GoalTargetMonth, c.GoalTargetDate,
				c.GoalPercentageComplete, c.GoalMonthsToBudget, c.GoalUnderFunded,
				c.GoalOverallFunded, c.GoalOverallLeft, formatTimePtr(c.GoalSnoozedAt), c.Deleted}
		})
}

func (t *Tx) UpsertPayees(planID string, rows []Payee) error {
	return upsert(t, "payees", `INSERT INTO payees (
		plan_id, id, name, transfer_account_id, deleted
	) VALUES (?,?,?,?,?)
	ON CONFLICT (plan_id, id) DO UPDATE SET
		name = excluded.name, transfer_account_id = excluded.transfer_account_id,
		deleted = excluded.deleted`,
		rows, func(p Payee) []any {
			return []any{planID, p.ID, p.Name, p.TransferAccountID, p.Deleted}
		})
}

// UpsertTransactions inserts or updates transactions. first_seen_at is set on
// insert, and reset when an existing row first gains an import_id.
func (t *Tx) UpsertTransactions(planID string, rows []Transaction) error {
	now := formatTime(time.Now())
	return upsert(t, "transactions", `INSERT INTO transactions (
		plan_id, id, date, amount, memo, cleared, approved, flag_color, flag_name, account_id,
		payee_id, category_id, transfer_account_id, transfer_transaction_id,
		matched_transaction_id, import_id, import_payee_name, import_payee_name_original,
		debt_transaction_type, deleted, first_seen_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT (plan_id, id) DO UPDATE SET
		date = excluded.date, amount = excluded.amount, memo = excluded.memo,
		cleared = excluded.cleared, approved = excluded.approved,
		flag_color = excluded.flag_color, flag_name = excluded.flag_name,
		account_id = excluded.account_id, payee_id = excluded.payee_id,
		category_id = excluded.category_id, transfer_account_id = excluded.transfer_account_id,
		transfer_transaction_id = excluded.transfer_transaction_id,
		matched_transaction_id = excluded.matched_transaction_id, import_id = excluded.import_id,
		import_payee_name = excluded.import_payee_name,
		import_payee_name_original = excluded.import_payee_name_original,
		debt_transaction_type = excluded.debt_transaction_type, deleted = excluded.deleted,
		first_seen_at = CASE
			WHEN transactions.import_id IS NULL AND excluded.import_id IS NOT NULL
			THEN excluded.first_seen_at ELSE transactions.first_seen_at END`,
		rows, func(x Transaction) []any {
			return []any{planID, x.ID, x.Date, x.Amount, x.Memo, x.Cleared, x.Approved, x.FlagColor,
				x.FlagName, x.AccountID, x.PayeeID, x.CategoryID, x.TransferAccountID,
				x.TransferTransactionID, x.MatchedTransactionID, x.ImportID, x.ImportPayeeName,
				x.ImportPayeeNameOriginal, x.DebtTransactionType, x.Deleted, now}
		})
}

func (t *Tx) UpsertSubtransactions(planID string, rows []Subtransaction) error {
	return upsert(t, "subtransactions", `INSERT INTO subtransactions (
		plan_id, id, transaction_id, amount, memo, payee_id, category_id, transfer_account_id,
		transfer_transaction_id, deleted
	) VALUES (?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT (plan_id, id) DO UPDATE SET
		transaction_id = excluded.transaction_id, amount = excluded.amount, memo = excluded.memo,
		payee_id = excluded.payee_id, category_id = excluded.category_id,
		transfer_account_id = excluded.transfer_account_id,
		transfer_transaction_id = excluded.transfer_transaction_id, deleted = excluded.deleted`,
		rows, func(s Subtransaction) []any {
			return []any{planID, s.ID, s.TransactionID, s.Amount, s.Memo, s.PayeeID, s.CategoryID,
				s.TransferAccountID, s.TransferTransactionID, s.Deleted}
		})
}

func (t *Tx) UpsertMonths(planID string, rows []Month) error {
	return upsert(t, "months", `INSERT INTO months (
		plan_id, month, note, income, budgeted, activity, to_be_budgeted, age_of_money, deleted
	) VALUES (?,?,?,?,?,?,?,?,?)
	ON CONFLICT (plan_id, month) DO UPDATE SET
		note = excluded.note, income = excluded.income, budgeted = excluded.budgeted,
		activity = excluded.activity, to_be_budgeted = excluded.to_be_budgeted,
		age_of_money = excluded.age_of_money, deleted = excluded.deleted`,
		rows, func(m Month) []any {
			return []any{planID, m.Month, m.Note, m.Income, m.Budgeted, m.Activity, m.ToBeBudgeted,
				m.AgeOfMoney, m.Deleted}
		})
}

func (t *Tx) UpsertMonthCategories(planID string, rows []MonthCategory) error {
	return upsert(t, "month_categories", `INSERT INTO month_categories (
		plan_id, month, category_id, budgeted, activity, balance, goal_target, goal_under_funded,
		goal_overall_funded, goal_overall_left, goal_percentage_complete, deleted
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT (plan_id, month, category_id) DO UPDATE SET
		budgeted = excluded.budgeted, activity = excluded.activity, balance = excluded.balance,
		goal_target = excluded.goal_target, goal_under_funded = excluded.goal_under_funded,
		goal_overall_funded = excluded.goal_overall_funded,
		goal_overall_left = excluded.goal_overall_left,
		goal_percentage_complete = excluded.goal_percentage_complete, deleted = excluded.deleted`,
		rows, func(m MonthCategory) []any {
			return []any{planID, m.Month, m.CategoryID, m.Budgeted, m.Activity, m.Balance,
				m.GoalTarget, m.GoalUnderFunded, m.GoalOverallFunded, m.GoalOverallLeft,
				m.GoalPercentageComplete, m.Deleted}
		})
}

func (t *Tx) UpsertScheduledTransactions(planID string, rows []ScheduledTransaction) error {
	return upsert(t, "scheduled_transactions", `INSERT INTO scheduled_transactions (
		plan_id, id, date_first, date_next, frequency, amount, memo, flag_color, flag_name,
		account_id, payee_id, category_id, transfer_account_id, deleted
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT (plan_id, id) DO UPDATE SET
		date_first = excluded.date_first, date_next = excluded.date_next,
		frequency = excluded.frequency, amount = excluded.amount, memo = excluded.memo,
		flag_color = excluded.flag_color, flag_name = excluded.flag_name,
		account_id = excluded.account_id, payee_id = excluded.payee_id,
		category_id = excluded.category_id, transfer_account_id = excluded.transfer_account_id,
		deleted = excluded.deleted`,
		rows, func(s ScheduledTransaction) []any {
			return []any{planID, s.ID, s.DateFirst, s.DateNext, s.Frequency, s.Amount, s.Memo,
				s.FlagColor, s.FlagName, s.AccountID, s.PayeeID, s.CategoryID, s.TransferAccountID,
				s.Deleted}
		})
}

func (t *Tx) UpsertScheduledSubtransactions(planID string, rows []ScheduledSubtransaction) error {
	return upsert(t, "scheduled_subtransactions", `INSERT INTO scheduled_subtransactions (
		plan_id, id, scheduled_transaction_id, amount, memo, payee_id, category_id,
		transfer_account_id, deleted
	) VALUES (?,?,?,?,?,?,?,?,?)
	ON CONFLICT (plan_id, id) DO UPDATE SET
		scheduled_transaction_id = excluded.scheduled_transaction_id, amount = excluded.amount,
		memo = excluded.memo, payee_id = excluded.payee_id, category_id = excluded.category_id,
		transfer_account_id = excluded.transfer_account_id, deleted = excluded.deleted`,
		rows, func(s ScheduledSubtransaction) []any {
			return []any{planID, s.ID, s.ScheduledTransactionID, s.Amount, s.Memo, s.PayeeID,
				s.CategoryID, s.TransferAccountID, s.Deleted}
		})
}

// SetSyncState records the delta cursor for s.PlanID.
func (t *Tx) SetSyncState(s SyncState) error {
	_, err := t.c.ExecContext(t.ctx, `INSERT INTO sync_state (plan_id, server_knowledge, synced_at)
		VALUES (?,?,?)
		ON CONFLICT (plan_id) DO UPDATE SET
			server_knowledge = excluded.server_knowledge, synced_at = excluded.synced_at`,
		s.PlanID, s.ServerKnowledge, formatTime(s.SyncedAt))
	if err != nil {
		return fmt.Errorf("store: set sync_state: %w", err)
	}
	return nil
}

// SetSyncState records the delta cursor in its own transaction.
func (d *DB) SetSyncState(ctx context.Context, s SyncState) error {
	return d.Update(ctx, func(t *Tx) error { return t.SetSyncState(s) })
}

// GetSyncState returns the delta cursor for planID; ok is false if the plan
// has never been synced.
func (d *DB) GetSyncState(ctx context.Context, planID string) (s SyncState, ok bool, err error) {
	var syncedAt string
	err = d.r.QueryRowContext(ctx,
		`SELECT plan_id, server_knowledge, synced_at FROM sync_state WHERE plan_id = ?`, planID).
		Scan(&s.PlanID, &s.ServerKnowledge, &syncedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncState{}, false, nil
	}
	if err != nil {
		return SyncState{}, false, fmt.Errorf("store: get sync_state: %w", err)
	}
	if s.SyncedAt, err = time.Parse(time.RFC3339Nano, syncedAt); err != nil {
		return SyncState{}, false, fmt.Errorf("store: parse sync_state.synced_at: %w", err)
	}
	return s, true, nil
}

// periodicJSON encodes a loan periodic-value map as JSON (keys sorted by
// encoding/json), or NULL when empty.
func periodicJSON(m map[string]int64) *string {
	if len(m) == 0 {
		return nil
	}
	b, _ := json.Marshal(m) // map[string]int64 cannot fail to marshal
	s := string(b)
	return &s
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func formatTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := formatTime(*t)
	return &s
}
