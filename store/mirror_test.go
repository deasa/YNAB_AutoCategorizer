package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fullPlan writes one of every mirror row type.
func fullPlan(tx *Tx, txnMemo string, deleted bool) error {
	steps := []func() error{
		// Children before parents: deferred FKs are checked at commit.
		func() error {
			return tx.UpsertSubtransactions(plan, []Subtransaction{
				{ID: "s1", TransactionID: "t1", Amount: -4000, CategoryID: ptr("c1")},
				{ID: "s2", TransactionID: "t1", Amount: -6000, CategoryID: ptr("c1")},
			})
		},
		func() error {
			return tx.UpsertTransactions(plan, []Transaction{{
				ID: "t1", Date: "2026-10-01", Amount: -10000, Memo: ptr(txnMemo), Cleared: "cleared",
				AccountID: "a1", PayeeID: ptr("p1"), CategoryID: ptr("split-sentinel"),
				ImportID: ptr("YNAB:-10000:2026-10-01:1"), MatchedTransactionID: ptr("t0"),
				FlagColor: ptr("red"), Deleted: deleted,
			}})
		},
		func() error {
			return tx.UpsertAccounts(plan, []Account{{
				ID: "a1", Name: "Checking", Type: "checking", OnBudget: true, Balance: 123450,
				DirectImportLinked: true, DirectImportInError: true,
				LastReconciledAt: ptr(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)),
			}})
		},
		func() error {
			return tx.UpsertCategoryGroups(plan, []CategoryGroup{{ID: "g1", Name: "Everyday"}})
		},
		func() error {
			return tx.UpsertCategories(plan, []Category{{
				ID: "c1", CategoryGroupID: "g1", Name: "Groceries", Budgeted: 50000,
				GoalType: ptr("NEED"), GoalTarget: ptr(int64(60000)), GoalNeedsWholeAmount: ptr(false),
				GoalTargetMonth: ptr("2026-12-01"),
			}})
		},
		func() error { return tx.UpsertPayees(plan, []Payee{{ID: "p1", Name: "Costco"}}) },
		func() error {
			return tx.UpsertMonthCategories(plan, []MonthCategory{{Month: "2026-10-01", CategoryID: "c1", Budgeted: 50000}})
		},
		func() error {
			return tx.UpsertMonths(plan, []Month{{Month: "2026-10-01", ToBeBudgeted: 1000, AgeOfMoney: ptr(int64(30))}})
		},
		func() error {
			return tx.UpsertScheduledSubtransactions(plan, []ScheduledSubtransaction{{ID: "ss1", ScheduledTransactionID: "st1", Amount: -500}})
		},
		func() error {
			return tx.UpsertScheduledTransactions(plan, []ScheduledTransaction{{
				ID: "st1", DateFirst: "2026-01-01", DateNext: "2026-11-01", Frequency: "monthly",
				Amount: -500, AccountID: "a1",
			}})
		},
		func() error {
			return tx.SetSyncState(SyncState{PlanID: plan, ServerKnowledge: 42, SyncedAt: time.Now()})
		},
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return err
		}
	}
	return nil
}

func TestUpsertThenReupsertUpdatesInPlace(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if err := db.Update(ctx, func(tx *Tx) error { return fullPlan(tx, "first", false) }); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first := count(t, db, `SELECT count(*) FROM transactions`)
	var seen1 string
	db.r.QueryRow(`SELECT first_seen_at FROM transactions WHERE id = 't1'`).Scan(&seen1)

	if err := db.Update(ctx, func(tx *Tx) error { return fullPlan(tx, "second", false) }); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	for _, table := range []string{"accounts", "category_groups", "categories", "payees", "transactions",
		"subtransactions", "months", "month_categories", "scheduled_transactions",
		"scheduled_subtransactions", "sync_state"} {
		want := 1
		if table == "subtransactions" {
			want = 2
		}
		if n := count(t, db, `SELECT count(*) FROM `+table); n != want {
			t.Errorf("%s rows = %d, want %d", table, n, want)
		}
	}
	if first != 1 {
		t.Errorf("transactions after first upsert = %d", first)
	}
	var memo, seen2 string
	var flag *string
	var imp string
	if err := db.r.QueryRow(`SELECT memo, first_seen_at, flag_color, import_id FROM transactions WHERE id = 't1'`).
		Scan(&memo, &seen2, &flag, &imp); err != nil {
		t.Fatal(err)
	}
	if memo != "second" {
		t.Errorf("memo = %q, want second", memo)
	}
	if seen2 != seen1 {
		t.Errorf("first_seen_at changed on update: %q -> %q", seen1, seen2)
	}
	if flag == nil || *flag != "red" || imp == "" {
		t.Errorf("flag/import_id not stored: %v %q", flag, imp)
	}
	var recon string
	db.r.QueryRow(`SELECT last_reconciled_at FROM accounts WHERE id = 'a1'`).Scan(&recon)
	if recon != "2026-09-01T12:00:00Z" {
		t.Errorf("last_reconciled_at = %q", recon)
	}
}

