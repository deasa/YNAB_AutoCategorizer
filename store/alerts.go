package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Values of Alert.Status. The table's CHECK enforces them.
const (
	AlertSent     = "sent"
	AlertRolledUp = "rolled_up" // held for the monthly report by the alert budget
)

// Alert is one alert raised.
type Alert struct {
	ID        int64 // set by InsertAlert
	PlanID    string
	Kind      string // the rule, e.g. "duplicate"
	DedupeKey string // with PlanID and Kind, identifies the alert; raised at most once
	Critical  bool
	Status    string
	Body      string
	CreatedAt time.Time // InsertAlert uses now if zero
}

// InsertAlert stores a and returns its id. inserted is false, and nothing
// changes, if an alert with the same plan, kind and dedupe key already exists:
// it was raised before and must not be sent again.
func (d *DB) InsertAlert(ctx context.Context, a Alert) (id int64, inserted bool, err error) {
	created := a.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	err = writeTx(ctx, d.w, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx, `
INSERT INTO alerts (plan_id, kind, dedupe_key, critical, status, body, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (plan_id, kind, dedupe_key) DO NOTHING`,
			a.PlanID, a.Kind, a.DedupeKey, a.Critical, a.Status, a.Body, formatTime(created))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		inserted = true
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, false, fmt.Errorf("store: insert alert: %w", err)
	}
	return id, inserted, nil
}

// HasAlert reports whether the alert (planID, kind, dedupeKey) is stored, so a
// caller can post first and store after without double-sending on the next tick.
func (d *DB) HasAlert(ctx context.Context, planID, kind, dedupeKey string) (bool, error) {
	var has bool
	err := d.r.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM alerts WHERE plan_id = ? AND kind = ? AND dedupe_key = ?)`,
		planID, kind, dedupeKey).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: has alert: %w", err)
	}
	return has, nil
}

// DeleteAlert removes the alert (planID, kind, dedupeKey), if stored, so the
// same key can alert again: a failed post that must retry, or an episode (link
// error, stale import) that has cleared. It reports whether a row was removed.
func (d *DB) DeleteAlert(ctx context.Context, planID, kind, dedupeKey string) (bool, error) {
	n, err := d.execWrite(ctx, `DELETE FROM alerts WHERE plan_id = ? AND kind = ? AND dedupe_key = ?`,
		planID, kind, dedupeKey)
	if err != nil {
		return false, fmt.Errorf("store: delete alert: %w", err)
	}
	return n == 1, nil
}

// CountSentNonCritical returns how many non-critical alerts of planID were sent
// in [from, to): what the alert budget has spent.
func (d *DB) CountSentNonCritical(ctx context.Context, planID string, from, to time.Time) (int, error) {
	var n int
	err := d.r.QueryRowContext(ctx, `
SELECT count(*) FROM alerts
WHERE plan_id = ? AND critical = 0 AND status = 'sent' AND created_at >= ? AND created_at < ?`,
		planID, formatTime(from), formatTime(to)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count alerts: %w", err)
	}
	return n, nil
}

// Alerts returns planID's alerts created in [from, to), oldest first; the
// monthly report reads the rolled-up ones here.
func (d *DB) Alerts(ctx context.Context, planID string, from, to time.Time) ([]Alert, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT id, plan_id, kind, dedupe_key, critical, status, body, created_at FROM alerts
WHERE plan_id = ? AND created_at >= ? AND created_at < ?
ORDER BY created_at, id`, planID, formatTime(from), formatTime(to))
	if err != nil {
		return nil, fmt.Errorf("store: alerts: %w", err)
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var (
			a       Alert
			created string
		)
		if err := rows.Scan(&a.ID, &a.PlanID, &a.Kind, &a.DedupeKey, &a.Critical, &a.Status, &a.Body, &created); err != nil {
			return nil, fmt.Errorf("store: scan alert: %w", err)
		}
		if a.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
			return nil, fmt.Errorf("store: parse alert created_at: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: alerts: %w", err)
	}
	return out, nil
}

// ImportState is the last bank-import attempt for a plan.
type ImportState struct {
	LastAttemptAt time.Time
	LastOKAt      *time.Time // nil if no import has succeeded
	LastError     *string    // nil after a success; redacted text
}

// RecordImport stores an import attempt at at. A nil errMsg is a success, which
// clears the last error; a failure keeps the last success time. errMsg must
// already be redacted.
func (d *DB) RecordImport(ctx context.Context, planID string, at time.Time, errMsg *string) error {
	var okAt *string
	if errMsg == nil {
		s := formatTime(at)
		okAt = &s
	}
	_, err := d.execWrite(ctx, `
INSERT INTO import_state (plan_id, last_attempt_at, last_ok_at, last_error) VALUES (?, ?, ?, ?)
ON CONFLICT (plan_id) DO UPDATE SET last_attempt_at = excluded.last_attempt_at,
    last_ok_at = COALESCE(excluded.last_ok_at, import_state.last_ok_at), last_error = excluded.last_error`,
		planID, formatTime(at), okAt, errMsg)
	if err != nil {
		return fmt.Errorf("store: record import: %w", err)
	}
	return nil
}

