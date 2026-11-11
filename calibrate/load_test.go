package calibrate

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

func sp(s string) *string { return &s }

func seed(t *testing.T, db *store.DB, planID string, cats []store.Category, txns []store.Transaction) {
	t.Helper()
	err := db.Update(context.Background(), func(tx *store.Tx) error {
		if err := tx.UpsertAccounts(planID, []store.Account{{ID: "a1", Name: "Checking", Type: "checking", OnBudget: true}}); err != nil {
			return err
		}
		if err := tx.UpsertCategoryGroups(planID, []store.CategoryGroup{
			{ID: "g1", Name: "Everyday"},
			{ID: "gcc", Name: "Credit Card Payments"},
			{ID: "gint", Name: "Internal Master Category", Internal: true},
		}); err != nil {
			return err
		}
		if err := tx.UpsertCategories(planID, cats); err != nil {
			return err
		}
		return tx.UpsertTransactions(planID, txns, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func txn(id, date, payee, cat string) store.Transaction {
	return store.Transaction{ID: id, Date: date, Amount: -5000, Cleared: "cleared", Approved: true, AccountID: "a1",
		ImportPayeeName: sp(payee), CategoryID: sp(cat)}
}

func TestInputsLabelSet(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seed(t, db, "active", []store.Category{
		{ID: "dine", CategoryGroupID: "g1", Name: "Dining"},
		{ID: "disc", CategoryGroupID: "g1", Name: "Discretionary"},
		{ID: "xmas", CategoryGroupID: "g1", Name: "Christmas Fund"},
		{ID: "rta", CategoryGroupID: "gint", Name: "Inflow: Ready to Assign", Internal: true},
		{ID: "visa", CategoryGroupID: "gcc", Name: "Visa"},
	}, []store.Transaction{
		txn("t1", "2026-10-01", "Cafe", "dine"),
		txn("t2", "2026-10-02", "Toy Store", "xmas"),
		txn("t3", "2026-10-03", "Employer", "rta"),
		txn("t4", "2026-10-04", "Card Payment", "visa"),
	})
	seed(t, db, "old", []store.Category{
		{ID: "groc", CategoryGroupID: "g1", Name: "Groceries"},
		{ID: "disc", CategoryGroupID: "g1", Name: "Discretionary"},
		{ID: "xmas2", CategoryGroupID: "g1", Name: "Christmas"},
	}, []store.Transaction{
		txn("o1", "2025-01-01", "Market", "groc"),
		txn("o2", "2025-01-02", "Cinema", "disc"),
		txn("o3", "2024-12-20", "Mall", "xmas2"),
	})
	seed(t, db, "nodisc", []store.Category{{ID: "dine", CategoryGroupID: "g1", Name: "Dining"}},
		[]store.Transaction{txn("n1", "2023-01-01", "Diner", "dine")})
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := db.ReplacePlanScans(ctx, []store.PlanScan{
		{PlanID: "active", Name: "Active", HasDiscretionary: true, ScannedAt: now},
		{PlanID: "old", Name: "Old", HasDiscretionary: true, ScannedAt: now},
		{PlanID: "nodisc", Name: "No Disc", ScannedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	p, err := classify.LoadPolicy([]byte(`{"version":1,"set_aside_keywords":["Christmas"],"descriptions":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	history, set, err := Inputs(ctx, db, "active", p)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range history {
		got = append(got, h.Category+"/"+map[bool]string{true: "active", false: "hard"}[h.InActiveList])
	}
	sort.Strings(got)
	// Ready to Assign, the card payment, the active set-aside and the "nodisc"
	// plan are out. The old plan's Christmas label is still here; Run drops it.
	want := "Christmas/hard,Dining/active,Discretionary/active,Groceries/hard"
	if strings.Join(got, ",") != want {
		t.Errorf("labels = %v, want %s", got, want)
	}
	if len(set.SetAsides) != 1 || set.SetAsides[0] != "Christmas Fund" {
		t.Errorf("set-asides = %v", set.SetAsides)
	}

	m := &fakeModel{answers: map[string]answer{}}
	c, err := Run(ctx, history, Options{Model: m, Policy: p, Candidates: set.Candidates})
	if err != nil {
		t.Fatal(err)
	}
	if c.N != 3 || c.Report.HardNegatives != 1 {
		t.Errorf("n = %d, hard negatives = %d; want 3 and 1 (Groceries)", c.N, c.Report.HardNegatives)
	}
}

func TestLabelPlansDedupes(t *testing.T) {
	got := labelPlans("a", []store.PlanScan{
		{PlanID: "b", HasDiscretionary: true}, {PlanID: "a", HasDiscretionary: true},
		{PlanID: "c"}, {PlanID: "b", HasDiscretionary: true}, {PlanID: "d", HasDiscretionary: true},
	})
	if strings.Join(got, ",") != "a,b,d" {
		t.Errorf("plans = %v", got)
	}
}
