package store

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func TestMigration0006OnExisting0005DB(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "finbot.db")
	w, err := openPool(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	upTo5 := fstest.MapFS{}
	for _, name := range []string{"0001_mirror", "0002_plan_scan", "0003_status_message", "0004_plan_scan_keep_verdict", "0005_oauth"} {
		body, err := fs.ReadFile(migrationFS, "migrations/"+name+".sql")
		if err != nil {
			t.Fatal(err)
		}
		upTo5["migrations/"+name+".sql"] = &fstest.MapFile{Data: body}
	}
	if err := migrateFS(ctx, w, upTo5); err != nil {
		t.Fatalf("apply through 0005: %v", err)
	}
	if _, err := w.ExecContext(ctx, `INSERT INTO sync_state VALUES ('p', 7, '2026-10-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	w.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open on a 0005 DB: %v", err)
	}
	defer db.Close()
	if n := count(t, db, `SELECT count(*) FROM schema_migrations WHERE version = 6`); n != 1 {
		t.Errorf("0006 rows in schema_migrations = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM decisions`); n != 0 {
		t.Errorf("decisions rows = %d, want 0", n)
	}
	if st, ok, err := db.GetSyncState(ctx, "p"); err != nil || !ok || st.ServerKnowledge != 7 {
		t.Errorf("pre-existing sync_state lost: %+v %v %v", st, ok, err)
	}
}

func decision(txn, category string) Decision {
	return Decision{
		PlanID: plan, TxnID: txn, CategoryID: ptr("id-" + category), CategoryName: ptr(category),
		Confidence: ptr(0.9), RunnerUp: ptr("Dining"), Method: MethodModel, Model: "jev-1.14.0",
		PolicyHash: "pol", CandidatesHash: "cand", Mode: ModeDryRun, Outcome: OutcomeWouldApply,
		Features: json.RawMessage(`{"payee":"costco"}`),
	}
}

