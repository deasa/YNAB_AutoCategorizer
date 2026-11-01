package store

import (
	"context"
	"testing"
	"time"
)

func TestStatusState(t *testing.T) {
	db, path := openTest(t)
	ctx := context.Background()
	if s, err := db.GetStatusState(ctx); err != nil || s != (StatusState{}) {
		t.Fatalf("empty: %+v, %v", s, err)
	}
	// Either half can be written first, and neither overwrites the other.
	at := time.Date(2026, 11, 1, 10, 17, 0, 0, time.UTC)
	if err := db.SetStatusBackup(ctx, at, false); err != nil {
		t.Fatal(err)
	}
	if err := db.SetStatusMessage(ctx, "C1", "1.1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetStatusMessage(ctx, "C2", "2.2"); err != nil {
		t.Fatal(err)
	}
	s, err := db.GetStatusState(ctx)
	if err != nil || s.ChannelID != "C2" || s.TS != "2.2" || !s.BackupAt.Equal(at) || s.BackupOK {
		t.Fatalf("got %+v, %v", s, err)
	}
	if err := db.SetStatusBackup(ctx, at.Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	// It survives a reopen.
	db.Close()
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err = db.GetStatusState(ctx)
	if err != nil || s.TS != "2.2" || !s.BackupOK || !s.BackupAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("after reopen: %+v, %v", s, err)
	}
}

func TestAccountImports(t *testing.T) {
	db, _ := openTest(t)
	ctx := context.Background()
	ref := func(s string) *string { return &s }
	err := db.Update(ctx, func(tx *Tx) error {
		if err := tx.UpsertAccounts("p", []Account{
			{ID: "a1", Name: "Zed Card", DirectImportLinked: true, DirectImportInError: true},
			{ID: "a2", Name: "Checking", DirectImportLinked: true},
			{ID: "a3", Name: "New Link", DirectImportLinked: true},
			{ID: "a4", Name: "Manual"},
			{ID: "a5", Name: "Closed", DirectImportLinked: true, Closed: true},
		}); err != nil {
			return err
		}
		if err := tx.UpsertAccounts("other", []Account{{ID: "a1", Name: "Other Plan", DirectImportLinked: true}}); err != nil {
			return err
		}
		return tx.UpsertTransactions("p", []Transaction{
			{ID: "t1", Date: "2026-10-30", AccountID: "a2", ImportID: ref("i1")},
			{ID: "t2", Date: "2026-10-31", AccountID: "a2", ImportID: ref("i2")},
			{ID: "t3", Date: "2026-11-01", AccountID: "a2"},                                     // not imported
			{ID: "t4", Date: "2026-11-02", AccountID: "a2", ImportID: ref("i4"), Deleted: true}, // deleted
			{ID: "t5", Date: "2026-10-01", AccountID: "a1", ImportID: ref("i5")},
		}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.AccountImports(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	want := []AccountImport{
		{Name: "Checking", NewestImport: "2026-10-31"},
		{Name: "New Link"},
		{Name: "Zed Card", NewestImport: "2026-10-01", InError: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
