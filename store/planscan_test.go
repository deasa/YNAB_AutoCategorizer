package store

import (
	"context"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func TestPlanScansReplace(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 29, 12, 0, 0, 500, time.UTC)
	first := []PlanScan{
		{PlanID: "p1", Name: "Budget 2019", HasDiscretionary: true, ScannedAt: at},
		{PlanID: "p2", Name: "Alpha", ScannedAt: at},
		{PlanID: "p3", Name: "Broken", HasDiscretionary: true, ScanError: "fetch categories: boom", ScannedAt: at},
	}
	if err := db.ReplacePlanScans(ctx, first); err != nil {
		t.Fatal(err)
	}
	got, err := db.PlanScans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].PlanID != "p2" || got[1].PlanID != "p3" || got[2].PlanID != "p1" {
		t.Fatalf("got %+v, want ordered by name", got)
	}
	if got[0].HasDiscretionary || !got[2].HasDiscretionary || !got[2].ScannedAt.Equal(at) {
		t.Errorf("round trip: %+v", got)
	}
	// A failed plan stores NULL, not a guess.
	if got[1].HasDiscretionary || got[1].ScanError != "fetch categories: boom" {
		t.Errorf("failed plan: %+v", got[1])
	}

	// A later scan replaces the table: p1 and p3 gone, p2 changed.
	if err := db.ReplacePlanScans(ctx, []PlanScan{{PlanID: "p2", Name: "Alpha", HasDiscretionary: true, ScannedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.PlanScans(ctx); len(got) != 1 || !got[0].HasDiscretionary {
		t.Errorf("after replace: %+v", got)
	}
}

func TestPlanScansFailureKeepsLastGoodVerdict(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	good := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	later := good.Add(24 * time.Hour)
	latest := later.Add(24 * time.Hour)
	byID := func() map[string]PlanScan {
		t.Helper()
		got, err := db.PlanScans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]PlanScan{}
		for _, p := range got {
			m[p.PlanID] = p
		}
		return m
	}

	if err := db.ReplacePlanScans(ctx, []PlanScan{
		{PlanID: "yes", Name: "Yes", HasDiscretionary: true, ScannedAt: good},
		{PlanID: "no", Name: "No", ScannedAt: good},
	}); err != nil {
		t.Fatal(err)
	}
	// Two failures in a row still point back at the last success.
	for _, at := range []time.Time{later, latest} {
		if err := db.ReplacePlanScans(ctx, []PlanScan{
			{PlanID: "yes", Name: "Yes renamed", ScanError: "boom", ScannedAt: at},
			{PlanID: "no", Name: "No", ScanError: "boom", ScannedAt: at},
			{PlanID: "new", Name: "New", ScanError: "boom", ScannedAt: at},
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := byID()
	if p := got["yes"]; !p.HasDiscretionary || !p.ScannedAt.Equal(good) || p.ScanError != "boom" || p.Name != "Yes renamed" {
		t.Errorf("yes after failures: %+v", p)
	}
	if p := got["no"]; p.HasDiscretionary || !p.ScannedAt.Equal(good) || p.ScanError != "boom" {
		t.Errorf("no after failures: %+v", p)
	}
	if p := got["new"]; p.HasDiscretionary || p.ScanError != "boom" || !p.ScannedAt.Equal(later) {
		t.Errorf("never-scanned plan: %+v", p)
	}

	// A success clears the error and takes the new verdict and time; a plan
	// that is no longer listed disappears even if its last scan failed.
	if err := db.ReplacePlanScans(ctx, []PlanScan{
		{PlanID: "yes", Name: "Yes", ScannedAt: latest},
		{PlanID: "new", Name: "New", HasDiscretionary: true, ScannedAt: latest},
	}); err != nil {
		t.Fatal(err)
	}
	got = byID()
	if p := got["yes"]; p.HasDiscretionary || p.ScanError != "" || !p.ScannedAt.Equal(latest) {
		t.Errorf("yes after success: %+v", p)
	}
	if p := got["new"]; !p.HasDiscretionary || p.ScanError != "" || !p.ScannedAt.Equal(latest) {
		t.Errorf("new after success: %+v", p)
	}
	if _, ok := got["no"]; ok || len(got) != 2 {
		t.Errorf("unlisted plan survived: %+v", got)
	}

	// An empty scan empties the table.
	if err := db.ReplacePlanScans(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := byID(); len(got) != 0 {
		t.Errorf("after empty scan: %+v", got)
	}
}

// A row written under the 0002 CHECK survives the 0004 rebuild, and a success
// with no verdict is still refused.
func TestPlanScanMigrationKeepsRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "finbot.db")
	w, err := openPool(ctx, path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	all, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatal(err)
	}
	old := fstest.MapFS{}
	for _, m := range all {
		if m.version < 4 {
			old["migrations/"+m.name] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	if err := migrateFS(ctx, w, old); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ExecContext(ctx, `INSERT INTO plan_scan VALUES
		('a','A',1,NULL,'2026-10-01T00:00:00Z'), ('b','B',NULL,'boom','2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, w); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := w.QueryRowContext(ctx, `SELECT count(*) FROM plan_scan
		WHERE (plan_id='a' AND has_discretionary=1 AND scan_error IS NULL)
		   OR (plan_id='b' AND has_discretionary IS NULL AND scan_error='boom')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rows kept = %d, want 2", n)
	}
	if _, err := w.ExecContext(ctx, `INSERT INTO plan_scan VALUES ('c','C',NULL,NULL,'x')`); err == nil {
		t.Error("a row with neither verdict nor error was accepted")
	}
}
