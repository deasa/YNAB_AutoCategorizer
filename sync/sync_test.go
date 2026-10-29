package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
	ynabapi "pkg.venceslau.dev/ynab"
)

const (
	planID = "11111111-1111-1111-1111-111111111111"
	acctA  = "aaaaaaaa-0000-0000-0000-000000000001"
	acctB  = "aaaaaaaa-0000-0000-0000-000000000002"
)

// fakeAPI serves one export and a chain of deltas keyed by the cursor they
// start from.
type fakeAPI struct {
	export   *ynab.PlanDetail
	exportSK ynab.ServerKnowledge
	deltas   map[ynab.ServerKnowledge]fakeDelta
	err      error
	imported []string
	groups   []*ynab.CategoryGroup
	calls    []string
}

type fakeDelta struct {
	detail *ynab.PlanDetail
	next   ynab.ServerKnowledge
}

func (f *fakeAPI) Export(context.Context) (*ynab.PlanDetail, ynab.ServerKnowledge, error) {
	f.calls = append(f.calls, "export")
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.export, f.exportSK, nil
}

func (f *fakeAPI) Delta(_ context.Context, st *ynab.SyncState) (*ynab.PlanDetail, error) {
	f.calls = append(f.calls, fmt.Sprintf("delta@%d", st.Plan))
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.deltas[st.Plan]
	if !ok {
		return &ynab.PlanDetail{}, nil // nothing changed
	}
	st.Plan = d.next
	return d.detail, nil
}

func (f *fakeAPI) ImportTransactions(context.Context) ([]string, error) {
	f.calls = append(f.calls, "import")
	return f.imported, f.err
}

func (f *fakeAPI) GetCategories(context.Context) ([]*ynab.CategoryGroup, error) {
	f.calls = append(f.calls, "categories")
	return f.groups, f.err
}

func (f *fakeAPI) setDelta(from, next ynab.ServerKnowledge, d *ynab.PlanDetail) {
	if f.deltas == nil {
		f.deltas = map[ynab.ServerKnowledge]fakeDelta{}
	}
	f.deltas[from] = fakeDelta{detail: d, next: next}
}

func ptr[T any](v T) *T { return &v }

func account(id string, inError bool) ynabapi.AccountBase {
	return ynabapi.AccountBase{ID: id, Name: "Acct " + id[len(id)-1:], Type: "checking", OnBudget: true,
		DirectImportLinked: true, DirectImportInError: inError}
}

func txn(id, acct string, day int, amount int64, importID *string) ynabapi.TransactionBase {
	return ynabapi.TransactionBase{ID: id, Date: ynabapi.NewDate(2026, time.October, day),
		Amount: ynabapi.Milliunits(amount), Cleared: ynabapi.ClearedStatusCleared, AccountID: acct,
		ImportID: importID}
}

func sub(id, parent string, amount int64) ynabapi.SubtransactionBase {
	return ynabapi.SubtransactionBase{ID: id, TransactionID: parent, Amount: ynabapi.Milliunits(amount),
		CategoryID: ptr("cat-1")}
}

// baseExport is a small plan: two linked accounts (A's link in error), one
// category, one payee, an imported txn and a manual split txn in A, one month.
func baseExport() *ynab.PlanDetail {
	cat := ynabapi.CategoryBase{ID: "cat-1", CategoryGroupID: "grp-1", Name: "Discretionary"}
	month := ynabapi.MonthDetailBase{Categories: []ynabapi.CategoryBase{cat}}
	month.Month = ynabapi.NewMonth(2026, time.October)
	return &ynab.PlanDetail{
		Accounts:       []ynabapi.AccountBase{account(acctA, true), account(acctB, false)},
		CategoryGroups: []ynabapi.CategoryGroup{{ID: "grp-1", Name: "Everyday"}},
		Categories:     []ynabapi.CategoryBase{cat},
		Payees:         []ynabapi.Payee{{ID: "payee-1", Name: "Grocer"}},
		Transactions: []ynabapi.TransactionBase{
			txn("t-imp", acctA, 1, -5000, ptr("YNAB:-5000:2026-10-01:1")),
			txn("t-split", acctA, 20, -9000, nil),
		},
		Subtransactions: []ynabapi.SubtransactionBase{sub("s-1", "t-split", -4000), sub("s-2", "t-split", -5000)},
		Months:          []ynabapi.MonthDetailBase{month},
	}
}

