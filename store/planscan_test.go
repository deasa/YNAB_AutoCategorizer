package store

import (
	"context"
	"testing"
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

	// A later scan replaces the table: p1 gone, p2 changed.
	if err := db.ReplacePlanScans(ctx, []PlanScan{{PlanID: "p2", Name: "Alpha", HasDiscretionary: true, ScannedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.PlanScans(ctx); len(got) != 1 || !got[0].HasDiscretionary {
		t.Errorf("after replace: %+v", got)
	}
}
