package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Values of WriteEntry.Actor, Via and Status, and of HumanLock.Source. The
// tables' CHECKs enforce them.
const (
	ActorSystem  = "system"
	ActorBrendan = "brendan"
	ActorWife    = "wife"

	ViaScheduler = "scheduler"
	ViaMCP       = "mcp"
	ViaSlack     = "slack"

	WriteIntent = "intent"
	WriteDone   = "done"
	WriteFailed = "failed"

	LockYNABEdit   = "ynab_edit"
	LockFirstTouch = "first_touch"
	LockSlack      = "slack"
)

// ErrWriteNotIntent means a write can't be finished: it is already done or
// failed, or no such row exists.
var ErrWriteNotIntent = errors.New("store: write is not an open intent")

// WriteEntry is one row of the audit log.
type WriteEntry struct {
	ID         int64 // set by InsertWriteIntent
	PlanID     string
	Actor      string
	Via        string
	Tier       string
	Endpoint   string
	TxnID      *string         // nil for a write that doesn't touch a transaction
	CategoryID *string         // nil for a write that doesn't touch a category
	BeforeJSON json.RawMessage // required; the JSON value null for a create
	AfterJSON  json.RawMessage // required; the JSON value null for a delete
	RequestID  *string         // shared by the rows of one bulk request
	Status     string
	Error      *string
	CreatedAt  time.Time // InsertWriteIntent uses now if zero
	RevertedBy *int64
}

// InsertWriteIntent records a write about to be sent and returns its id. e.ID,
// e.Status, e.Error and e.RevertedBy are ignored.
func (d *DB) InsertWriteIntent(ctx context.Context, e WriteEntry) (int64, error) {
	created := e.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	var id int64
	err := writeTx(ctx, d.w, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx, `
INSERT INTO write_log (plan_id, actor, via, tier, endpoint, txn_id, category_id, before_json, after_json,
                       request_id, status, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.PlanID, e.Actor, e.Via, e.Tier, e.Endpoint, e.TxnID, e.CategoryID, string(e.BeforeJSON), string(e.AfterJSON),
			e.RequestID, WriteIntent, formatTime(created))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: insert write intent: %w", err)
	}
	return id, nil
}

// MarkWriteDone moves an intent to done. A non-empty after replaces the intended
// after_json with what YNAB actually stored, so a value YNAB normalized doesn't
// later read as a human edit. It returns ErrWriteNotIntent if id is not an intent.
func (d *DB) MarkWriteDone(ctx context.Context, id int64, after json.RawMessage) error {
	var stored *string
	if len(after) > 0 {
		s := string(after)
		stored = &s
	}
	return d.finishWrite(ctx, id, WriteDone, stored, nil)
}

// MarkWriteFailed moves an intent to failed, keeping what was attempted. It
// returns ErrWriteNotIntent if id is not an intent. errMsg must already be
// redacted.
func (d *DB) MarkWriteFailed(ctx context.Context, id int64, errMsg string) error {
	return d.finishWrite(ctx, id, WriteFailed, nil, &errMsg)
}

func (d *DB) finishWrite(ctx context.Context, id int64, status string, after, errMsg *string) error {
	err := writeTx(ctx, d.w, func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx, `
UPDATE write_log SET status = ?, after_json = COALESCE(?, after_json), error = ?
WHERE id = ? AND status = 'intent'`, status, after, errMsg, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrWriteNotIntent
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: finish write: %w", err)
	}
	return nil
}

const writeColumns = `id, plan_id, actor, via, tier, endpoint, txn_id, category_id, before_json, after_json,
       request_id, status, error, created_at, reverted_by`

func scanWrite(scan func(...any) error) (WriteEntry, error) {
	var (
		e             WriteEntry
		before, after string
		created       string
	)
	if err := scan(&e.ID, &e.PlanID, &e.Actor, &e.Via, &e.Tier, &e.Endpoint, &e.TxnID, &e.CategoryID, &before, &after,
		&e.RequestID, &e.Status, &e.Error, &created, &e.RevertedBy); err != nil {
		return WriteEntry{}, err
	}
	at, err := time.Parse(timeLayout, created)
	if err != nil {
		return WriteEntry{}, fmt.Errorf("parse created_at: %w", err)
	}
	e.CreatedAt = at
	e.BeforeJSON = json.RawMessage(before)
	e.AfterJSON = json.RawMessage(after)
	return e, nil
}

// LastDoneWrite returns the transaction's most recent completed write. ok is
// false if it has none.
func (d *DB) LastDoneWrite(ctx context.Context, planID, txnID string) (e WriteEntry, ok bool, err error) {
	e, err = scanWrite(d.r.QueryRowContext(ctx, `SELECT `+writeColumns+`
FROM write_log WHERE plan_id = ? AND txn_id = ? AND status = 'done' ORDER BY id DESC LIMIT 1`, planID, txnID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return WriteEntry{}, false, nil
	}
	if err != nil {
		return WriteEntry{}, false, fmt.Errorf("store: last done write: %w", err)
	}
	return e, true, nil
}

// UnfinishedWrites returns every intent never marked done or failed, in any
// plan, oldest first. After a crash, each may or may not have reached YNAB.
func (d *DB) UnfinishedWrites(ctx context.Context) ([]WriteEntry, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT `+writeColumns+`
FROM write_log WHERE status = 'intent' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: unfinished writes: %w", err)
	}
	defer rows.Close()
	var out []WriteEntry
	for rows.Next() {
		e, err := scanWrite(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan write: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: unfinished writes: %w", err)
	}
	return out, nil
}

// HumanLock marks one transaction field as owned by a person.
type HumanLock struct {
	PlanID    string
	TxnID     string
	Field     string
	Source    string
	CreatedAt time.Time // Lock uses now if zero
}

// Lock stores l. If the field is already locked it does nothing, so the first
// lock's source and time stand.
func (d *DB) Lock(ctx context.Context, l HumanLock) error {
	created := l.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	err := writeTx(ctx, d.w, func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `
INSERT INTO human_locks (plan_id, txn_id, field, source, created_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (plan_id, txn_id, field) DO NOTHING`, l.PlanID, l.TxnID, l.Field, l.Source, formatTime(created))
		return err
	})
	if err != nil {
		return fmt.Errorf("store: lock: %w", err)
	}
	return nil
}

// Locks returns the locks on txnIDs, keyed by transaction id, each transaction's
// ordered by field. A transaction with no locks has no key.
func (d *DB) Locks(ctx context.Context, planID string, txnIDs []string) (map[string][]HumanLock, error) {
	out := map[string][]HumanLock{}
	if len(txnIDs) == 0 {
		return out, nil
	}
	ids, err := json.Marshal(txnIDs)
	if err != nil {
		return nil, fmt.Errorf("store: locks: %w", err)
	}
	// json_each, because the set can outgrow SQLite's bound-variable limit.
	rows, err := d.r.QueryContext(ctx, `
SELECT plan_id, txn_id, field, source, created_at FROM human_locks
WHERE plan_id = ? AND txn_id IN (SELECT value FROM json_each(?))
ORDER BY txn_id, field`, planID, string(ids))
	if err != nil {
		return nil, fmt.Errorf("store: locks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			l       HumanLock
			created string
		)
		if err := rows.Scan(&l.PlanID, &l.TxnID, &l.Field, &l.Source, &created); err != nil {
			return nil, fmt.Errorf("store: scan lock: %w", err)
		}
		if l.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
			return nil, fmt.Errorf("store: parse lock created_at: %w", err)
		}
		out[l.TxnID] = append(out[l.TxnID], l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: locks: %w", err)
	}
	return out, nil
}
