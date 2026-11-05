package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// PlanScan is one plan's result from the archived-plan scan.
type PlanScan struct {
	PlanID string
	Name   string
	// HasDiscretionary is the verdict of the plan's last successful scan, so
	// it survives a later failure. It is false if no scan has succeeded yet.
	HasDiscretionary bool
	// ScanError is the redacted reason the latest scan couldn't fetch the
	// plan's categories; empty if the latest scan succeeded.
	ScanError string
	// ScannedAt is when the plan last scanned successfully. A plan that has
	// never succeeded keeps the time of its first failed attempt.
	ScannedAt time.Time
}

// ReplacePlanScans stores rows as the latest scan and drops plans that are not
// in rows. A row with ScanError set records only the error and the name: a plan
// already stored keeps its HasDiscretionary and ScannedAt, and a new one is
// stored with no verdict.
func (d *DB) ReplacePlanScans(ctx context.Context, rows []PlanScan) error {
	ids := make([]string, 0, len(rows))
	for _, p := range rows {
		ids = append(ids, p.PlanID)
	}
	listed, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("store: encode plan ids: %w", err)
	}
	return d.Update(ctx, func(t *Tx) error {
		if _, err := t.c.ExecContext(t.ctx, `DELETE FROM plan_scan
			WHERE plan_id NOT IN (SELECT value FROM json_each(?))`, string(listed)); err != nil {
			return fmt.Errorf("store: prune plan_scan: %w", err)
		}
		return upsert(t, "plan_scan", `INSERT INTO plan_scan (
			plan_id, name, has_discretionary, scan_error, scanned_at
		) VALUES (?,?,?,?,?)
		ON CONFLICT (plan_id) DO UPDATE SET
			name = excluded.name,
			scan_error = excluded.scan_error,
			has_discretionary = CASE WHEN excluded.scan_error IS NULL
				THEN excluded.has_discretionary ELSE has_discretionary END,
			scanned_at = CASE WHEN excluded.scan_error IS NULL
				THEN excluded.scanned_at ELSE scanned_at END`,
			rows, func(p PlanScan) []any {
				var has, scanErr any = p.HasDiscretionary, nil
				if p.ScanError != "" {
					has, scanErr = nil, p.ScanError
				}
				return []any{p.PlanID, p.Name, has, scanErr, formatTime(p.ScannedAt)}
			})
	})
}

// PlanScans returns the stored scan result ordered by plan name, or nil if
// no scan has run.
func (d *DB) PlanScans(ctx context.Context) ([]PlanScan, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT plan_id, name, has_discretionary, scan_error, scanned_at
		FROM plan_scan ORDER BY name, plan_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read plan_scan: %w", err)
	}
	defer rows.Close()
	var out []PlanScan
	for rows.Next() {
		var p PlanScan
		var has sql.NullBool
		var scanErr sql.NullString
		var at string
		if err := rows.Scan(&p.PlanID, &p.Name, &has, &scanErr, &at); err != nil {
			return nil, fmt.Errorf("store: read plan_scan: %w", err)
		}
		p.HasDiscretionary, p.ScanError = has.Bool, scanErr.String
		if p.ScannedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("store: parse plan_scan.scanned_at: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
