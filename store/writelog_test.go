package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func TestMigration0007OnExisting0006DB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "finbot.db")
	w, err := openPool(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	upTo6 := fstest.MapFS{}
	for _, name := range []string{"0001_mirror", "0002_plan_scan", "0003_status_message", "0004_plan_scan_keep_verdict",
		"0005_oauth", "0006_decisions"} {
		body, err := fs.ReadFile(migrationFS, "migrations/"+name+".sql")
		if err != nil {
			t.Fatal(err)
		}
		upTo6["migrations/"+name+".sql"] = &fstest.MapFile{Data: body}
	}
	if err := migrateFS(ctx, w, upTo6); err != nil {
		t.Fatalf("apply through 0006: %v", err)
	}
	if _, err := w.ExecContext(ctx, `INSERT INTO sync_state VALUES ('p', 7, '2026-10-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	w.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open on a 0006 DB: %v", err)
	}
	defer db.Close()
	if n := count(t, db, `SELECT count(*) FROM schema_migrations WHERE version = 7`); n != 1 {
		t.Errorf("0007 rows in schema_migrations = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM write_log`) + count(t, db, `SELECT count(*) FROM human_locks`); n != 0 {
		t.Errorf("new tables hold %d rows, want 0", n)
	}
	if st, ok, err := db.GetSyncState(ctx, "p"); err != nil || !ok || st.ServerKnowledge != 7 {
		t.Errorf("pre-existing sync_state lost: %+v %v %v", st, ok, err)
	}
}

func writeIntent(txn string) WriteEntry {
	return WriteEntry{
		PlanID: plan, Actor: ActorSystem, Via: ViaScheduler, Tier: "routine", Endpoint: "PATCH /plans/{id}/transactions",
		TxnID: ptr(txn), CategoryID: ptr("cat-1"), BeforeJSON: json.RawMessage(`{"category_id": null}`),
		AfterJSON: json.RawMessage(`{"category_id": "cat-1"}`), RequestID: ptr("req-1"),
	}
}

func insertIntent(t *testing.T, db *DB, e WriteEntry) int64 {
	t.Helper()
	id, err := db.InsertWriteIntent(context.Background(), e)
	if err != nil {
		t.Fatalf("InsertWriteIntent: %v", err)
	}
	return id
}

func lastDone(t *testing.T, db *DB, planID, txn string) (WriteEntry, bool) {
	t.Helper()
	e, ok, err := db.LastDoneWrite(context.Background(), planID, txn)
	if err != nil {
		t.Fatalf("LastDoneWrite: %v", err)
	}
	return e, ok
}

func unfinished(t *testing.T, db *DB) []WriteEntry {
	t.Helper()
	got, err := db.UnfinishedWrites(context.Background())
	if err != nil {
		t.Fatalf("UnfinishedWrites: %v", err)
	}
	return got
}

func TestWriteIntentToDoneKeepsEveryField(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	e := writeIntent("t1")
	e.Actor, e.Via, e.CreatedAt = ActorWife, ViaSlack, at
	id := insertIntent(t, db, e)
	if err := db.MarkWriteDone(ctx, id, nil); err != nil {
		t.Fatalf("MarkWriteDone: %v", err)
	}

	got, ok := lastDone(t, db, plan, "t1")
	if !ok {
		t.Fatal("no done write")
	}
	if got.ID != id || got.PlanID != plan || got.Actor != ActorWife || got.Via != ViaSlack || got.Tier != "routine" ||
		got.Endpoint != e.Endpoint || *got.TxnID != "t1" || *got.CategoryID != "cat-1" || *got.RequestID != "req-1" ||
		got.Status != WriteDone || got.Error != nil || !got.CreatedAt.Equal(at) || got.RevertedBy != nil {
		t.Errorf("round trip = %+v", got)
	}
	// Byte for byte: a revert replays exactly what was stored.
	if string(got.BeforeJSON) != string(e.BeforeJSON) || string(got.AfterJSON) != string(e.AfterJSON) {
		t.Errorf("json = %s, %s", got.BeforeJSON, got.AfterJSON)
	}
}

func TestMarkWriteDoneStoresWhatYNABReturned(t *testing.T) {
	db, _ := openTest(t)
	id := insertIntent(t, db, writeIntent("t1"))
	actual := json.RawMessage(`{"category_id": "cat-1", "memo": "normalized"}`)
	if err := db.MarkWriteDone(context.Background(), id, actual); err != nil {
		t.Fatal(err)
	}
	if got, _ := lastDone(t, db, plan, "t1"); string(got.AfterJSON) != string(actual) {
		t.Errorf("after = %s", got.AfterJSON)
	}
}

