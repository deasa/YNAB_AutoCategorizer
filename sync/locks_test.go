package sync

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
	ynabapi "pkg.venceslau.dev/ynab"
)

// lockExport adds to baseExport a second real category, YNAB's internal
// Uncategorized, and one row per first-touch case.
func lockExport() *ynab.PlanDetail {
	d := baseExport()
	d.Categories = append(d.Categories,
		ynabapi.CategoryBase{ID: "cat-2", CategoryGroupID: "grp-1", Name: "Groceries"},
		ynabapi.CategoryBase{ID: "cat-u", CategoryGroupID: "grp-1", Name: "Uncategorized", Internal: true})
	d.Transactions = append(d.Transactions,
		categorized("t-human", true, ptr("cat-1")),  // approved, real category: first touch
		categorized("t-guess", false, ptr("cat-1")), // unapproved: YNAB's payee-memory guess
		categorized("t-bare", true, nil),            // approved with no category
		categorized("t-uncat", true, ptr("cat-u")),  // approved, internal Uncategorized
	)
	return d
}

func categorized(id string, approved bool, cat *string) ynabapi.TransactionBase {
	t := txn(id, acctB, 5, -1200, nil)
	t.Approved = approved
	t.CategoryID = cat
	return t
}

func newLockHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.api.export = lockExport()
	h.s.DetectLocks(planID)
	return h
}

// locks renders the plan's human_locks as sorted "txn:field:source".
func (h *harness) locks() string {
	h.t.Helper()
	rows, err := h.db.Reader().Query(`SELECT txn_id || ':' || field || ':' || source FROM human_locks
WHERE plan_id = ? ORDER BY 1`, planID)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

// write logs a write of after to txnID as an intent and returns its id; done
// also marks it done.
func (h *harness) write(txnID, after string, done bool) int64 {
	h.t.Helper()
	ctx := context.Background()
	id, err := h.db.InsertWriteIntent(ctx, store.WriteEntry{PlanID: planID, Actor: store.ActorSystem,
		Via: store.ViaScheduler, Tier: "auto", Endpoint: "PATCH /transactions", TxnID: ptr(txnID),
		BeforeJSON: json.RawMessage(`{"category_id":null,"approved":false}`), AfterJSON: json.RawMessage(after)})
	if err != nil {
		h.t.Fatal(err)
	}
	if done {
		h.done(id)
	}
	return id
}

func (h *harness) done(id int64) {
	h.t.Helper()
	if err := h.db.MarkWriteDone(context.Background(), id, nil); err != nil {
		h.t.Fatal(err)
	}
}

// impWith is t-imp as a delta carries it after an edit.
func impWith(approved bool, cat *string, memo *string) ynabapi.TransactionBase {
	t := txn("t-imp", acctA, 1, -5000, ptr("YNAB:-5000:2026-10-01:1"))
	t.Approved, t.CategoryID, t.Memo = approved, cat, memo
	return t
}

const ourWrite = `{"category_id":"cat-1","approved":true}`

func TestDetectLocks_FirstTouch(t *testing.T) {
	h := newLockHarness(t)
	res := h.tick()

	// Only the approved row with a real category is a person's choice.
	if got := h.locks(); got != "t-human:category:first_touch" {
		t.Errorf("locks = %q", got)
	}
	want := LockCounts{Backfill: true, Judged: 6, FirstTouch: 1}
	if res.Locks != want {
		t.Errorf("Locks = %+v, want %+v", res.Locks, want)
	}
	if !strings.Contains(h.logs.String(), "backfill=true judged=6 ynab_edit=0 first_touch=1 skipped_intent=0") {
		t.Errorf("summary log missing counts:\n%s", h.logs.String())
	}
	h.assertLogsRedacted()
	for _, id := range []string{"t-human", "cat-1"} {
		if strings.Contains(h.logs.String(), id) {
			t.Errorf("logs leak %q", id)
		}
	}

	// A later delta approving the guess makes it a person's choice too.
	h.api.setDelta(100, 101, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{
		categorized("t-guess", true, ptr("cat-1")), categorized("t-bare", true, nil)}})
	res = h.tick()
	if got := h.locks(); got != "t-guess:category:first_touch,t-human:category:first_touch" {
		t.Errorf("after approval, locks = %q", got)
	}
	if want := (LockCounts{Judged: 2, FirstTouch: 1}); res.Locks != want {
		t.Errorf("delta Locks = %+v, want %+v", res.Locks, want)
	}
}