type harness struct {
	t    *testing.T
	db   *store.DB
	api  *fakeAPI
	s    *Syncer
	logs *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{t: t, db: db, api: &fakeAPI{export: baseExport(), exportSK: 100}, logs: &bytes.Buffer{}}
	h.s = New(db, func(string) PlanAPI { return h.api }, log.New(h.logs, "", 0))
	return h
}

func (h *harness) tick() Result {
	h.t.Helper()
	res, err := h.s.SyncPlan(context.Background(), planID)
	if err != nil {
		h.t.Fatalf("SyncPlan: %v", err)
	}
	return res
}

func (h *harness) count(table, where string, args ...any) int {
	h.t.Helper()
	var n int
	q := "SELECT COUNT(*) FROM " + table + " WHERE plan_id = ?"
	if where != "" {
		q += " AND " + where
	}
	if err := h.db.Reader().QueryRow(q, append([]any{planID}, args...)...).Scan(&n); err != nil {
		h.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (h *harness) cursor() int64 {
	h.t.Helper()
	st, ok, err := h.db.GetSyncState(context.Background(), planID)
	if err != nil || !ok {
		h.t.Fatalf("sync state: ok=%v err=%v", ok, err)
	}
	return st.ServerKnowledge
}

// dump renders every mirror row of the plan, for exact before/after compares.
func (h *harness) dump() string {
	h.t.Helper()
	var sb strings.Builder
	for _, table := range []string{"accounts", "category_groups", "categories", "payees", "transactions",
		"subtransactions", "months", "month_categories", "scheduled_transactions", "scheduled_subtransactions"} {
		rows, err := h.db.Reader().Query("SELECT * FROM "+table+" WHERE plan_id = ? ORDER BY 2, 3", planID)
		if err != nil {
			h.t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				h.t.Fatal(err)
			}
			fmt.Fprintf(&sb, "%s %v\n", table, vals)
		}
		rows.Close()
	}
	return sb.String()
}

func (h *harness) assertLogsRedacted() {
	h.t.Helper()
	for _, id := range []string{planID, acctA, acctB, "t-imp", "t-split"} {
		if strings.Contains(h.logs.String(), id) {
			h.t.Errorf("logs leak %q:\n%s", id, h.logs.String())
		}
	}
}

func TestSyncPlan_InitialLoad(t *testing.T) {
	h := newHarness(t)
	res := h.tick()

	if !res.Initial || res.Skipped || res.ServerKnowledge != 100 {
		t.Errorf("result = %+v, want initial at 100", res)
	}
	if got := strings.Join(h.api.calls, ","); got != "export" {
		t.Errorf("calls = %s, want export", got)
	}
	want := map[string]int{"accounts": 2, "category_groups": 1, "categories": 1, "payees": 1,
		"transactions": 2, "subtransactions": 2, "months": 1, "month_categories": 1}
	for table, n := range want {
		if got := h.count(table, ""); got != n {
			t.Errorf("%s: %d rows, want %d", table, got, n)
		}
		if res.Rows[table] != n {
			t.Errorf("result rows[%s] = %d, want %d", table, res.Rows[table], n)
		}
	}
	if h.cursor() != 100 {
		t.Errorf("cursor = %d, want 100", h.cursor())
	}
	if len(res.LinkEvents) != 0 {
		t.Errorf("initial load emitted %d link events; want baseline only", len(res.LinkEvents))
	}
	h.assertLogsRedacted()
}

func TestSyncPlan_DeltaUpdate(t *testing.T) {
	h := newHarness(t)
	h.tick()
	changed := txn("t-imp", acctA, 1, -6000, ptr("YNAB:-5000:2026-10-01:1"))
	changed.Approved = true
	h.api.setDelta(100, 105, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{
		changed, txn("t-new", acctB, 28, -100, nil),
	}})

	res := h.tick()
	if res.Initial || res.ServerKnowledge != 105 || res.Rows["transactions"] != 2 {
		t.Errorf("result = %+v", res)
	}
	if got := strings.Join(h.api.calls, ","); got != "export,delta@100" {
		t.Errorf("calls = %s", got)
	}
	if h.count("transactions", "id = 't-imp' AND amount = -6000 AND approved = 1") != 1 {
		t.Error("t-imp not updated")
	}
	if h.count("transactions", "") != 3 || h.cursor() != 105 {
		t.Errorf("transactions=%d cursor=%d, want 3 and 105", h.count("transactions", ""), h.cursor())
	}

	// An idle tick advances nothing but still costs one delta.
	h.tick()
	if h.cursor() != 105 || h.api.calls[len(h.api.calls)-1] != "delta@105" {
		t.Errorf("idle tick: cursor=%d calls=%v", h.cursor(), h.api.calls)
	}
}

