package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// PlanScan is one plan's result from the archived-plan scan.
type PlanScan struct {
	PlanID string
	Name   string
	// HasDiscretionary is meaningful only when ScanError is empty.
	HasDiscretionary bool
	// ScanError is the redacted reason the plan's categories couldn't be
	// fetched; empty on success.
	ScanError string
	ScannedAt time.Time
}

// ReplacePlanScans makes rows the whole stored scan result, dropping plans
// from earlier scans that are not in rows.
func (d *DB) ReplacePlanScans(ctx context.Context, rows []PlanScan) error {
	return d.Update(ctx, func(t *Tx) error {
		if _, err := t.c.ExecContext(t.ctx, `DELETE FROM plan_scan`); err != nil {
			return fmt.Errorf("store: clear plan_scan: %w", err)
		}
		return upsert(t, "plan_scan", `INSERT INTO plan_scan (
			plan_id, name, has_discretionary, scan_error, scanned_at
		) VALUES (?,?,?,?,?)`,
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