func TestDetectLocks_EchoDoesNotLockButDivergenceDoes(t *testing.T) {
	h := newLockHarness(t)
	h.tick()
	h.write("t-imp", ourWrite, true)

	// Our own write echoing back matches after_json: no lock, and no first-touch
	// lock either, though the row is now approved and categorized.
	h.api.setDelta(100, 101, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{impWith(true, ptr("cat-1"), nil)}})
	res := h.tick()
	if got := h.locks(); got != "t-human:category:first_touch" {
		t.Fatalf("echo locked: %q", got)
	}
	if want := (LockCounts{Judged: 1}); res.Locks != want {
		t.Errorf("echo Locks = %+v, want %+v", res.Locks, want)
	}

	// A memo edit isn't compared: our write never set the memo.
	h.api.setDelta(101, 102, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{
		impWith(true, ptr("cat-1"), ptr("note"))}})
	h.tick()
	if got := h.locks(); got != "t-human:category:first_touch" {
		t.Fatalf("memo edit locked: %q", got)
	}

	// A person recategorizes: category_id diverges and is locked as "category".
	h.api.setDelta(102, 103, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{
		impWith(true, ptr("cat-2"), ptr("note"))}})
	res = h.tick()
	if got := h.locks(); got != "t-human:category:first_touch,t-imp:category:ynab_edit" {
		t.Fatalf("divergence: locks = %q", got)
	}
	if want := (LockCounts{Judged: 1, Edit: 1}); res.Locks != want {
		t.Errorf("divergence Locks = %+v, want %+v", res.Locks, want)
	}

	// Each diverging field is locked on its own; approved here.
	h.api.setDelta(103, 104, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{
		impWith(false, ptr("cat-2"), ptr("note"))}})
	h.tick()
	if got := h.locks(); got != "t-human:category:first_touch,t-imp:approved:ynab_edit,t-imp:category:ynab_edit" {
		t.Errorf("approved divergence: locks = %q", got)
	}
}

func TestDetectLocks_ComparesAgainstTheLatestDoneWrite(t *testing.T) {
	h := newLockHarness(t)
	h.tick()
	h.write("t-imp", ourWrite, true)
	h.write("t-imp", `{"category_id":"cat-2","approved":true}`, true)
	h.write("t-imp", `{"category_id":null,"approved":true}`, false)
	h.failOpenIntents()

	h.api.setDelta(100, 101, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{impWith(true, ptr("cat-2"), nil)}})
	h.tick()
	if got := h.locks(); got != "t-human:category:first_touch" {
		t.Errorf("matching the latest done write locked: %q", got)
	}
}

func (h *harness) failOpenIntents() {
	h.t.Helper()
	open, err := h.db.UnfinishedWrites(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	for _, w := range open {
		if err := h.db.MarkWriteFailed(context.Background(), w.ID, "boom"); err != nil {
			h.t.Fatal(err)
		}
	}
}

func TestDetectLocks_UnresolvedIntentIsNotJudged(t *testing.T) {
	h := newLockHarness(t)
	h.tick()

	// Post-crash: our PATCH reached YNAB but the intent was never marked done.
	// The sync brings the echo (approved, categorized) back before
	// writer.Reconcile runs; rule (b) would call it a first touch.
	id := h.write("t-imp", ourWrite, false)
	h.api.setDelta(100, 101, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{impWith(true, ptr("cat-1"), nil)}})
	res := h.tick()
	if got := h.locks(); got != "t-human:category:first_touch" {
		t.Fatalf("open intent judged: %q", got)
	}
	if want := (LockCounts{Judged: 1, SkippedIntent: 1}); res.Locks != want {
		t.Errorf("Locks = %+v, want %+v", res.Locks, want)
	}

	// Reconcile marks it done; the echo, carried again, still matches.
	h.done(id)
	h.api.setDelta(101, 102, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{impWith(true, ptr("cat-1"), nil)}})
	h.tick()
	if got := h.locks(); got != "t-human:category:first_touch" {
		t.Fatalf("reconciled echo locked: %q", got)
	}

	// With a done write and a newer open intent, a diverging row is skipped too:
	// it may be the new write landing, not a person.
	h.write("t-imp", `{"category_id":"cat-2","approved":true}`, false)
	h.api.setDelta(102, 103, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{impWith(true, ptr("cat-2"), nil)}})
	res = h.tick()
	if got := h.locks(); got != "t-human:category:first_touch" {
		t.Errorf("in-flight write judged: %q", got)
	}
	if res.Locks.SkippedIntent != 1 {
		t.Errorf("Locks = %+v, want SkippedIntent 1", res.Locks)
	}
}