func TestWriteIntentToFailed(t *testing.T) {
	db, _ := openTest(t)
	id := insertIntent(t, db, writeIntent("t1"))
	if err := db.MarkWriteFailed(context.Background(), id, "429 rate limited"); err != nil {
		t.Fatalf("MarkWriteFailed: %v", err)
	}
	if _, ok := lastDone(t, db, plan, "t1"); ok {
		t.Error("a failed write counted as done")
	}
	if got := unfinished(t, db); len(got) != 0 {
		t.Errorf("failed write still unfinished: %+v", got)
	}
	var status, msg, after string
	if err := db.r.QueryRow(`SELECT status, error, after_json FROM write_log WHERE id = ?`, id).Scan(&status, &msg, &after); err != nil {
		t.Fatal(err)
	}
	if status != WriteFailed || msg != "429 rate limited" || after != `{"category_id": "cat-1"}` {
		t.Errorf("row = %q %q %q", status, msg, after)
	}
}

func TestFinishedWriteCannotTransition(t *testing.T) {
	ctx := context.Background()
	replacement := json.RawMessage(`{"category_id": "other"}`)
	transitions := map[string]func(*DB, int64) error{
		"done":   func(db *DB, id int64) error { return db.MarkWriteDone(ctx, id, replacement) },
		"failed": func(db *DB, id int64) error { return db.MarkWriteFailed(ctx, id, "late") },
	}
	for first, finish := range transitions {
		for second, again := range transitions {
			t.Run(first+" then "+second, func(t *testing.T) {
				db, _ := openTest(t)
				id := insertIntent(t, db, writeIntent("t1"))
				if err := finish(db, id); err != nil {
					t.Fatal(err)
				}
				var before [3]string
				row := `SELECT status, COALESCE(error, ''), after_json FROM write_log WHERE id = ?`
				if err := db.r.QueryRow(row, id).Scan(&before[0], &before[1], &before[2]); err != nil {
					t.Fatal(err)
				}
				if err := again(db, id); !errors.Is(err, ErrWriteNotIntent) {
					t.Fatalf("second transition: %v, want ErrWriteNotIntent", err)
				}
				var after [3]string
				if err := db.r.QueryRow(row, id).Scan(&after[0], &after[1], &after[2]); err != nil {
					t.Fatal(err)
				}
				if before != after {
					t.Errorf("row changed: %v -> %v", before, after)
				}
			})
		}
	}
}

func TestMarkUnknownWrite(t *testing.T) {
	db, _ := openTest(t)
	if err := db.MarkWriteDone(context.Background(), 99, nil); !errors.Is(err, ErrWriteNotIntent) {
		t.Errorf("err = %v, want ErrWriteNotIntent", err)
	}
}

func TestMarkWriteDoneRejectsBadJSONAndStaysOpen(t *testing.T) {
	db, _ := openTest(t)
	id := insertIntent(t, db, writeIntent("t1"))
	if err := db.MarkWriteDone(context.Background(), id, json.RawMessage(`{broken`)); err == nil {
		t.Fatal("accepted invalid after_json")
	}
	if got := unfinished(t, db); len(got) != 1 || got[0].ID != id {
		t.Errorf("unfinished = %+v", got)
	}
}

