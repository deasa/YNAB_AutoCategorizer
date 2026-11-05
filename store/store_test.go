package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

const plan = "plan-1"

var testSyncedAt = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

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
	if n := count(t, db, `SELECT count(*) FROM schema_migrations`); n != 4 {
		t.Fatalf("schema_migrations rows = %d, want 4", n)
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
	if n := count(t, db2, `SELECT count(*) FROM schema_migrations`); n != 4 {
		t.Fatalf("schema_migrations rows after reopen = %d, want 4", n)
	}
}

// A DB created before 0002 existed (only 0001 applied) picks up 0002 on the
// next Open, keeping its mirror rows.
func TestMigration0002OnExisting0001DB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "finbot.db")
	w, err := openPool(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := fs.ReadFile(migrationFS, "migrations/0001_mirror.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateFS(ctx, w, fstest.MapFS{"migrations/0001_mirror.sql": {Data: body}}); err != nil {
		t.Fatalf("apply 0001 only: %v", err)
	}
	if _, err := w.ExecContext(ctx, `INSERT INTO sync_state VALUES ('p', 7, '2026-10-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	w.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open on a 0001 DB: %v", err)
	}
	defer db.Close()
	if n := count(t, db, `SELECT count(*) FROM schema_migrations`); n != 4 {
		t.Errorf("schema_migrations rows = %d, want 4", n)
	}
	if st, ok, err := db.GetSyncState(ctx, "p"); err != nil || !ok || st.ServerKnowledge != 7 {
		t.Errorf("pre-existing sync_state lost: %+v %v %v", st, ok, err)
	}
	if got, err := db.PlanScans(ctx); err != nil || got != nil {
		t.Errorf("PlanScans on fresh table = %v, %v; want nil, nil", got, err)
	}
}

func TestMigrationsOrderedAndAtomic(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"migrations/0001_mirror.sql": {Data: []byte(`SELECT 1;`)}, // already applied
		"migrations/0002_scan.sql":   {Data: []byte(`SELECT 1;`)}, // already applied
		"migrations/0101_c.sql":      {Data: []byte(`CREATE TABLE c (x INTEGER); CREATE TABLE b_dup (x INTEGER);`)},
		"migrations/0100_b.sql":      {Data: []byte(`CREATE TABLE b (x INTEGER); CREATE TABLE b_dup (x INTEGER);`)},
	}
	// 0100 applies; 0101 fails on b_dup and must leave no trace (no table c,
	// no schema_migrations row).
	if err := migrateFS(ctx, db.w, fsys); err == nil || !strings.Contains(err.Error(), "0101_c.sql") {
		t.Fatalf("migrateFS = %v, want failure in 0101", err)
	}
	if n := count(t, db, `SELECT count(*) FROM schema_migrations`); n != 5 {
		t.Errorf("schema_migrations rows = %d, want 5", n)
	}
	if n := count(t, db, `SELECT count(*) FROM sqlite_master WHERE name = 'c'`); n != 0 {
		t.Error("failed migration 0101 left table c behind")
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

func TestOpenEscapesURIPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "odd #1 ?x %41")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "finbot.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database not created at the literal path: %v", err)
	}
}

func TestSnapshot_ConsistentCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := writeTx(ctx, db.w, func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, "CREATE TABLE snap_test (v TEXT); INSERT INTO snap_test VALUES ('a'), ('b')")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "snap.db")
	if err := db.Snapshot(ctx, out); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := db.Snapshot(ctx, out); err == nil {
		t.Error("Snapshot over an existing file succeeded, want an error")
	}
	copyDB, err := sql.Open("sqlite", "file:"+out+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var n int
	if err := copyDB.QueryRow("SELECT COUNT(*) FROM snap_test").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("snapshot has %d rows, want 2", n)
	}
}

func wantMode0600(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s: mode %o, want 600", filepath.Base(p), got)
		}
	}
}

func writeSomething(t *testing.T, db *DB) {
	t.Helper()
	err := db.ReplacePlanScans(context.Background(), []PlanScan{{PlanID: "p", Name: "P", ScannedAt: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenNewDatabaseIsPrivate(t *testing.T) {
	db, path := openTest(t)
	wantMode0600(t, path)
	writeSomething(t, db)
	wantMode0600(t, path, path+"-wal", path+"-shm")
}

// An existing database left at 0644 with a live WAL, as on hoid: a copy taken
// while it's open has all three files.
func TestOpenTightensExistingDatabase(t *testing.T) {
	ctx := context.Background()
	src, srcPath := openTest(t)
	writeSomething(t, src)
	path := filepath.Join(t.TempDir(), "finbot.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		body, err := os.ReadFile(srcPath + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+suffix, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path+suffix, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	wantMode0600(t, path, path+"-wal", path+"-shm")
	writeSomething(t, db)
	wantMode0600(t, path, path+"-wal", path+"-shm")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	writeSomething(t, db)
	wantMode0600(t, path, path+"-wal", path+"-shm")
}

func TestOpenFailsIfFilesCantBeTightened(t *testing.T) {
	path := filepath.Join(t.TempDir(), "finbot.db")
	// A symlink to itself: chmod fails with ELOOP, which isn't "doesn't exist".
	if err := os.Symlink(filepath.Base(path)+"-wal", path+"-wal"); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), path)
	if err == nil {
		db.Close()
		t.Fatal("Open succeeded with a -wal file it can't chmod")
	}
}

func TestMakePrivateSkipsMemory(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := makePrivate(":memory:"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(":memory:"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("created a file named :memory: (%v)", err)
	}
}