func TestInsertDecisionSupersedesAndKeepsHistory(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

	first := decision("t1", "Groceries")
	first.CreatedAt = at
	first.SecondOpinion = json.RawMessage(`{"model":"claude"}`)
	id1, err := db.InsertDecision(ctx, first)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if got, ok, err := db.LatestDecision(ctx, plan, "t1"); err != nil || !ok || got.ID != id1 || got.SupersededBy != nil {
		t.Fatalf("latest after first = %+v, %v, %v", got, ok, err)
	}
	id2, err := db.InsertDecision(ctx, decision("t1", "Dining"))
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if _, err := db.InsertDecision(ctx, decision("t2", "Fuel")); err != nil {
		t.Fatalf("other txn: %v", err)
	}

	hist, err := db.DecisionHistory(ctx, plan, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || hist[0].ID != id1 || hist[1].ID != id2 {
		t.Fatalf("history = %+v, want first then second", hist)
	}
	old, cur := hist[0], hist[1]
	if old.SupersededBy == nil || *old.SupersededBy != id2 {
		t.Errorf("first superseded_by = %v, want %d", old.SupersededBy, id2)
	}
	if cur.SupersededBy != nil {
		t.Errorf("second superseded_by = %v, want nil", *cur.SupersededBy)
	}
	if !old.CreatedAt.Equal(at) || *old.CategoryName != "Groceries" || *old.Confidence != 0.9 || old.Method != MethodModel ||
		string(old.SecondOpinion) != `{"model":"claude"}` || string(old.Features) != `{"payee":"costco"}` {
		t.Errorf("first round-trip = %+v", old)
	}
	if cur.SecondOpinion != nil || cur.CreatedAt.IsZero() {
		t.Errorf("second round-trip = %+v", cur)
	}
	if got, ok, err := db.LatestDecision(ctx, plan, "t1"); err != nil || !ok || got.ID != id2 {
		t.Errorf("latest = %+v, %v, %v; want the second", got, ok, err)
	}
	if n := count(t, db, `SELECT count(*) FROM decisions WHERE superseded_by IS NULL`); n != 2 {
		t.Errorf("current rows = %d, want one each for t1 and t2", n)
	}
}

func TestDecisionNoChoiceStoresNulls(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	d := decision("t1", "x")
	d.CategoryID, d.CategoryName, d.Confidence, d.RunnerUp = nil, nil, nil, nil
	d.Outcome = OutcomeSkipped
	if _, err := db.InsertDecision(ctx, d); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.LatestDecision(ctx, plan, "t1")
	if err != nil || !ok {
		t.Fatalf("latest: %v %v", ok, err)
	}
	if got.CategoryID != nil || got.CategoryName != nil || got.Confidence != nil || got.RunnerUp != nil {
		t.Errorf("want nulls, got %+v", got)
	}
}

func TestDecisionsAreKeyedByPlan(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	a := decision("t1", "Groceries")
	b := decision("t1", "Dining")
	b.PlanID = "plan-2"
	if _, err := db.InsertDecision(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertDecision(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, ok, err := db.LatestDecision(ctx, plan, "t1")
	if err != nil || !ok || got.SupersededBy != nil || *got.CategoryName != "Groceries" {
		t.Errorf("plan-1's decision was touched by plan-2: %+v, %v, %v", got, ok, err)
	}
}

func TestLatestDecisionNotFound(t *testing.T) {
	db, _ := openTest(t)
	if _, ok, err := db.LatestDecision(context.Background(), plan, "nope"); err != nil || ok {
		t.Errorf("latest = %v, %v; want not found", ok, err)
	}
}

func TestInsertDecisionRejectsBadValues(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	for name, mutate := range map[string]func(*Decision){
		"method":         func(d *Decision) { d.Method = "guess" },
		"mode":           func(d *Decision) { d.Mode = "live" },
		"outcome":        func(d *Decision) { d.Outcome = "done" },
		"features":       func(d *Decision) { d.Features = json.RawMessage(`{`) },
		"no features":    func(d *Decision) { d.Features = nil },
		"second_opinion": func(d *Decision) { d.SecondOpinion = json.RawMessage(`nope`) },
	} {
		d := decision("t1", "Groceries")
		mutate(&d)
		if _, err := db.InsertDecision(ctx, d); err == nil {
			t.Errorf("%s: insert succeeded, want error", name)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM decisions`); n != 0 {
		t.Errorf("rejected inserts left %d rows", n)
	}
}

func TestFailedInsertLeavesPreviousDecisionCurrent(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if _, err := db.InsertDecision(ctx, decision("t1", "Groceries")); err != nil {
		t.Fatal(err)
	}
	bad := decision("t1", "Dining")
	bad.Outcome = "done"
	if _, err := db.InsertDecision(ctx, bad); err == nil {
		t.Fatal("bad insert succeeded")
	}
	got, ok, err := db.LatestDecision(ctx, plan, "t1")
	if err != nil || !ok || got.SupersededBy != nil || *got.CategoryName != "Groceries" {
		t.Errorf("latest = %+v, %v, %v; want the first, still current", got, ok, err)
	}
}

func TestHasCurrentDecision(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if _, err := db.InsertDecision(ctx, decision("t1", "Groceries")); err != nil {
		t.Fatal(err)
	}
	dryRun := DecisionKey{Model: "jev-1.14.0", PolicyHash: "pol", CandidatesHash: "cand", Mode: ModeDryRun}

	check := func(name, txn string, k DecisionKey, want bool) {
		t.Helper()
		got, err := db.HasCurrentDecision(ctx, plan, txn, k)
		if err != nil || got != want {
			t.Errorf("%s: got %v, %v; want %v", name, got, err, want)
		}
	}
	check("exact match", "t1", dryRun, true)
	check("other txn", "t2", dryRun, false)

	auto := dryRun
	auto.Mode = ModeAuto
	check("dry_run is not current for auto", "t1", auto, false)

	other := dryRun
	other.Model = "jev-1.15.0"
	check("other model", "t1", other, false)
	other = dryRun
	other.PolicyHash = "pol2"
	check("other policy", "t1", other, false)
	other = dryRun
	other.CandidatesHash = "cand2"
	check("other candidates", "t1", other, false)

	// Once superseded, the old key is no longer current.
	next := decision("t1", "Dining")
	next.Model = "jev-1.15.0"
	if _, err := db.InsertDecision(ctx, next); err != nil {
		t.Fatal(err)
	}
	check("superseded key", "t1", dryRun, false)
}
