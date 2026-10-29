// Package store owns finbot's SQLite database: connection setup, embedded
// migrations, and the YNAB mirror tables.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// BusyTimeout is how long a connection waits on a locked database before
// returning SQLITE_BUSY.
const BusyTimeout = 5 * time.Second

// readPoolSize caps concurrent readers (sync, MCP and Slack goroutines).
const readPoolSize = 4

// DB is finbot's database. It keeps two pools over the same file:
//   - a single writer connection, so writes serialize in Go instead of
//     contending for SQLite's write lock, and every write transaction starts
//     with BEGIN IMMEDIATE (see writeTx);
//   - a read pool of query_only connections, which under WAL read a
//     consistent snapshot without blocking on, or being blocked by, the writer.
type DB struct {
	w *sql.DB
	r *sql.DB
}

// Open opens (creating if needed) the database at path, verifies that the
// required pragmas took effect, and applies pending migrations. path must be a
// file: WAL is not available for in-memory databases, so ":memory:" fails.
func Open(ctx context.Context, path string) (*DB, error) {
	w, err := openPool(ctx, path, false)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, w); err != nil {
		w.Close()
		return nil, err
	}
	r, err := openPool(ctx, path, true)
	if err != nil {
		w.Close()
		return nil, err
	}
	return &DB{w: w, r: r}, nil
}

// Close closes both pools.
func (d *DB) Close() error {
	rerr := d.r.Close()
	if err := d.w.Close(); err != nil {
		return err
	}
	return rerr
}

// Reader returns the read-only pool, for queries by other packages.
func (d *DB) Reader() *sql.DB { return d.r }

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeout.Milliseconds()))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(ON)")
	}
	return "file:" + uriPath(path) + "?" + q.Encode()
}

// uriPath escapes a filesystem path for a SQLite "file:" URI: '%', '?' and '#'
// are URI syntax, and a leading "//" would be read as an authority.
func uriPath(path string) string {
	if path != ":memory:" {
		path = filepath.Clean(path)
	}
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
}

func openPool(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, readOnly))
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if readOnly {
		db.SetMaxOpenConns(readPoolSize)
	} else {
		db.SetMaxOpenConns(1)
	}
	// Connections are cheap to keep and pragmas are per connection; never
	// expire them so the pool doesn't churn.
	db.SetConnMaxLifetime(0)
	db.SetMaxIdleConns(readPoolSize)
	if err := verifyPragmas(ctx, db, readOnly); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// verifyPragmas fails if any connection setting the store relies on is not in
// effect, e.g. a driver that ignored the DSN or a database that can't do WAL.
func verifyPragmas(ctx context.Context, db *sql.DB, readOnly bool) error {
	want := []struct {
		pragma string
		value  string
	}{
		{"journal_mode", "wal"},
		{"busy_timeout", fmt.Sprint(BusyTimeout.Milliseconds())},
		{"foreign_keys", "1"},
		{"synchronous", "1"}, // NORMAL
	}
	if readOnly {
		want = append(want, struct{ pragma, value string }{"query_only", "1"})
	}
	for _, p := range want {
		var got string
		if err := db.QueryRowContext(ctx, "PRAGMA "+p.pragma).Scan(&got); err != nil {
			return fmt.Errorf("store: read pragma %s: %w", p.pragma, err)
		}
		if got != p.value {
			return fmt.Errorf("store: pragma %s = %q, want %q", p.pragma, got, p.value)
		}
	}
	return nil
}

// writeTx runs fn inside BEGIN IMMEDIATE ... COMMIT on one pinned connection.
//
// It manages the transaction by hand instead of with sql.Tx because SQLite
// can fail COMMIT and keep the transaction open (deferred foreign keys are
// checked at COMMIT). sql.Tx considers itself finished after a failed Commit
// and won't roll back, which would leave the single writer connection stuck
// inside a transaction.
//
// Any exit other than a successful COMMIT, including a panic in fn, rolls back.
func writeTx(ctx context.Context, db *sql.DB, fn func(*sql.Conn) error) error {
	c, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire writer: %w", err)
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// A background context so a cancelled ctx can't skip the rollback.
		if _, rerr := c.ExecContext(context.Background(), "ROLLBACK"); rerr != nil {
			// The connection's state is unknown; drop it from the pool.
			c.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if err := fn(c); err != nil {
		return err
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	committed = true
	return nil
}
