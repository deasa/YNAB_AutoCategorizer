package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const (
	planOld = "plan-old"
	planNew = "plan-new"
)

var noNames []string

// seedMirror writes a plan's reference rows, then txns and subs.
func seedMirror(t *testing.T, db *DB, planID string, txns []Transaction, subs []Subtransaction) {
	t.Helper()
	err := db.Update(context.Background(), func(tx *Tx) error {
		if err := tx.UpsertAccounts(planID, []Account{{ID: "a1", Name: "Checking", Type: "checking", OnBudget: true}}); err != nil {
			return err
		}
		if err := tx.UpsertCategoryGroups(planID, []CategoryGroup{
			{ID: "g1", Name: "Everyday"},
			{ID: "gcc", Name: "Credit Card Payments"},
			{ID: "gint", Name: "Internal Master Category", Internal: true},
			{ID: "ghid", Name: "Hidden Categories", Hidden: true, Internal: true},
		}); err != nil {
			return err
		}
		if err := tx.UpsertCategories(planID, []Category{
			{ID: "groc", CategoryGroupID: "g1", Name: "Groceries"},
			{ID: "dine", CategoryGroupID: "g1", Name: "Dining", Note: ptr("eating out"), GoalType: ptr("NEED")},
			{ID: "bday", CategoryGroupID: "g1", Name: "Birthday"},
			{ID: "old", CategoryGroupID: "ghid", OriginalCategoryGroupID: ptr("g1"), Name: "Old Hobby", Hidden: true},
			{ID: "gone", CategoryGroupID: "g1", Name: "Gone", Deleted: true},
			{ID: "orphan", CategoryGroupID: "g-missing", Name: "Orphan"},
			{ID: "rta", CategoryGroupID: "gint", Name: "Inflow: Ready to Assign", Internal: true},
			{ID: "unc", CategoryGroupID: "gint", Name: "Uncategorized", Internal: true},
			{ID: "visa", CategoryGroupID: "gcc", Name: "Visa"},
			{ID: "visa-old", CategoryGroupID: "ghid", OriginalCategoryGroupID: ptr("gcc"), Name: "Closed Visa", Hidden: true},
		}); err != nil {
			return err
		}
		if err := tx.UpsertPayees(planID, []Payee{
			{ID: "costco", Name: "Costco, Inc."},
			{ID: "costco2", Name: "COSTCO  INC"},
			{ID: "shell", Name: "Shell"},
		}); err != nil {
			return err
		}
		if err := tx.UpsertSubtransactions(planID, subs); err != nil {
			return err
		}
		return tx.UpsertTransactions(planID, txns, testSyncedAt)
	})
	if err != nil {
		t.Fatalf("seed %s: %v", planID, err)
	}
}

// labeled is a Transaction a human approved, on 2026-10-15 at Shell.
func labeled(id, category string) Transaction {
	return Transaction{
		ID: id, Date: "2026-10-15", Amount: -1000, Cleared: "cleared", Approved: true, AccountID: "a1",
		PayeeID: ptr("shell"), CategoryID: ptr(category),
	}
}

func history(t *testing.T, db *DB, plans, active, excluded []string) []LabeledRow {
	t.Helper()
	got, err := db.LabeledHistory(context.Background(), plans, active, excluded)
	if err != nil {
		t.Fatalf("LabeledHistory: %v", err)
	}
	return got
}

func memos(rows []LabeledRow) string {
	var s []string
	for _, r := range rows {
		s = append(s, r.Memo)
	}
	return strings.Join(s, ",")
}

func TestLabeledHistoryExcludesUnlabelledRows(t *testing.T) {
	db, _ := openTest(t)
	keep := labeled("keep", "groc")
	keep.Memo = ptr("keep")
	unapproved := labeled("unapproved", "groc")
	unapproved.Approved = false
	deleted := labeled("deleted", "groc")
	deleted.Deleted = true
	split := labeled("split", "groc") // a live child makes it a split, whatever its category
	sentinel := labeled("sentinel", "split-sentinel")
	transfer := labeled("transfer", "groc")
	transfer.TransferAccountID = ptr("a2")
	none := labeled("none", "")
	none.CategoryID = nil
	seedMirror(t, db, plan, []Transaction{keep, unapproved, deleted, split, sentinel, transfer, none},
		[]Subtransaction{{ID: "s1", TransactionID: "split", Amount: -1000, CategoryID: ptr("groc")}})

	got := history(t, db, []string{plan}, []string{"Groceries"}, noNames)
	if len(got) != 1 {
		t.Fatalf("rows = %+v, want only keep", got)
	}
	want := LabeledRow{Date: "2026-10-15", Amount: -1000, Payee: "shell", Memo: "keep", Account: "Checking",
		Category: "Groceries", InActiveList: true, PlanID: plan}
	if got[0] != want {
		t.Errorf("row = %+v, want %+v", got[0], want)
	}
}

