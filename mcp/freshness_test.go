package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	finauth "github.com/deasa/YNAB_AutoCategorizer/auth"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

func ptr(s string) *string { return &s }

// seedLinks gives the active plan one account in each state, and a fresh sync.
// now is 2026-11-05.
func seedLinks(t *testing.T, o Options) {
	t.Helper()
	ctx := context.Background()
	imported := func(id, account, date string, deleted bool) store.Transaction {
		return store.Transaction{ID: id, AccountID: account, Date: date, ImportID: ptr("YNAB:" + id), Deleted: deleted}
	}
	seen := now.Add(-48 * time.Hour)
	err := o.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertAccounts(planID, []store.Account{
			{ID: "acct-healthy", Name: "Healthy Checking", DirectImportLinked: true},
			{ID: "acct-edge", Name: "Edge Savings", DirectImportLinked: true},
			{ID: "acct-old", Name: "Quiet Card", DirectImportLinked: true},
			{ID: "acct-error", Name: "Broken Card", DirectImportLinked: true, DirectImportInError: true},
			{ID: "acct-never", Name: "Empty Card", DirectImportLinked: true},
			{ID: "acct-deleted-only", Name: "Tombstone Card", DirectImportLinked: true},
			{ID: "acct-manual", Name: "Cash", DirectImportLinked: false},
			{ID: "acct-closed", Name: "Closed Card", DirectImportLinked: true, Closed: true, DirectImportInError: true},
			{ID: "acct-gone", Name: "Deleted Card", DirectImportLinked: true, Deleted: true, DirectImportInError: true},
		}); err != nil {
			return err
		}
		if err := tx.UpsertAccounts("other-plan", []store.Account{
			{ID: "acct-other", Name: "Other Plan Card", DirectImportLinked: true, DirectImportInError: true},
		}); err != nil {
			return err
		}
		// A manual entry (no import_id) must not count as an import.
		manual := store.Transaction{ID: "t-manual", AccountID: "acct-never", Date: "2026-11-04"}
		if err := tx.UpsertTransactions(planID, []store.Transaction{
			imported("t1", "acct-healthy", "2026-11-03", false),
			imported("t1-old", "acct-healthy", "2026-10-01", false),
			imported("t2", "acct-edge", "2026-10-22", false), // exactly 14 days
			imported("t3", "acct-old", "2026-10-21", false),  // 15 days
			imported("t4", "acct-error", "2026-11-04", false),
			imported("t5", "acct-deleted-only", "2026-11-04", true),
			manual,
		}, seen); err != nil {
			return err
		}
		return tx.SetSyncState(store.SyncState{PlanID: planID, ServerKnowledge: 1, SyncedAt: now.Add(-time.Minute)})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The point of the finding: a fresh sync must not hide broken or stale links.
func TestFreshness_BrokenLinksShowWhenSyncIsFresh(t *testing.T) {
	o, _ := testOptions(t)
	seedLinks(t, o)
	f, err := readFreshness(context.Background(), &o)
	if err != nil {
		t.Fatal(err)
	}
	if f.Stale {
		t.Error("the sync is fresh")
	}
	if f.LinksOK {
		t.Error("links_ok is true with broken links")
	}
	want := map[string]struct {
		status, date string
		age          int // -1: absent
		inError      bool
	}{
		"Healthy Checking": {importOK, "2026-11-03", 2, false},
		"Edge Savings":     {importOK, "2026-10-22", 14, false},
		"Quiet Card":       {importStale, "2026-10-21", 15, false},
		"Broken Card":      {importLinkErr, "2026-11-04", 1, true},
		"Empty Card":       {importNeverSet, "", -1, false},
		"Tombstone Card":   {importNeverSet, "", -1, false},
	}
	if len(f.Accounts) != len(want) {
		t.Errorf("accounts = %+v", f.Accounts)
	}
	for _, a := range f.Accounts {
		w, ok := want[a.Name]
		if !ok {
			t.Errorf("unexpected account %q (unlinked, closed, deleted or another plan's)", a.Name)
			continue
		}
		age := -1
		if a.ImportAgeDays != nil {
			age = *a.ImportAgeDays
		}
		if a.Status != w.status || a.LastImportDate != w.date || age != w.age || a.InError != w.inError {
			t.Errorf("%s = %+v (age %d), want %+v", a.Name, a, age, w)
		}
		if w.date != "" && a.LastImportSeenAt != now.Add(-48*time.Hour).Format(time.RFC3339) {
			t.Errorf("%s: last_import_seen_at = %q", a.Name, a.LastImportSeenAt)
		}
		if w.date == "" && a.LastImportSeenAt != "" {
			t.Errorf("%s: last_import_seen_at = %q with no imports", a.Name, a.LastImportSeenAt)
		}
	}
	warnings := strings.Join(f.Warnings, "\n")
	for _, name := range []string{"Quiet Card", "Broken Card", "Empty Card", "Tombstone Card"} {
		if !strings.Contains(warnings, name) {
			t.Errorf("no warning names %s: %s", name, warnings)
		}
	}
	for _, name := range []string{"Healthy Checking", "Edge Savings"} {
		if strings.Contains(warnings, name) {
			t.Errorf("a healthy account has a warning: %s", warnings)
		}
	}
}

func TestFreshness_AllHealthy(t *testing.T) {
	ctx := context.Background()
	o, _ := testOptions(t)
	err := o.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertAccounts(planID, []store.Account{{ID: "a", Name: "Checking", DirectImportLinked: true}}); err != nil {
			return err
		}
		if err := tx.UpsertTransactions(planID, []store.Transaction{
			{ID: "t", AccountID: "a", Date: "2026-11-05", ImportID: ptr("YNAB:t")}}, now); err != nil {
			return err
		}
		return tx.SetSyncState(store.SyncState{PlanID: planID, ServerKnowledge: 1, SyncedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := readFreshness(ctx, &o)
	if err != nil || !f.LinksOK || f.Stale || len(f.Warnings) != 0 || len(f.Accounts) != 1 {
		t.Errorf("%+v, %v", f, err)
	}
}

// A sync gone stale warns too, separately from the links.
func TestFreshness_StaleSyncWarns(t *testing.T) {
	ctx := context.Background()
	o, _ := testOptions(t)
	if err := o.DB.SetSyncState(ctx, store.SyncState{PlanID: planID, ServerKnowledge: 1, SyncedAt: now.Add(-3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f, err := readFreshness(ctx, &o)
	if err != nil || !f.Stale || !f.LinksOK || len(f.Warnings) != 1 || !strings.Contains(f.Warnings[0], "3h0m0s") {
		t.Errorf("%+v, %v", f, err)
	}
}

// Every tool gets it through the shared wrapper, not just data_freshness, and
// the wire form names accounts without their ids.
func TestEveryToolResponseCarriesAccountFreshness(t *testing.T) {
	o, _ := testOptions(t)
	seedLinks(t, o)
	defs := []toolDef{define(finauth.ScopeMCP, &sdkmcp.Tool{Name: "other_tool"},
		func(context.Context, *Options, *sdkmcp.CallToolRequest, emptyArgs) (struct{}, error) {
			return struct{}{}, nil
		})}
	srv := httptest.NewServer(newHandler(o, defs))
	defer srv.Close()
	res, err := connect(t, srv.URL, "good").CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "other_tool"})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	got := decode[struct{}](t, res).DataFreshness
	if got.LinksOK || len(got.Accounts) != 6 || len(got.Warnings) != 4 {
		t.Errorf("freshness = %+v", got)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if strings.Contains(string(raw), "acct-") || strings.Contains(string(raw), planID) {
		t.Errorf("response carries an id: %s", raw)
	}
}

func TestDaysSince_UsesCalendarDaysInNowsZone(t *testing.T) {
	denver := time.FixedZone("MST", -7*3600)
	for _, c := range []struct {
		date string
		now  time.Time
		want int
	}{
		{"2026-11-05", time.Date(2026, 11, 5, 23, 59, 0, 0, denver), 0},
		{"2026-11-04", time.Date(2026, 11, 5, 0, 1, 0, 0, denver), 1},
		{"2026-11-06", time.Date(2026, 11, 5, 12, 0, 0, 0, denver), 0}, // future-dated
		{"2026-03-07", time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC), 2},
	} {
		if got, err := daysSince(c.date, c.now); err != nil || got != c.want {
			t.Errorf("daysSince(%s, %v) = %d, %v; want %d", c.date, c.now, got, err, c.want)
		}
	}
	if _, err := daysSince("nope", now); err == nil {
		t.Error("a bad date passed")
	}
}
