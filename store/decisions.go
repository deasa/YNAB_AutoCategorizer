package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Values of Decision.Method, Mode and Outcome. The table's CHECKs enforce them.
const (
	MethodRule  = "rule"
	MethodModel = "model"

	ModeDryRun = "dry_run"
	ModeAuto   = "auto"

	OutcomeWouldApply = "would_apply"
	OutcomeApplied    = "applied"
	OutcomeAsked      = "asked"
	OutcomeSkipped    = "skipped"
)

// Decision is one categorizer outcome for a transaction.
type Decision struct {
	ID             int64 // set by InsertDecision; insertion order within a plan
	PlanID         string
	TxnID          string
	CategoryID     *string // nil when no category was chosen, as are the next three
	CategoryName   *string
	Confidence     *float64
	RunnerUp       *string
	Method         string
	Model          string
	PolicyHash     string
	CandidatesHash string
	Mode           string
	Outcome        string
	SecondOpinion  json.RawMessage // nil when there was none
	Features       json.RawMessage // required
	CreatedAt      time.Time       // InsertDecision uses now if zero
	SupersededBy   *int64
}

// DecisionKey is what a decision was made under. A decision made under another
// key must be made again.
type DecisionKey struct {
	Model          string
	PolicyHash     string
	CandidatesHash string
	Mode           string
}

// InsertDecision stores d as the transaction's current decision and points the
// previous one at it, in one transaction. It returns the new id; d.ID and
// d.SupersededBy are ignored.
func (d *DB) InsertDecision(ctx context.Context, dec Decision) (int64, error) {
	created := dec.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	var secondOpinion *string
	if len(dec.SecondOpinion) > 0 {
		s := string(dec.SecondOpinion)
		secondOpinion = &s
	}
	var id int64
	err := writeTx(ctx, d.w, func(c *sql.Conn) error {
		// The single writer inside BEGIN IMMEDIATE makes this read safe.
		if err := c.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(id), 0) + 1 FROM decisions WHERE plan_id = ?`, dec.PlanID).Scan(&id); err != nil {
			return err
		}
		// Before the insert, so decisions_current never sees two current rows.
		// The foreign key to the row inserted next is deferred to COMMIT.
		if _, err := c.ExecContext(ctx,
			`UPDATE decisions SET superseded_by = ? WHERE plan_id = ? AND txn_id = ? AND superseded_by IS NULL`,
			id, dec.PlanID, dec.TxnID); err != nil {
			return err
		}
		_, err := c.ExecContext(ctx, `
INSERT INTO decisions (plan_id, id, txn_id, category_id, category_name, confidence, runner_up, method, model,
                       policy_hash, candidates_hash, mode, outcome, second_opinion, features, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			dec.PlanID, id, dec.TxnID, dec.CategoryID, dec.CategoryName, dec.Confidence, dec.RunnerUp, dec.Method, dec.Model,
			dec.PolicyHash, dec.CandidatesHash, dec.Mode, dec.Outcome, secondOpinion, string(dec.Features), formatTime(created))
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: insert decision: %w", err)
	}
	return id, nil
}

const decisionColumns = `plan_id, id, txn_id, category_id, category_name, confidence, runner_up, method, model,
       policy_hash, candidates_hash, mode, outcome, second_opinion, features, created_at, superseded_by`

func scanDecision(scan func(...any) error) (Decision, error) {
	var (
		dec           Decision
		secondOpinion *string
		features      string
		created       string
	)
	if err := scan(&dec.PlanID, &dec.ID, &dec.TxnID, &dec.CategoryID, &dec.CategoryName, &dec.Confidence, &dec.RunnerUp,
		&dec.Method, &dec.Model, &dec.PolicyHash, &dec.CandidatesHash, &dec.Mode, &dec.Outcome, &secondOpinion, &features,
		&created, &dec.SupersededBy); err != nil {
		return Decision{}, err
	}
	at, err := time.Parse(timeLayout, created)
	if err != nil {
		return Decision{}, fmt.Errorf("parse created_at: %w", err)
	}
	dec.CreatedAt = at
	dec.Features = json.RawMessage(features)
	if secondOpinion != nil {
		dec.SecondOpinion = json.RawMessage(*secondOpinion)
	}
	return dec, nil
}

// LatestDecision returns the transaction's current decision. ok is false if it
// has none.
func (d *DB) LatestDecision(ctx context.Context, planID, txnID string) (dec Decision, ok bool, err error) {
	dec, err = scanDecision(d.r.QueryRowContext(ctx, `SELECT `+decisionColumns+`
FROM decisions WHERE plan_id = ? AND txn_id = ? ORDER BY id DESC LIMIT 1`, planID, txnID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Decision{}, false, nil
	}
	if err != nil {
		return Decision{}, false, fmt.Errorf("store: latest decision: %w", err)
	}
	return dec, true, nil
}

// DecisionHistory returns every decision for the transaction, oldest first.
func (d *DB) DecisionHistory(ctx context.Context, planID, txnID string) ([]Decision, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT `+decisionColumns+`
FROM decisions WHERE plan_id = ? AND txn_id = ? ORDER BY id`, planID, txnID)
	if err != nil {
		return nil, fmt.Errorf("store: decision history: %w", err)
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
		return nil, fmt.Errorf("store: decision history: %w", err)
	}
	return out, nil
}

// HasCurrentDecision reports whether the transaction's current decision was
// made under exactly key.
func (d *DB) HasCurrentDecision(ctx context.Context, planID, txnID string, key DecisionKey) (bool, error) {
	var has bool
	err := d.r.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM decisions
               WHERE plan_id = ? AND txn_id = ? AND superseded_by IS NULL
                 AND model = ? AND policy_hash = ? AND candidates_hash = ? AND mode = ?)`,
		planID, txnID, key.Model, key.PolicyHash, key.CandidatesHash, key.Mode).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: has current decision: %w", err)
	}
	return has, nil
}
