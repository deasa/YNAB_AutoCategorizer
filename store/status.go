package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// StatusState is the stored state of the Slack status message.
type StatusState struct {
	// ChannelID and TS locate the message; both are empty before the first post.
	ChannelID, TS string
	// BackupAt is zero until a backup has been recorded.
	BackupAt time.Time
	BackupOK bool
}

// GetStatusState returns the stored state, or the zero value if there is none.
func (d *DB) GetStatusState(ctx context.Context) (StatusState, error) {
	var channel, ts, backupAt sql.NullString
	var backupOK sql.NullBool
	err := d.r.QueryRowContext(ctx, `SELECT channel_id, ts, backup_at, backup_ok FROM status_message WHERE id = 1`).
		Scan(&channel, &ts, &backupAt, &backupOK)
	if errors.Is(err, sql.ErrNoRows) {
		return StatusState{}, nil
	}
	if err != nil {
		return StatusState{}, fmt.Errorf("store: get status_message: %w", err)
	}
	s := StatusState{ChannelID: channel.String, TS: ts.String, BackupOK: backupOK.Bool}
	if backupAt.Valid {
		if s.BackupAt, err = time.Parse(time.RFC3339Nano, backupAt.String); err != nil {
			return StatusState{}, fmt.Errorf("store: parse status_message.backup_at: %w", err)
		}
	}
	return s, nil
}

// SetStatusMessage records where the status message lives.
func (d *DB) SetStatusMessage(ctx context.Context, channelID, ts string) error {
	return d.Update(ctx, func(t *Tx) error {
		_, err := t.c.ExecContext(t.ctx, `INSERT INTO status_message (id, channel_id, ts) VALUES (1,?,?)
			ON CONFLICT (id) DO UPDATE SET channel_id = excluded.channel_id, ts = excluded.ts`, channelID, ts)
		if err != nil {
			return fmt.Errorf("store: set status_message: %w", err)
		}
		return nil
	})
}

// SetStatusBackup records the latest backup's time and result.
func (d *DB) SetStatusBackup(ctx context.Context, at time.Time, ok bool) error {
	return d.Update(ctx, func(t *Tx) error {
		_, err := t.c.ExecContext(t.ctx, `INSERT INTO status_message (id, backup_at, backup_ok) VALUES (1,?,?)
			ON CONFLICT (id) DO UPDATE SET backup_at = excluded.backup_at, backup_ok = excluded.backup_ok`,
			formatTime(at), ok)
		if err != nil {
			return fmt.Errorf("store: set status_message backup: %w", err)
		}
		return nil
	})
}

// AccountImport is one bank-linked account's import state.
type AccountImport struct {
	Name string
	// NewestImport is the newest imported transaction's date (YYYY-MM-DD),
	// empty if the account has none.
	NewestImport string
	InError      bool
}

// AccountImports returns planID's open, bank-linked accounts ordered by name.
func (d *DB) AccountImports(ctx context.Context, planID string) ([]AccountImport, error) {
	rows, err := d.r.QueryContext(ctx, `SELECT a.name, a.direct_import_in_error, COALESCE(MAX(t.date), '')
		FROM accounts a
		LEFT JOIN transactions t ON t.plan_id = a.plan_id AND t.account_id = a.id
			AND t.import_id IS NOT NULL AND t.deleted = 0
		WHERE a.plan_id = ? AND a.deleted = 0 AND a.closed = 0 AND a.direct_import_linked = 1
		GROUP BY a.id ORDER BY a.name, a.id`, planID)
	if err != nil {
		return nil, fmt.Errorf("store: read account imports: %w", err)
	}
	defer rows.Close()
	var out []AccountImport
	for rows.Next() {
		var a AccountImport
		if err := rows.Scan(&a.Name, &a.InError, &a.NewestImport); err != nil {
			return nil, fmt.Errorf("store: scan account import: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read account imports: %w", err)
	}
	return out, nil
}