func TestTaxBucketSurvivesSync(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if err := db.Update(ctx, func(tx *Tx) error {
		if err := fullPlan(tx, "m", false); err != nil {
			return err
		}
		return tx.SetAccountTaxBucket(plan, "a1", ptr("roth"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(ctx, func(tx *Tx) error { return fullPlan(tx, "m", false) }); err != nil {
		t.Fatal(err)
	}
	var bucket *string
	db.r.QueryRow(`SELECT tax_bucket FROM accounts WHERE id = 'a1'`).Scan(&bucket)
	if bucket == nil || *bucket != "roth" {
		t.Errorf("tax_bucket = %v, want roth", bucket)
	}
	if err := db.Update(ctx, func(tx *Tx) error { return tx.SetAccountTaxBucket(plan, "nope", nil) }); err == nil {
		t.Error("SetAccountTaxBucket on unknown account succeeded")
	}
}

func TestTombstoneMarksDeleted(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if err := db.Update(ctx, func(tx *Tx) error { return fullPlan(tx, "m", false) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		if err := tx.UpsertTransactions(plan, []Transaction{{ID: "t1", Date: "2026-10-01", Amount: -10000,
			Cleared: "cleared", AccountID: "a1", Deleted: true}}); err != nil {
			return err
		}
		return tx.UpsertPayees(plan, []Payee{{ID: "p1", Name: "Costco", Deleted: true}})
	}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM transactions WHERE id = 't1' AND deleted = 1`); n != 1 {
		t.Errorf("tombstoned transaction rows = %d, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM payees WHERE deleted = 1`); n != 1 {
		t.Errorf("tombstoned payee rows = %d, want 1", n)
	}
	// Children are kept (their own tombstones arrive separately).
	if n := count(t, db, `SELECT count(*) FROM subtransactions`); n != 2 {
		t.Errorf("subtransactions = %d, want 2", n)
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	cases := map[string]func(*Tx) error{
		"subtransaction without parent": func(tx *Tx) error {
			return tx.UpsertSubtransactions(plan, []Subtransaction{{ID: "s9", TransactionID: "missing"}})
		},
		"category without group": func(tx *Tx) error {
			return tx.UpsertCategories(plan, []Category{{ID: "c9", CategoryGroupID: "missing", Name: "x"}})
		},
		"month category without month": func(tx *Tx) error {
			return tx.UpsertMonthCategories(plan, []MonthCategory{{Month: "1999-01-01", CategoryID: "c1"}})
		},
		"scheduled sub without parent": func(tx *Tx) error {
			return tx.UpsertScheduledSubtransactions(plan, []ScheduledSubtransaction{{ID: "x", ScheduledTransactionID: "missing"}})
		},
		"parent in another plan": func(tx *Tx) error {
			if err := tx.UpsertTransactions("other-plan", []Transaction{{ID: "t1", Date: "2026-01-01", Cleared: "cleared", AccountID: "a"}}); err != nil {
				return err
			}
			return tx.UpsertSubtransactions(plan, []Subtransaction{{ID: "s9", TransactionID: "t1"}})
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			err := db.Update(ctx, fn)
			if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
				t.Fatalf("Update = %v, want FOREIGN KEY failure", err)
			}
		})
	}
	// A failed batch rolls back entirely.
	if n := count(t, db, `SELECT count(*) FROM transactions`); n != 0 {
		t.Errorf("transactions after failed batches = %d, want 0", n)
	}
	// The writer connection is usable after failed COMMITs, and a split
	// parent's sentinel category_id needs no matching category.
	if err := db.Update(ctx, func(tx *Tx) error { return fullPlan(tx, "m", false) }); err != nil {
		t.Fatalf("split sentinel rejected: %v", err)
	}
}

func TestSyncState(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	if _, ok, err := db.GetSyncState(ctx, plan); err != nil || ok {
		t.Fatalf("GetSyncState on empty = ok %v, err %v", ok, err)
	}
	at := time.Date(2026, 10, 29, 8, 15, 0, 123, time.FixedZone("MDT", -6*3600))
	for _, sk := range []int64{100, 250} {
		if err := db.SetSyncState(ctx, SyncState{PlanID: plan, ServerKnowledge: sk, SyncedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := db.GetSyncState(ctx, plan)
	if err != nil || !ok {
		t.Fatalf("GetSyncState = ok %v, err %v", ok, err)
	}
	if got.ServerKnowledge != 250 || !got.SyncedAt.Equal(at) || got.PlanID != plan {
		t.Errorf("GetSyncState = %+v", got)
	}
}
