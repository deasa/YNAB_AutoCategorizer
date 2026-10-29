package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const plan = "plan-1"

func openTest(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "finbot.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func ptr[T any](v T) *T { return &v }

func count(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.r.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestOpenAppliesPragmas(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if err := verifyPragmas(ctx, db.w, false); err != nil {
		t.Errorf("writer: %v", err)
	}
	if err := verifyPragmas(ctx, db.r, true); err != nil {
		t.Errorf("reader: %v", err)
	}
	if _, err := db.r.Exec(`INSERT INTO payees (plan_id, id, name) VALUES ('p','x','y')`); err == nil {
		t.Error("read pool accepted a write")
	}
}

func TestOpenFailsFastWithoutPragmas(t *testing.T) {
	// A pool opened without the DSN pragmas must fail verification.
	path := filepath.Join(t.TempDir(), "plain.db")
	plain, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	err = verifyPragmas(context.Background(), plain, false)
	if err == nil || !strings.Contains(err.Error(), "journal_mode") {
		t.Fatalf("verifyPragmas on plain pool = %v, want journal_mode mismatch", err)
	}

	// WAL is impossible in memory, so Open must refuse rather than run degraded.
	if db, err := Open(context.Background(), ":memory:"); err == nil {
		db.Close()
		t.Fatal("Open(:memory:) succeeded, want pragma failure")
	}
}

func TestMigrationsIdempotent(t *testing.T) {
	db, path := openTest(t)
	if n := count(t, db, `SELECT count(*) FROM schema_migrations`); n != 1 {
		t.Fatalf("schema_migrations rows = %d, want 1", n)
	}
	db.Close()

	db2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer db2.Close()
	if err := migrate(context.Background(), db2.w); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	if n := count(t, db2, `SELECT count(*) FROM schema_migrations`); n != 1 {
		t.Fatalf("schema_migrations rows after reopen = %d, want 1", n)
	}
}

func TestMigrationsOrderedAndAtomic(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"migrations/0001_mirror.sql": {Data: []byte(`SELECT 1;`)}, // already applied
		"migrations/0003_c.sql":      {Data: []byte(`CREATE TABLE c (x INTEGER); CREATE TABLE b_dup (x INTEGER);`)},
		"migrations/0002_b.sql":      {Data: []byte(`CREATE TABLE b (x INTEGER); CREATE TABLE b_dup (x INTEGER);`)},
	}
	// 0002 applies; 0003 fails on b_dup and must leave no trace (no table c,
	// no schema_migrations row).
	if err := migrateFS(ctx, db.w, fsys); err == nil || !strings.Contains(err.Error(), "0003_c.sql") {
		t.Fatalf("migrateFS = %v, want failure in 0003", err)
	}
	if n := count(t, db, `SELECT count(*) FROM schema_migrations`); n != 2 {
		t.Errorf("schema_migrations rows = %d, want 2", n)
	}
	if n := count(t, db, `SELECT count(*) FROM sqlite_master WHERE name = 'c'`); n != 0 {
		t.Error("failed migration 0003 left table c behind")
	}

	if _, err := loadMigrations(fstest.MapFS{"migrations/x.sql": {}}); err == nil {
		t.Error("loadMigrations accepted an unnumbered file")
	}
	if _, err := loadMigrations(fstest.MapFS{
		"migrations/0001_a.sql": {}, "migrations/1_b.sql": {},
	}); err == nil {
		t.Error("loadMigrations accepted duplicate versions")
	}
}

func TestConcurrentReadDuringWrite(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if err := db.Update(ctx, func(tx *Tx) error {
		return tx.UpsertPayees(plan, []Payee{{ID: "p1", Name: "Old"}})
	}); err != nil {
		t.Fatal(err)
	}

	inTx := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db.Update(ctx, func(tx *Tx) error {
			if err := tx.UpsertPayees(plan, []Payee{{ID: "p1", Name: "New"}, {ID: "p2", Name: "Two"}}); err != nil {
				return err
			}
			close(inTx)
			<-release
			return nil
		})
	}()
	<-inTx

	// The writer holds an open write transaction; a reader must not block and
	// must see the last committed snapshot.
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var name string
	var n int
	if err := db.r.QueryRowContext(readCtx, `SELECT name, (SELECT count(*) FROM payees) FROM payees WHERE id = 'p1'`).Scan(&name, &n); err != nil {
		t.Fatalf("read during write: %v", err)
	}
	if name != "Old" || n != 1 {
		t.Errorf("read during write = (%q, %d), want (Old, 1)", name, n)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM payees`); n != 2 {
		t.Errorf("payees after commit = %d, want 2", n)
	}
}

func TestSchemaDump(t *testing.T) {
	db, _ := openTest(t)
	rows, err := db.r.Query(`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		b.WriteString(s + ";\n")
	}
	t.Log("\n" + b.String())
}