func TestSyncPlan_Tombstone(t *testing.T) {
	h := newHarness(t)
	h.tick()
	gone := txn("t-imp", acctA, 1, -5000, ptr("YNAB:-5000:2026-10-01:1"))
	gone.Deleted = true
	h.api.setDelta(100, 101, &ynab.PlanDetail{
		Transactions: []ynabapi.TransactionBase{gone},
		Payees:       []ynabapi.Payee{{ID: "payee-1", Name: "Grocer", Deleted: true}},
	})
	h.tick()

	if h.count("transactions", "id = 't-imp' AND deleted = 1") != 1 {
		t.Error("deleted transaction not kept as a tombstone")
	}
	if h.count("payees", "id = 'payee-1' AND deleted = 1") != 1 {
		t.Error("deleted payee not kept as a tombstone")
	}
	if h.count("transactions", "deleted = 0") != 1 {
		t.Errorf("live transactions = %d, want 1", h.count("transactions", "deleted = 0"))
	}
}

func TestSyncPlan_SplitStitching(t *testing.T) {
	h := newHarness(t)
	h.tick()
	if h.count("subtransactions", "transaction_id = 't-split' AND deleted = 0") != 2 {
		t.Fatal("split children not stored under their parent")
	}

	// A child whose parent is in neither the delta nor the mirror is dropped
	// instead of failing the deferred FK; a child of a known parent applies.
	h.api.setDelta(100, 101, &ynab.PlanDetail{Subtransactions: []ynabapi.SubtransactionBase{
		sub("s-orphan", "t-unknown", -1), sub("s-1", "t-split", -4500),
	}})
	res := h.tick()
	if res.Orphans != 1 || h.count("subtransactions", "id = 's-orphan'") != 0 {
		t.Errorf("orphans=%d, want 1 dropped", res.Orphans)
	}
	if n := strings.Count(h.logs.String(), "dropped 1 orphan subtransactions"); n != 1 {
		t.Errorf("orphan log lines = %d, want 1:\n%s", n, h.logs.String())
	}
	if strings.Contains(h.logs.String(), "s-orphan") || strings.Contains(h.logs.String(), "t-unknown") {
		t.Errorf("orphan log names ids:\n%s", h.logs.String())
	}
	if h.count("subtransactions", "id = 's-1' AND amount = -4500") != 1 || h.cursor() != 101 {
		t.Error("child of a mirrored parent not applied")
	}

	// A new split arrives with its children in one delta.
	h.api.setDelta(101, 102, &ynab.PlanDetail{
		Transactions:    []ynabapi.TransactionBase{txn("t-split2", acctB, 25, -300, nil)},
		Subtransactions: []ynabapi.SubtransactionBase{sub("s-3", "t-split2", -100), sub("s-4", "t-split2", -200)},
	})
	h.tick()
	if h.count("subtransactions", "transaction_id = 't-split2'") != 2 {
		t.Error("new split's children not stitched")
	}

	// A parent tombstone without its children takes them with it.
	gone := txn("t-split", acctA, 20, -9000, nil)
	gone.Deleted = true
	h.api.setDelta(102, 103, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{gone}})
	h.tick()
	if n := h.count("subtransactions", "transaction_id = 't-split' AND deleted = 1"); n != 2 {
		t.Errorf("%d of 2 children tombstoned with their parent", n)
	}
	if h.count("subtransactions", "id = 's-1' AND amount = -4500") != 1 {
		t.Error("tombstoned child lost its data")
	}
}