func TestInsertWriteIntentRejectsBadRows(t *testing.T) {
	cases := map[string]func(*WriteEntry){
		"actor":       func(e *WriteEntry) { e.Actor = "mallory" },
		"via":         func(e *WriteEntry) { e.Via = "email" },
		"no tier":     func(e *WriteEntry) { e.Tier = "" },
		"no endpoint": func(e *WriteEntry) { e.Endpoint = "" },
		"no before":   func(e *WriteEntry) { e.BeforeJSON = nil },
		"no after":    func(e *WriteEntry) { e.AfterJSON = nil },
		"bad before":  func(e *WriteEntry) { e.BeforeJSON = json.RawMessage(`{`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			db, _ := openTest(t)
			e := writeIntent("t1")
			mutate(&e)
			if _, err := db.InsertWriteIntent(context.Background(), e); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestWriteIntentForCreateHasNoTxn(t *testing.T) {
	db, _ := openTest(t)
	e := writeIntent("unused")
	e.TxnID, e.BeforeJSON = nil, json.RawMessage(`null`)
	insertIntent(t, db, e)
	got := unfinished(t, db)
	if len(got) != 1 || got[0].TxnID != nil || string(got[0].BeforeJSON) != "null" {
		t.Errorf("unfinished = %+v", got)
	}
}

func TestLastDoneWriteSkipsOtherRows(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if _, ok := lastDone(t, db, plan, "t1"); ok {
		t.Error("done write on an empty log")
	}

	older := insertIntent(t, db, writeIntent("t1"))
	newest := insertIntent(t, db, writeIntent("t1"))
	for _, id := range []int64{older, newest} {
		if err := db.MarkWriteDone(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	failed := insertIntent(t, db, writeIntent("t1"))
	if err := db.MarkWriteFailed(ctx, failed, "boom"); err != nil {
		t.Fatal(err)
	}
	insertIntent(t, db, writeIntent("t1")) // still open
	if err := db.MarkWriteDone(ctx, insertIntent(t, db, writeIntent("t2")), nil); err != nil {
		t.Fatal(err)
	}

	if got, ok := lastDone(t, db, plan, "t1"); !ok || got.ID != newest {
		t.Errorf("last done = %d %v, want %d", got.ID, ok, newest)
	}
	if _, ok := lastDone(t, db, "other-plan", "t1"); ok {
		t.Error("a write leaked across plans")
	}
}

func TestUnfinishedWritesSurviveACrash(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "finbot.db")
	crashed, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { crashed.Close() })

	first := insertIntent(t, crashed, writeIntent("t1"))
	finished := insertIntent(t, crashed, writeIntent("t2"))
	if err := crashed.MarkWriteDone(ctx, finished, nil); err != nil {
		t.Fatal(err)
	}
	other := writeIntent("t3")
	other.PlanID = "other-plan"
	second := insertIntent(t, crashed, other)

	// No Close: what the process had committed is all a restart can see.
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	got := unfinished(t, reopened)
	if len(got) != 2 || got[0].ID != first || got[1].ID != second {
		t.Fatalf("unfinished = %+v, want ids %d and %d", got, first, second)
	}
	if err := reopened.MarkWriteFailed(ctx, first, "unknown outcome"); err != nil {
		t.Fatal(err)
	}
	if got := unfinished(t, reopened); len(got) != 1 || got[0].ID != second {
		t.Errorf("after resolving one: %+v", got)
	}
}

func locks(t *testing.T, db *DB, planID string, txns ...string) map[string][]HumanLock {
	t.Helper()
	got, err := db.Locks(context.Background(), planID, txns)
	if err != nil {
		t.Fatalf("Locks: %v", err)
	}
	return got
}

func TestLockIsIdempotentAndKeepsOriginalSource(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	if err := db.Lock(ctx, HumanLock{PlanID: plan, TxnID: "t1", Field: "category", Source: LockFirstTouch, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{LockFirstTouch, LockSlack} {
		if err := db.Lock(ctx, HumanLock{PlanID: plan, TxnID: "t1", Field: "category", Source: source}); err != nil {
			t.Fatalf("re-lock from %s: %v", source, err)
		}
	}

	got := locks(t, db, plan, "t1")["t1"]
	if len(got) != 1 || got[0].Source != LockFirstTouch || !got[0].CreatedAt.Equal(at) ||
		got[0].PlanID != plan || got[0].TxnID != "t1" || got[0].Field != "category" {
		t.Errorf("locks = %+v", got)
	}
}

func TestLockKeysArePerPlanTxnAndField(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	for _, l := range []HumanLock{
		{PlanID: plan, TxnID: "t1", Field: "memo", Source: LockYNABEdit},
		{PlanID: plan, TxnID: "t1", Field: "category", Source: LockSlack},
		{PlanID: plan, TxnID: "t2", Field: "category", Source: LockFirstTouch},
		{PlanID: plan, TxnID: "t3", Field: "category", Source: LockFirstTouch},
		{PlanID: "other-plan", TxnID: "t1", Field: "payee", Source: LockFirstTouch},
	} {
		if err := db.Lock(ctx, l); err != nil {
			t.Fatal(err)
		}
	}

	got := locks(t, db, plan, "t1", "t2", "no-locks")
	if len(got) != 2 {
		t.Fatalf("got %d txns, want 2: %+v", len(got), got)
	}
	if t1 := got["t1"]; len(t1) != 2 || t1[0].Field != "category" || t1[0].Source != LockSlack || t1[1].Field != "memo" {
		t.Errorf("t1 = %+v", t1)
	}
	if t2 := got["t2"]; len(t2) != 1 || t2[0].Source != LockFirstTouch {
		t.Errorf("t2 = %+v", t2)
	}
}

func TestLockRejectsUnknownSource(t *testing.T) {
	db, _ := openTest(t)
	if err := db.Lock(context.Background(), HumanLock{PlanID: plan, TxnID: "t1", Field: "memo", Source: "guess"}); err == nil {
		t.Error("accepted an unknown source")
	}
}

func TestLocksEmptySet(t *testing.T) {
	db, _ := openTest(t)
	if got := locks(t, db, plan); got == nil || len(got) != 0 {
		t.Errorf("locks = %#v, want an empty map", got)
	}
}

func TestLocksHandlesMoreIDsThanSQLiteVariables(t *testing.T) {
	db, _ := openTest(t)
	if err := db.Lock(context.Background(), HumanLock{PlanID: plan, TxnID: "t39999", Field: "memo", Source: LockSlack}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 40000)
	for i := range ids {
		ids[i] = fmt.Sprintf("t%d", i)
	}
	if got := locks(t, db, plan, ids...); len(got["t39999"]) != 1 {
		t.Errorf("locks = %+v", got)
	}
}

func TestTxLockCommitsAndRollsBackWithTheTransaction(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	l := HumanLock{PlanID: plan, TxnID: "t1", Field: LockFieldCategory, Source: LockFirstTouch}

	boom := errors.New("boom")
	err := db.Update(ctx, func(tx *Tx) error {
		if err := tx.SetSyncState(SyncState{PlanID: plan, ServerKnowledge: 5, SyncedAt: testSyncedAt}); err != nil {
			return err
		}
		if _, err := tx.Lock(l); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update = %v", err)
	}
	if got := locks(t, db, plan, "t1"); len(got) != 0 {
		t.Errorf("lock survived the rollback: %+v", got)
	}

	err = db.Update(ctx, func(tx *Tx) error {
		added, err := tx.Lock(l)
		if err != nil || !added {
			t.Errorf("first Lock = %v, %v; want true", added, err)
		}
		l.Source = LockSlack
		added, err = tx.Lock(l)
		if err != nil || added {
			t.Errorf("repeat Lock = %v, %v; want false", added, err)
		}
		return tx.SetSyncState(SyncState{PlanID: plan, ServerKnowledge: 6, SyncedAt: testSyncedAt})
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := locks(t, db, plan, "t1")["t1"]; len(got) != 1 || got[0].Source != LockFirstTouch {
		t.Errorf("locks = %+v", got)
	}
}

func TestLabeledHistoryReturnsSlackLockedLabelOnceMirrored(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	unanswered := labeled("t1", "groc")
	unanswered.Approved, unanswered.CategoryID = false, nil
	seedMirror(t, db, plan, []Transaction{unanswered}, nil)

	// The writer's order for a Slack answer: intent, done, then the lock.
	e := writeIntent("t1")
	e.Actor, e.Via = ActorBrendan, ViaSlack
	if err := db.MarkWriteDone(ctx, insertIntent(t, db, e), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Lock(ctx, HumanLock{PlanID: plan, TxnID: "t1", Field: LockFieldCategory, Source: LockSlack}); err != nil {
		t.Fatal(err)
	}
	if got := history(t, db, []string{plan}, nil, nil); len(got) != 0 {
		t.Fatalf("a lock alone produced a label: %+v", got)
	}

	seedMirror(t, db, plan, []Transaction{labeled("t1", "groc")}, nil)
	got := history(t, db, []string{plan}, nil, nil)
	if len(got) != 1 || got[0].Category != "Groceries" {
		t.Errorf("history = %+v", got)
	}
}

func TestLabeledHistoryCountsFinbotWritesOnlyWithACategoryLock(t *testing.T) {
	ctx := context.Background()
	const (
		none = iota
		open
		done
		failed
	)
	cases := []struct {
		name      string
		write     int
		writePlan string
		lock      *HumanLock
		want      int
	}{
		{name: "no write, no lock: a human label", write: none, want: 1},
		{name: "finbot's write echoed back", write: done, want: 0},
		{name: "open intent may have landed", write: open, want: 0},
		{name: "failed write changed nothing", write: failed, want: 1},
		{name: "write in another plan", write: done, writePlan: "other-plan", want: 1},
		{name: "human changed the category after finbot", write: done,
			lock: &HumanLock{Field: LockFieldCategory, Source: LockYNABEdit}, want: 1},
		{name: "answered in Slack", write: done, lock: &HumanLock{Field: LockFieldCategory, Source: LockSlack}, want: 1},
		{name: "human categorized, then finbot wrote the memo", write: done,
			lock: &HumanLock{Field: LockFieldCategory, Source: LockFirstTouch}, want: 1},
		{name: "human edited only the memo", write: done, lock: &HumanLock{Field: "memo", Source: LockYNABEdit}, want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, _ := openTest(t)
			seedMirror(t, db, plan, []Transaction{labeled("t1", "groc")}, nil)
			if c.write != none {
				e := writeIntent("t1")
				if c.writePlan != "" {
					e.PlanID = c.writePlan
				}
				id := insertIntent(t, db, e)
				var err error
				switch c.write {
				case done:
					err = db.MarkWriteDone(ctx, id, nil)
				case failed:
					err = db.MarkWriteFailed(ctx, id, "boom")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if c.lock != nil {
				c.lock.PlanID, c.lock.TxnID = plan, "t1"
				if err := db.Lock(ctx, *c.lock); err != nil {
					t.Fatal(err)
				}
			}
			if got := history(t, db, []string{plan}, nil, nil); len(got) != c.want {
				t.Errorf("got %d labels, want %d: %+v", len(got), c.want, got)
			}
		})
	}
}