// GetImportState returns planID's import state. ok is false before the first
// attempt.
func (d *DB) GetImportState(ctx context.Context, planID string) (st ImportState, ok bool, err error) {
	var (
		attempt string
		okAt    *string
	)
	err = d.r.QueryRowContext(ctx, `SELECT last_attempt_at, last_ok_at, last_error FROM import_state WHERE plan_id = ?`,
		planID).Scan(&attempt, &okAt, &st.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportState{}, false, nil
	}
	if err != nil {
		return ImportState{}, false, fmt.Errorf("store: import state: %w", err)
	}
	if st.LastAttemptAt, err = time.Parse(timeLayout, attempt); err != nil {
		return ImportState{}, false, fmt.Errorf("store: parse last_attempt_at: %w", err)
	}
	if okAt != nil {
		t, err := time.Parse(timeLayout, *okAt)
		if err != nil {
			return ImportState{}, false, fmt.Errorf("store: parse last_ok_at: %w", err)
		}
		st.LastOKAt = &t
	}
	return st, true, nil
}

// SetAlertCursor stores the newest first_seen_at the anomaly rules have seen.
func (d *DB) SetAlertCursor(ctx context.Context, planID string, lastFirstSeenAt time.Time) error {
	_, err := d.execWrite(ctx, `
INSERT INTO alert_cursor (plan_id, last_first_seen_at) VALUES (?, ?)
ON CONFLICT (plan_id) DO UPDATE SET last_first_seen_at = excluded.last_first_seen_at`,
		planID, formatTime(lastFirstSeenAt))
	if err != nil {
		return fmt.Errorf("store: set alert cursor: %w", err)
	}
	return nil
}

// AlertCursor returns planID's alert cursor. ok is false if none is set.
func (d *DB) AlertCursor(ctx context.Context, planID string) (t time.Time, ok bool, err error) {
	var s string
	err = d.r.QueryRowContext(ctx, `SELECT last_first_seen_at FROM alert_cursor WHERE plan_id = ?`, planID).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: alert cursor: %w", err)
	}
	if t, err = time.Parse(timeLayout, s); err != nil {
		return time.Time{}, false, fmt.Errorf("store: parse alert cursor: %w", err)
	}
	return t, true, nil
}

// TransactionsFirstSeenAfter returns planID's live transactions first stored
// after t, by first_seen_at then id: the rows the anomaly rules haven't seen.
func (d *DB) TransactionsFirstSeenAfter(ctx context.Context, planID string, t time.Time) ([]TransactionDetail, error) {
	rows, err := d.r.QueryContext(ctx, transactionDetailQuery+`
WHERE t.plan_id = ? AND t.deleted = 0 AND t.first_seen_at > ?
ORDER BY t.first_seen_at, t.id`, planID, formatTime(t))
	if err != nil {
		return nil, fmt.Errorf("store: transactions first seen after: %w", err)
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
		return nil, fmt.Errorf("store: transactions first seen after: %w", err)
	}
	return out, nil
}

// PayeeCharge is one earlier outflow to a payee.
type PayeeCharge struct {
	TxnID    string
	Date     string // YYYY-MM-DD
	Amount   int64  // milliunits, negative
	ImportID *string
}

// PayeeCharges returns planID's live, non-transfer outflows dated before
// before ("YYYY-MM-DD", exclusive) whose payee name (or imported name, if it
// has no payee) normalizes to normalizedPayee, oldest first. normalizedPayee is
// a NormalizePayee result.
func (d *DB) PayeeCharges(ctx context.Context, planID, normalizedPayee, before string) ([]PayeeCharge, error) {
	rows, err := d.r.QueryContext(ctx, `
SELECT t.id, t.date, t.amount, t.import_id, COALESCE(p.name, t.import_payee_name, '')
FROM transactions t
LEFT JOIN payees p ON p.plan_id = t.plan_id AND p.id = t.payee_id
WHERE t.plan_id = ? AND t.deleted = 0 AND t.transfer_account_id IS NULL AND t.amount < 0 AND t.date < ?
ORDER BY t.date, t.id`, planID, before)
	if err != nil {
		return nil, fmt.Errorf("store: payee charges: %w", err)
	}
	defer rows.Close()
	var out []PayeeCharge
	for rows.Next() {
		var (
			c    PayeeCharge
			name string
		)
		if err := rows.Scan(&c.TxnID, &c.Date, &c.Amount, &c.ImportID, &name); err != nil {
			return nil, fmt.Errorf("store: scan payee charge: %w", err)
		}
		if NormalizePayee(name) == normalizedPayee {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: payee charges: %w", err)
	}
	return out, nil
}