func TestDetectLocks_BackfillRunsOnce(t *testing.T) {
	h := newHarness(t)
	h.api.export = lockExport()
	h.tick() // mirrored before detection existed
	if got := h.locks(); got != "" {
		t.Fatalf("detection off, locks = %q", got)
	}

	// The first tick with detection judges every live row, though the delta is
	// idle, and locks the pre-existing approved row.
	h.s.DetectLocks(planID)
	res := h.tick()
	if got := h.locks(); got != "t-human:category:first_touch" {
		t.Errorf("backfill locks = %q", got)
	}
	if want := (LockCounts{Backfill: true, Judged: 6, FirstTouch: 1}); res.Locks != want {
		t.Errorf("backfill Locks = %+v, want %+v", res.Locks, want)
	}

	// It doesn't run again: an idle tick judges nothing, and a delta judges
	// only the rows it carries.
	res = h.tick()
	if res.Locks != (LockCounts{}) {
		t.Errorf("idle tick: Locks = %+v", res.Locks)
	}
	h.api.setDelta(100, 101, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{
		categorized("t-bare", true, nil)}})
	if res = h.tick(); res.Locks != (LockCounts{Judged: 1}) {
		t.Errorf("delta tick: Locks = %+v", res.Locks)
	}
	if n := h.count("lock_detection", ""); n != 1 {
		t.Errorf("lock_detection rows = %d, want 1", n)
	}
}

func TestDetectLocks_OtherPlansAreNotJudged(t *testing.T) {
	h := newHarness(t)
	h.api.export = lockExport()
	h.s.DetectLocks("22222222-2222-2222-2222-222222222222")
	res := h.tick()
	if got := h.locks(); got != "" || res.Locks != (LockCounts{}) {
		t.Errorf("non-active plan judged: locks = %q, Locks = %+v", got, res.Locks)
	}
	if strings.Contains(h.logs.String(), "human locks") {
		t.Errorf("non-active plan logged lock counts:\n%s", h.logs.String())
	}
}

func TestDetectLocks_CommitAndRollBackWithRowsAndCursor(t *testing.T) {
	h := newLockHarness(t)
	crash := errors.New("simulated crash")

	// A crashed initial load leaves no rows, no locks and no backfill mark.
	h.s.beforeCursor = func() error { return crash }
	if _, err := h.s.SyncPlan(context.Background(), planID); !errors.Is(err, crash) {
		t.Fatalf("SyncPlan = %v, want the crash", err)
	}
	if h.locks() != "" || h.count("lock_detection", "") != 0 || h.count("transactions", "") != 0 {
		t.Fatalf("crashed load left locks %q, marks %d, txns %d", h.locks(), h.count("lock_detection", ""),
			h.count("transactions", ""))
	}
	h.s.beforeCursor = nil
	h.tick()
	if got := h.locks(); got != "t-human:category:first_touch" || h.count("lock_detection", "") != 1 {
		t.Fatalf("retried load: locks = %q", got)
	}

	// A crashed delta rolls its lock back with the rows and cursor.
	h.write("t-imp", ourWrite, true)
	h.api.setDelta(100, 101, &ynab.PlanDetail{Transactions: []ynabapi.TransactionBase{impWith(true, ptr("cat-2"), nil)}})
	h.s.beforeCursor = func() error { return crash }
	if _, err := h.s.SyncPlan(context.Background(), planID); !errors.Is(err, crash) {
		t.Fatalf("SyncPlan = %v, want the crash", err)
	}
	if got := h.locks(); got != "t-human:category:first_touch" || h.cursor() != 100 {
		t.Fatalf("crashed delta left lock %q, cursor %d", got, h.cursor())
	}
	h.s.beforeCursor = nil
	h.tick()
	if got := h.locks(); got != "t-human:category:first_touch,t-imp:category:ynab_edit" || h.cursor() != 101 {
		t.Errorf("retried delta: locks = %q, cursor %d", got, h.cursor())
	}
}

func TestDivergent(t *testing.T) {
	fields := map[string]any{"amount": int64(-5000), "memo": nil, "category_id": "cat-1", "approved": true}
	for _, tc := range []struct{ after, want string }{
		{`{"amount":-5000,"memo":null,"category_id":"cat-1","approved":true}`, ""},
		{`{"amount":-5001,"memo":"x","category_id":null,"approved":false}`, "amount,approved,category_id,memo"},
		{`{"subtransactions":[],"payee_name":"x"}`, ""}, // not mirrored here: never compared
		{`null`, ""}, // a delete
		{`[1]`, ""},
	} {
		got, err := divergent(store.LockCandidate{Fields: fields, LastDone: json.RawMessage(tc.after)})
		if err != nil || strings.Join(got, ",") != tc.want {
			t.Errorf("divergent(%s) = %v, %v; want %q", tc.after, got, err, tc.want)
		}
	}
}