func TestLabeledHistoryKeepsRowWhoseSplitWasUndone(t *testing.T) {
	db, _ := openTest(t)
	undone := labeled("undone", "groc")
	undone.Memo = ptr("undone")
	seedMirror(t, db, plan, []Transaction{undone},
		[]Subtransaction{{ID: "s1", TransactionID: "undone", Amount: -1000, CategoryID: ptr("groc"), Deleted: true}})
	if got := history(t, db, []string{plan}, noNames, noNames); memos(got) != "undone" {
		t.Errorf("rows = %+v", got)
	}
}

func TestLabeledHistoryAbsentCategoryIsKeptAndFlagged(t *testing.T) {
	db, _ := openTest(t)
	a, b, c := labeled("a", "groc"), labeled("b", "dine"), labeled("c", "gone")
	a.Memo, b.Memo, c.Memo = ptr("a"), ptr("b"), ptr("c")
	a.Date, b.Date, c.Date = "2026-10-03", "2026-10-02", "2026-10-01"
	seedMirror(t, db, plan, []Transaction{a, b, c}, nil)

	got := history(t, db, []string{plan}, []string{"gROCERIES"}, noNames)
	if memos(got) != "a,b,c" {
		t.Fatalf("rows = %+v", got)
	}
	if !got[0].InActiveList {
		t.Error("Groceries should match the active list case-insensitively")
	}
	if got[1].InActiveList || got[1].Category != "Dining" {
		t.Errorf("Dining = %+v, want in_active_list false", got[1])
	}
	if got[2].InActiveList || got[2].Category != "Gone" {
		t.Errorf("deleted category = %+v, want kept with in_active_list false", got[2])
	}
}

func TestLabeledHistoryDropsNonLabelCategories(t *testing.T) {
	db, _ := openTest(t)
	keep := labeled("keep", "old") // hidden, but an ordinary category
	keep.Memo = ptr("keep")
	seedMirror(t, db, plan, []Transaction{
		keep, labeled("rta", "rta"), labeled("unc", "unc"), labeled("visa", "visa"),
		labeled("visa-old", "visa-old"), labeled("bday", "bday"),
	}, nil)

	got := history(t, db, []string{plan}, []string{"Inflow: Ready to Assign", "Visa", "Birthday"}, []string{"BIRTHDAY"})
	if memos(got) != "keep" {
		t.Errorf("rows = %+v, want only the hidden ordinary category", got)
	}
}