// first_seen_at is the tick's sync time, the same instant as synced_at, not
// a separate time.Now inside the store.
func TestSyncPlan_FirstSeenIsTickSyncTime(t *testing.T) {
	h := newHarness(t)
	t1 := time.Date(2026, 10, 29, 9, 30, 0, 123456789, time.UTC)
	h.s.now = func() time.Time { return t1 }
	h.tick()

	t2 := t1.Add(10 * time.Minute)
	h.s.now = func() time.Time { return t2 }
	h.api.setDelta(100, 101, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{
		txn("t-new", acctB, 28, -100, ptr("YNAB:-100:2026-10-28:1")),
	}})
	h.tick()

	firstSeen := func(id string) time.Time {
		t.Helper()
		var s string
		if err := h.db.Reader().QueryRow(`SELECT first_seen_at FROM transactions WHERE plan_id = ? AND id = ?`,
			planID, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		v, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := firstSeen("t-imp"); !got.Equal(t1) {
		t.Errorf("initial load first_seen_at = %v, want %v", got, t1)
	}
	if got := firstSeen("t-new"); !got.Equal(t2) {
		t.Errorf("delta first_seen_at = %v, want %v", got, t2)
	}
	st, _, _ := h.db.GetSyncState(context.Background(), planID)
	if !st.SyncedAt.Equal(t2) {
		t.Errorf("synced_at = %v, want %v", st.SyncedAt, t2)
	}
}

func TestSyncPlan_CrashBeforeCursorSaveReappliesIdempotently(t *testing.T) {
	h := newHarness(t)
	h.tick()
	before := h.dump()
	h.api.setDelta(100, 105, &ynab.PlanDetail{
		Transactions: []ynabapi.TransactionBase{
			txn("t-imp", acctA, 1, -7000, ptr("YNAB:-5000:2026-10-01:1")),
			txn("t-new", acctB, 28, -100, ptr("YNAB:-100:2026-10-28:1")),
		},
		Subtransactions: []ynabapi.SubtransactionBase{sub("s-1", "t-split", -4100)},
	})

	crash := errors.New("simulated crash")
	h.s.beforeCursor = func() error { return crash }
	if _, err := h.s.SyncPlan(context.Background(), planID); !errors.Is(err, crash) {
		t.Fatalf("SyncPlan = %v, want the crash", err)
	}
	if h.cursor() != 100 || h.dump() != before {
		t.Fatalf("crashed tick left changes: cursor=%d", h.cursor())
	}

	h.s.beforeCursor = nil
	h.tick() // re-applies the same delta
	if h.cursor() != 105 || h.count("transactions", "") != 3 {
		t.Fatalf("retry: cursor=%d txns=%d", h.cursor(), h.count("transactions", ""))
	}
	applied := h.dump()

	// Applying the same delta again changes nothing, first_seen_at included.
	if err := h.db.SetSyncState(context.Background(), store.SyncState{PlanID: planID, ServerKnowledge: 100,
		SyncedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	h.tick()
	if got := h.dump(); got != applied {
		t.Errorf("re-applying the delta changed rows:\nbefore:\n%s\nafter:\n%s", applied, got)
	}
}

func TestSyncPlan_LinkErrorDedupe(t *testing.T) {
	h := newHarness(t) // A is in error from the start
	if res := h.tick(); len(res.LinkEvents) != 0 {
		t.Fatalf("initial load: %v", res.LinkEvents)
	}

	both := &ynab.PlanDetail{Accounts: []ynabapi.AccountBase{account(acctA, true), account(acctB, true)}}
	h.api.setDelta(100, 101, both)
	h.api.setDelta(101, 102, both) // same state again
	h.api.setDelta(102, 103, &ynab.PlanDetail{Accounts: []ynabapi.AccountBase{account(acctB, false)}})

	res := h.tick()
	if len(res.LinkEvents) != 1 || res.LinkEvents[0].AccountID != acctB || !res.LinkEvents[0].InError {
		t.Errorf("B breaking: events = %+v, want one for B in error", res.LinkEvents)
	}
	if res := h.tick(); len(res.LinkEvents) != 0 {
		t.Errorf("unchanged state re-emitted: %+v", res.LinkEvents)
	}
	res = h.tick()
	if len(res.LinkEvents) != 1 || res.LinkEvents[0].AccountID != acctB || res.LinkEvents[0].InError {
		t.Errorf("B recovering: events = %+v, want one for B healthy", res.LinkEvents)
	}

	// A new account that arrives already broken is reported.
	h.api.setDelta(103, 104, &ynab.PlanDetail{Accounts: []ynabapi.AccountBase{
		account("aaaaaaaa-0000-0000-0000-000000000003", true)}})
	if res := h.tick(); len(res.LinkEvents) != 1 {
		t.Errorf("new broken account: events = %+v", res.LinkEvents)
	}
	h.assertLogsRedacted()
}

func TestFreshness(t *testing.T) {
	h := newHarness(t)
	h.tick()
	ctx := context.Background()

	fr, err := h.s.Freshness(ctx, planID)
	if err != nil {
		t.Fatal(err)
	}
	if len(fr) != 2 {
		t.Fatalf("got %d accounts, want 2", len(fr))
	}
	a, b := fr[0], fr[1]
	// The manual split on Oct 20 is newer but has no import_id.
	if a.AccountID != acctA || a.NewestImportDate != "2026-10-01" || a.LastImportSeenAt == nil || !a.InError {
		t.Errorf("A = %+v", a)
	}
	if b.NewestImportDate != "" || b.LastImportSeenAt != nil || a.SyncedAt.IsZero() {
		t.Errorf("B = %+v", b)
	}
	initialSeen := *a.LastImportSeenAt

	time.Sleep(5 * time.Millisecond)
	start := time.Now()
	h.api.setDelta(100, 101, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{
		txn("t-imp2", acctA, 28, -100, ptr("YNAB:-100:2026-10-28:1")),
	}})
	h.tick()
	fr, err = h.s.Freshness(ctx, planID)
	if err != nil {
		t.Fatal(err)
	}
	a = fr[0]
	if a.NewestImportDate != "2026-10-28" || !a.LastImportSeenAt.After(initialSeen) || a.LastImportSeenAt.Before(start) {
		t.Errorf("after a new import: A = %+v (start %v)", a, start)
	}
}

func TestSyncPlan_BudgetExhaustedSkipsTick(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A real client whose only token is the write reserve: reads are refused
	// before anything reaches the transport.
	gov := ynab.NewGovernor(1, 1, nil)
	sent := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		sent++
		return nil, errors.New("must not be called")
	})
	var logs bytes.Buffer
	s := New(db, func(id string) PlanAPI {
		return ynab.NewClient("tok", id, log.New(io.Discard, "", 0), ynab.WithGovernor(gov),
			ynab.WithBaseTransport(transport))
	}, log.New(&logs, "", 0))

	res, err := s.SyncPlan(context.Background(), planID)
	if err != nil {
		t.Fatalf("SyncPlan = %v, want a skipped tick", err)
	}
	if !res.Skipped || sent != 0 {
		t.Errorf("result=%+v sent=%d, want skipped with nothing sent", res, sent)
	}
	if _, ok, _ := db.GetSyncState(context.Background(), planID); ok {
		t.Error("skipped tick wrote a cursor")
	}
	if !strings.Contains(logs.String(), "skipping tick") || strings.Contains(logs.String(), planID) {
		t.Errorf("log = %q", logs.String())
	}

	// A fake returning the sentinel mid-chain is skipped the same way.
	h := newHarness(t)
	h.tick()
	h.api.err = fmt.Errorf("plan delta: %w", ynab.ErrBudgetExhausted)
	if res := h.tick(); !res.Skipped || h.cursor() != 100 {
		t.Errorf("delta skip: %+v cursor=%d", res, h.cursor())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTriggerImport(t *testing.T) {
	h := newHarness(t)
	h.api.imported = []string{"x", "y", "z"}
	res, err := h.s.TriggerImport(context.Background(), planID)
	if err != nil || res.Imported != 3 || res.Skipped {
		t.Errorf("TriggerImport = %+v, %v", res, err)
	}

	h.api.err = fmt.Errorf("import transactions: %w", ynab.ErrBudgetExhausted)
	if res, err := h.s.TriggerImport(context.Background(), planID); err != nil || !res.Skipped {
		t.Errorf("exhausted: %+v, %v", res, err)
	}
	h.api.err = errors.New("boom")
	if _, err := h.s.TriggerImport(context.Background(), planID); err == nil {
		t.Error("want error")
	}
}

type fakeLister []ynab.PlanSummary

func (l fakeLister) Plans(context.Context) ([]ynab.PlanSummary, error) { return l, nil }

func TestScanDiscretionary(t *testing.T) {
	group := func(name string, deleted bool) []*ynab.CategoryGroup {
		c := ynabapi.Category{}
		c.Name, c.Deleted = name, deleted
		return []*ynab.CategoryGroup{{Name: "G", Categories: []ynabapi.Category{c}}}
	}
	const brokenID = "33333333-3333-3333-3333-333333333333"
	apis := map[string]*fakeAPI{
		"old":     {groups: group(" discretionary", false)},
		"gone":    {groups: group("Discretionary", true)},
		"other":   {groups: group("Groceries", false)},
		brokenID:  {groups: group("Discretionary", false), err: fmt.Errorf("fetch categories %s: boom", brokenID)},
		"current": {groups: group("Discretionary", false)},
	}
	// More plans than the old live run's cap of 8, so a cap would show.
	lister := fakeLister{{ID: "old", Name: "Old"}, {ID: "gone", Name: "Gone"}, {ID: "other", Name: "Other"},
		{ID: brokenID, Name: "Broken"}, {ID: "current", Name: "Current"}}
	for i := range 6 {
		id := fmt.Sprintf("extra-%d", i)
		apis[id] = &fakeAPI{groups: group("Rent", false)}
		lister = append(lister, ynab.PlanSummary{ID: id, Name: id})
	}
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A previous scan's plan that is no longer reachable must not survive.
	if err := db.ReplacePlanScans(context.Background(), []store.PlanScan{{PlanID: "stale", Name: "Stale"}}); err != nil {
		t.Fatal(err)
	}

	got, err := ScanDiscretionary(context.Background(), lister, func(id string) PlanAPI { return apis[id] }, db)
	var scanErr *ScanError
	if !errors.As(err, &scanErr) || scanErr.Scanned != len(lister) || len(scanErr.Failed) != 1 ||
		scanErr.Failed[0].Name != "Broken" {
		t.Fatalf("err = %v, want a ScanError naming the broken plan", err)
	}
	if strings.Contains(err.Error(), brokenID) {
		t.Errorf("scan error leaks the plan id: %v", err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, ",") != "old,current" {
		t.Errorf("got %v, want [old current]", ids)
	}
	for id, api := range apis {
		if len(api.calls) != 1 {
			t.Errorf("plan %s: calls %v, want one categories fetch", id, api.calls)
		}
	}

	rows, err := db.PlanScans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(lister) {
		t.Fatalf("stored %d rows, want %d", len(rows), len(lister))
	}
	for _, r := range rows {
		switch {
		case r.PlanID == "stale":
			t.Error("stale row from an earlier scan survived")
		case r.ScannedAt.IsZero():
			t.Errorf("%s: no scanned_at", r.PlanID)
		case r.PlanID == brokenID:
			if r.ScanError == "" || strings.Contains(r.ScanError, brokenID) || r.HasDiscretionary {
				t.Errorf("broken plan row = %+v", r)
			}
		case r.HasDiscretionary != (r.PlanID == "old" || r.PlanID == "current"):
			t.Errorf("%s: has_discretionary = %v", r.PlanID, r.HasDiscretionary)
		}
	}

	// A failed list stores nothing.
	failing := failingLister{errors.New("down")}
	if _, err := ScanDiscretionary(context.Background(), failing, nil, db); err == nil {
		t.Fatal("want list error")
	}
	if rows2, _ := db.PlanScans(context.Background()); len(rows2) != len(lister) {
		t.Errorf("failed list changed stored rows: %d", len(rows2))
	}
}

type failingLister struct{ err error }

func (l failingLister) Plans(context.Context) ([]ynab.PlanSummary, error) { return nil, l.err }

func TestSyncPlan_PlansAreKeyedByID(t *testing.T) {
	h := newHarness(t)
	h.tick()
	other := &fakeAPI{export: baseExport(), exportSK: 7}
	h.s.client = func(id string) PlanAPI {
		if id == planID {
			return h.api
		}
		return other
	}
	const archived = "22222222-2222-2222-2222-222222222222"
	if _, err := h.s.SyncPlan(context.Background(), archived); err != nil {
		t.Fatal(err)
	}
	var n int
	h.db.Reader().QueryRow("SELECT COUNT(*) FROM transactions WHERE plan_id = ?", archived).Scan(&n)
	if n != 2 || h.count("transactions", "") != 2 {
		t.Errorf("archived plan rows = %d, active = %d; want 2 each", n, h.count("transactions", ""))
	}
}