func TestLabeledHistoryDropsRenamedReadyToAssignByName(t *testing.T) {
	db, _ := openTest(t)
	// Not flagged internal, so only the name can exclude it.
	err := db.Update(context.Background(), func(tx *Tx) error {
		return tx.UpsertCategories(plan, []Category{{ID: "rta2", CategoryGroupID: "g1", Name: "inflow: ready to assign"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	seedMirror(t, db, plan, []Transaction{labeled("r", "rta2")}, nil)
	if got := history(t, db, []string{plan}, noNames, noNames); len(got) != 0 {
		t.Errorf("rows = %+v, want none", got)
	}
}

func TestLabeledHistoryDedupesAcrossPlans(t *testing.T) {
	copied := func(memo string, importID *string, payee string) Transaction {
		r := labeled("t-"+memo, "groc")
		r.Memo, r.ImportID, r.PayeeID = ptr(memo), importID, ptr(payee)
		return r
	}
	// marker makes planNew the newest plan by latest transaction date.
	marker := copied("marker", ptr("m"), "shell")
	marker.Date = "2026-11-01"

	for _, tc := range []struct {
		name     string
		old, new Transaction
		want     string
	}{
		{"same import_id", copied("o", ptr("i1"), "shell"), copied("n", ptr("i1"), "costco"), "marker,n"},
		{"null import_id, same normalized payee", copied("o", nil, "costco"), copied("n", nil, "costco2"), "marker,n"},
		{"import_id only on the older copy", copied("o", ptr("i1"), "costco"), copied("n", nil, "costco2"), "marker,n"},
		{"import_id only on the newer copy", copied("o", nil, "costco"), copied("n", ptr("i1"), "costco2"), "marker,n"},
		{"different import_ids are different rows", copied("o", ptr("i1"), "costco"), copied("n", ptr("i2"), "costco"), "marker,n,o"},
		{"null import_id, different payee", copied("o", nil, "shell"), copied("n", nil, "costco"), "marker,n,o"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openTest(t)
			seedMirror(t, db, planOld, []Transaction{tc.old}, nil)
			seedMirror(t, db, planNew, []Transaction{tc.new, marker}, nil)
			// Listed oldest first: recency comes from the data, not the argument order.
			got := history(t, db, []string{planOld, planNew}, noNames, noNames)
			if memos(got) != tc.want {
				t.Fatalf("rows = %q, want %q", memos(got), tc.want)
			}
			for _, r := range got {
				if wantPlan := map[string]string{"o": planOld}[r.Memo]; wantPlan != "" && r.PlanID != wantPlan {
					t.Errorf("%s from %s", r.Memo, r.PlanID)
				} else if wantPlan == "" && r.PlanID != planNew {
					t.Errorf("%s from %s, want the newest plan", r.Memo, r.PlanID)
				}
			}
		})
	}
}

func TestLabeledHistoryKeepsLookalikesWithinOnePlan(t *testing.T) {
	db, _ := openTest(t)
	a, b := labeled("a", "groc"), labeled("b", "groc") // two identical coffees, no import_id
	a.Memo, b.Memo = ptr("a"), ptr("b")
	seedMirror(t, db, plan, []Transaction{a, b}, nil)
	if got := history(t, db, []string{plan}, noNames, noNames); memos(got) != "a,b" {
		t.Errorf("rows = %q, want both", memos(got))
	}
}

func TestLabeledHistoryFallsBackToOlderLabel(t *testing.T) {
	db, _ := openTest(t)
	old := labeled("t", "groc")
	old.Memo, old.ImportID = ptr("old"), ptr("i1")
	unlabelled := labeled("t", "groc")
	unlabelled.ImportID, unlabelled.Approved = ptr("i1"), false
	newer := labeled("x", "groc")
	newer.Date, newer.Memo = "2026-11-01", ptr("x")
	seedMirror(t, db, planOld, []Transaction{old}, nil)
	seedMirror(t, db, planNew, []Transaction{unlabelled, newer}, nil)
	if got := history(t, db, []string{planNew, planOld}, noNames, noNames); memos(got) != "x,old" {
		t.Errorf("rows = %q, want the older label kept", memos(got))
	}
}

func TestLabeledHistoryNoPlans(t *testing.T) {
	db, _ := openTest(t)
	if got := history(t, db, nil, noNames, noNames); len(got) != 0 {
		t.Errorf("rows = %+v", got)
	}
}

func TestNormalizePayee(t *testing.T) {
	for in, want := range map[string]string{
		"":                  "",
		"  Costco, Inc. ":   "costco inc",
		"COSTCO  INC":       "costco inc",
		"Trader Joe's #552": "trader joes 552",
		"Trader Joe’s":      "trader joes",
		"Café  Río":         "café río",
		"***":               "",
	} {
		if got := NormalizePayee(in); got != want {
			t.Errorf("NormalizePayee(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCategoriesWithGroups(t *testing.T) {
	db, _ := openTest(t)
	seedMirror(t, db, plan, nil, nil)
	if err := db.Update(context.Background(), func(tx *Tx) error {
		return tx.UpsertCategoryGroups("plan-other", []CategoryGroup{{ID: "z", Name: "Other Plan"}})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(context.Background(), func(tx *Tx) error {
		return tx.UpsertCategories("plan-other", []Category{{ID: "z1", CategoryGroupID: "z", Name: "Other"}})
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.CategoriesWithGroups(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]CategoryWithGroup{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if len(got) != 10 || len(byName) != 10 {
		t.Fatalf("categories = %d, want 10 from this plan only", len(got))
	}
	if c := byName["Groceries"]; c.ID != "groc" || c.GroupName != "Everyday" || c.Note != nil || c.Hidden || c.Deleted || c.GroupHidden || c.GroupDeleted {
		t.Errorf("no-target category = %+v", c)
	}
	if c := byName["Dining"]; c.Note == nil || *c.Note != "eating out" {
		t.Errorf("note = %+v", c)
	}
	if c := byName["Old Hobby"]; !c.Hidden || !c.GroupHidden || c.GroupName != "Hidden Categories" {
		t.Errorf("hidden = %+v", c)
	}
	if c := byName["Gone"]; !c.Deleted {
		t.Errorf("deleted = %+v", c)
	}
	// Full exports omit deleted groups, so an absent group reads as deleted.
	if c := byName["Orphan"]; c.GroupName != "" || !c.GroupDeleted {
		t.Errorf("group not in mirror = %+v", c)
	}
}

func TestCategoriesWithGroupsDeletedGroup(t *testing.T) {
	db, _ := openTest(t)
	seedMirror(t, db, plan, nil, nil)
	if err := db.Update(context.Background(), func(tx *Tx) error {
		return tx.UpsertCategoryGroups(plan, []CategoryGroup{{ID: "g1", Name: "Everyday", Deleted: true}})
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.CategoriesWithGroups(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.Name == "Groceries" && !c.GroupDeleted {
			t.Errorf("Groceries = %+v, want group deleted", c)
		}
	}
}

func TestTransactionByID(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	full := labeled("full", "groc")
	full.Memo, full.ImportID = ptr("m"), ptr("i1")
	split := labeled("split", "split-sentinel")
	gone := labeled("gone", "groc")
	gone.Deleted = true
	transfer := labeled("transfer", "groc")
	transfer.TransferAccountID = ptr("a2")
	seedMirror(t, db, plan, []Transaction{full, split, gone, transfer},
		[]Subtransaction{{ID: "s1", TransactionID: "split", Amount: -1000, CategoryID: ptr("groc")}})

	got, ok, err := db.TransactionByID(ctx, plan, "full")
	if err != nil || !ok {
		t.Fatalf("found: %v %v", ok, err)
	}
	if got.ID != "full" || got.Date != "2026-10-15" || got.Amount != -1000 || !got.Approved || *got.Memo != "m" ||
		*got.ImportID != "i1" || *got.CategoryID != "groc" || *got.CategoryName != "Groceries" ||
		*got.AccountName != "Checking" || *got.PayeeName != "Shell" || got.IsSplit || got.Deleted || !got.FirstSeenAt.Equal(testSyncedAt) {
		t.Errorf("full row = %+v", got)
	}
	if got, ok, err := db.TransactionByID(ctx, plan, "split"); err != nil || !ok || !got.IsSplit || got.CategoryName != nil {
		t.Errorf("split = %+v, %v, %v", got, ok, err)
	}
	if got, ok, err := db.TransactionByID(ctx, plan, "gone"); err != nil || !ok || !got.Deleted {
		t.Errorf("deleted = %+v, %v, %v", got, ok, err)
	}
	if got, ok, err := db.TransactionByID(ctx, plan, "transfer"); err != nil || !ok || *got.TransferAccountID != "a2" {
		t.Errorf("transfer = %+v, %v, %v", got, ok, err)
	}
	if _, ok, err := db.TransactionByID(ctx, plan, "nope"); err != nil || ok {
		t.Errorf("missing = %v, %v; want not found", ok, err)
	}
	if _, ok, err := db.TransactionByID(ctx, "plan-other", "full"); err != nil || ok {
		t.Errorf("other plan = %v, %v; want not found", ok, err)
	}
}

func TestEligibleTransactions(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	at := func(id, date string, approved bool, category string) Transaction {
		r := labeled(id, category)
		r.Date, r.Approved = date, approved
		if category == "" {
			r.CategoryID = nil
		}
		return r
	}
	deleted := at("deleted", "2026-10-20", false, "")
	deleted.Deleted = true
	seedMirror(t, db, plan, []Transaction{
		at("tooOld", "2026-09-30", false, ""),
		at("onSince", "2026-10-01", false, "groc"), // categorized, unapproved
		at("noCat", "2026-10-10", true, ""),        // approved, no category
		at("uncat", "2026-10-11", true, "unc"),     // approved, YNAB's Uncategorized
		at("newest", "2026-10-12", false, ""),
		at("done", "2026-10-13", true, "groc"),
		deleted,
	}, nil)

	ids := func(limit int) string {
		t.Helper()
		got, err := db.EligibleTransactions(ctx, plan, "2026-10-01", limit)
		if err != nil {
			t.Fatal(err)
		}
		var s []string
		for _, r := range got {
			s = append(s, r.ID)
		}
		return strings.Join(s, ",")
	}
	if got := ids(0); got != "newest,uncat,noCat,onSince" {
		t.Errorf("no limit = %s", got)
	}
	if got := ids(2); got != "newest,uncat" {
		t.Errorf("limit 2 = %s", got)
	}

	got, err := db.EligibleTransactions(ctx, plan, "2026-10-12", 1)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d rows, %v", len(got), err)
	}
	if r := got[0]; *r.PayeeName != "Shell" || *r.AccountName != "Checking" || r.Amount != -1000 || r.Date != "2026-10-12" {
		t.Errorf("row = %s", fmt.Sprintf("%+v", r))
	}
}
