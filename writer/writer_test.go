package writer

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

const (
	planID    = "11111111-2222-3333-4444-555555555555"
	otherPlan = "99999999-2222-3333-4444-555555555555"
	acct      = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	txn1      = "00000000-0000-4000-8000-000000000001"
	txn2      = "00000000-0000-4000-8000-000000000002"
	txn3      = "00000000-0000-4000-8000-000000000003"
	catGroc   = "cccccccc-0000-4000-8000-000000000001"
	catFun    = "cccccccc-0000-4000-8000-000000000002"
	autoSince = "2026-11-01"
)

var allIDs = []string{planID, otherPlan, acct, txn1, txn2, txn3, catGroc, catFun}

// clock is a race-safe fake clock that advances one second per call.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

// fakeAPI answers a PATCH with every change applied, unless resp or err is set.
type fakeAPI struct {
	mu     sync.Mutex
	calls  [][]ynab.TransactionChange
	during func([]ynab.TransactionChange) // runs inside the call
	resp   func([]ynab.TransactionChange) []ynab.PatchedTransaction
	err    error
}

func (f *fakeAPI) PatchTransactions(_ context.Context, ch []ynab.TransactionChange) ([]ynab.PatchedTransaction, error) {
	f.mu.Lock()
	f.calls = append(f.calls, ch)
	during, resp, err := f.during, f.resp, f.err
	f.mu.Unlock()
	if during != nil {
		during(ch)
	}
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return resp(ch), nil
	}
	return echo(ch), nil
}

func (f *fakeAPI) nCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func echo(ch []ynab.TransactionChange) []ynab.PatchedTransaction {
	out := make([]ynab.PatchedTransaction, len(ch))
	for i, c := range ch {
		out[i] = ynab.PatchedTransaction{ID: c.ID, CategoryID: c.CategoryID, Approved: c.Approved != nil && *c.Approved}
	}
	return out
}

type env struct {
	t      *testing.T
	db     *store.DB
	api    *fakeAPI
	gate   *Gate
	clock  *clock
	w      *Writer
	resync func(context.Context) error // set to change the mirror during Apply's re-sync
	syncs  atomic.Int32
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{t: t, db: db, api: &fakeAPI{}, gate: NewGate(), clock: &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}}
	e.gate.Set(ModeAuto)
	e.api.resp = e.echoMirror
	e.w = New(Config{
		PlanID: planID, AutoSince: autoSince, Gate: e.gate, API: e.api, DB: db, Now: e.clock.Now,
		Resync: func(ctx context.Context) error {
			e.syncs.Add(1)
			if e.resync != nil {
				return e.resync(ctx)
			}
			return nil
		},
	})
	return e
}

func ptr[T any](v T) *T { return &v }

// echoMirror answers like YNAB: each transaction's whole state, the mirror row
// with the change applied.
func (e *env) echoMirror(ch []ynab.TransactionChange) []ynab.PatchedTransaction {
	out := make([]ynab.PatchedTransaction, len(ch))
	for i, c := range ch {
		row, _, err := e.db.TransactionByID(context.Background(), planID, c.ID)
		if err != nil {
			e.t.Error(err)
		}
		out[i] = ynab.PatchedTransaction{ID: c.ID, CategoryID: row.CategoryID, Approved: row.Approved}
		if c.CategoryID != nil {
			out[i].CategoryID = c.CategoryID
		}
		if c.Approved != nil {
			out[i].Approved = *c.Approved
		}
	}
	return out
}

// put upserts a transaction into the mirror.
func (e *env) put(id, date string, cat *string, approved bool) {
	e.t.Helper()
	err := e.db.Update(context.Background(), func(tx *store.Tx) error {
		return tx.UpsertTransactions(planID, []store.Transaction{{
			ID: id, Date: date, Amount: -5000, Cleared: "cleared", Approved: approved, AccountID: acct, CategoryID: cat,
		}}, e.clock.Now())
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) writes(id string) []store.WriteEntry {
	e.t.Helper()
	rows, err := e.db.Reader().Query(`SELECT status, COALESCE(error, '') FROM write_log WHERE txn_id = ? ORDER BY id`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []store.WriteEntry
	for rows.Next() {
		var we store.WriteEntry
		var msg string
		if err := rows.Scan(&we.Status, &msg); err != nil {
			e.t.Fatal(err)
		}
		if msg != "" {
			we.Error = &msg
		}
		out = append(out, we)
	}
	return out
}

func (e *env) status(id string) string {
	e.t.Helper()
	ws := e.writes(id)
	if len(ws) != 1 {
		e.t.Fatalf("got %d write_log rows, want 1", len(ws))
	}
	return ws[0].Status
}

func categorize(id, cat string) Write { return Write{TxnID: id, CategoryID: cat, Approve: true} }

func batch(ws ...Write) Batch { return Batch{PlanID: planID, Writes: ws} }

func assertNoIDs(t *testing.T, s string) {
	t.Helper()
	for _, id := range allIDs {
		if strings.Contains(s, id) {
			t.Errorf("text leaks an id: %q", s)
		}
	}
}

func TestGate_NewIsDryRun(t *testing.T) {
	if m := NewGate().Mode(); m != ModeDryRun {
		t.Errorf("new Gate = %q, want dry_run", m)
	}
	var g Gate
	if m := g.Mode(); m != ModeDryRun {
		t.Errorf("zero Gate = %q, want dry_run", m)
	}
}

func TestApply_GatesRefuseWithoutHTTP(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.put(txn1, "2026-11-05", nil, false)

	// A writer on a new Gate: dry_run.
	fresh := New(Config{PlanID: planID, AutoSince: autoSince, API: e.api, DB: e.db, Resync: e.w.cfg.Resync})
	if _, err := fresh.Apply(ctx, batch(categorize(txn1, catGroc)), store.ActorSystem, store.ViaScheduler); !errors.Is(err, ErrDryRun) {
		t.Errorf("new gate: err %v, want ErrDryRun", err)
	}

	// auto → dry_run blocks the next Apply.
	if _, err := e.w.Apply(ctx, batch(categorize(txn1, catGroc)), store.ActorSystem, store.ViaScheduler); err != nil {
		t.Fatalf("auto: %v", err)
	}
	e.put(txn2, "2026-11-05", nil, false)
	e.gate.Set(ModeDryRun)
	if _, err := e.w.Apply(ctx, batch(categorize(txn2, catGroc)), store.ActorSystem, store.ViaScheduler); !errors.Is(err, ErrDryRun) {
		t.Errorf("after flip: err %v, want ErrDryRun", err)
	}
	e.gate.Set(ModeOff)
	if _, err := e.w.Apply(ctx, batch(categorize(txn2, catGroc)), store.ActorSystem, store.ViaScheduler); !errors.Is(err, ErrDryRun) {
		t.Errorf("off: err %v, want ErrDryRun", err)
	}

	// A flip during the re-sync blocks this Apply.
	e.gate.Set(ModeAuto)
	e.resync = func(context.Context) error { e.gate.Set(ModeDryRun); return nil }
	if _, err := e.w.Apply(ctx, batch(categorize(txn2, catGroc)), store.ActorSystem, store.ViaScheduler); !errors.Is(err, ErrDryRun) {
		t.Errorf("flip during re-sync: err %v, want ErrDryRun", err)
	}
	e.resync = nil

	// Another plan.
	e.gate.Set(ModeAuto)
	b := batch(categorize(txn2, catGroc))
	b.PlanID = otherPlan
	_, err := e.w.Apply(ctx, b, store.ActorSystem, store.ViaScheduler)
	if !errors.Is(err, ErrWrongPlan) {
		t.Errorf("other plan: err %v, want ErrWrongPlan", err)
	}
	assertNoIDs(t, err.Error())

	if n := e.api.nCalls(); n != 1 {
		t.Errorf("%d PATCH calls, want 1 (the auto one)", n)
	}
	if ws := e.writes(txn2); len(ws) != 0 {
		t.Errorf("refused write logged %d rows", len(ws))
	}
}

func TestApply_BeforeAutoSinceRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.put(txn1, "2026-10-31", nil, false)
	outs, err := e.w.Apply(ctx, batch(categorize(txn1, catGroc)), store.ActorBrendan, store.ViaSlack)
	if !errors.Is(err, ErrBeforeAutoSince) {
		t.Fatalf("err %v, want ErrBeforeAutoSince", err)
	}
	if outs[0].Status != StatusSkipped || outs[0].Reason != ReasonBeforeAutoSince {
		t.Errorf("outcome %+v", outs[0])
	}

	// Judged from the re-read row: a re-sync that moves the date back refuses it.
	e.put(txn2, "2026-11-05", nil, false)
	e.resync = func(context.Context) error { e.put(txn2, "2026-10-01", nil, false); return nil }
	if _, err := e.w.Apply(ctx, batch(categorize(txn2, catGroc)), store.ActorSystem, store.ViaScheduler); !errors.Is(err, ErrBeforeAutoSince) {
		t.Errorf("re-read date: err %v, want ErrBeforeAutoSince", err)
	}
	e.resync = nil

	// No AutoSince configured refuses everything.
	e.put(txn3, "2026-11-05", nil, false)
	e.w.cfg.AutoSince = ""
	if _, err := e.w.Apply(ctx, batch(categorize(txn3, catGroc)), store.ActorSystem, store.ViaScheduler); !errors.Is(err, ErrBeforeAutoSince) {
		t.Errorf("no AutoSince: err %v, want ErrBeforeAutoSince", err)
	}
	if n := e.api.nCalls(); n != 0 {
		t.Errorf("%d PATCH calls, want 0", n)
	}
}

func TestApply_SendsOnlyChangedFields(t *testing.T) {
	e := newEnv(t)
	e.put(txn1, "2026-11-05", nil, false)          // both change
	e.put(txn2, "2026-11-05", ptr(catGroc), false) // already this category: approve only
	e.put(txn3, "2026-11-05", nil, true)           // already approved: category only
	outs, err := e.w.Apply(context.Background(), batch(
		categorize(txn1, catGroc),
		Write{TxnID: txn2, CategoryID: catGroc, Approve: true, DecidedCategoryID: ptr(catGroc)},
		categorize(txn3, catFun),
	), store.ActorSystem, store.ViaScheduler)
	if err != nil {
		t.Fatal(err)
	}
	ch := e.api.calls[0]
	if len(ch) != 3 {
		t.Fatalf("sent %d changes, want 3", len(ch))
	}
	check := func(c ynab.TransactionChange, wantCat *string, wantApproved bool) {
		t.Helper()
		if !sameID(c.CategoryID, wantCat) || (c.Approved != nil) != wantApproved {
			t.Errorf("change %+v: want category %v, approved set %v", c, wantCat, wantApproved)
		}
	}
	check(ch[0], ptr(catGroc), true)
	check(ch[1], nil, true)
	check(ch[2], ptr(catFun), false)
	for i, o := range outs {
		if o.Status != StatusDone {
			t.Errorf("outcome %d = %+v", i, o)
		}
	}

	// Nothing left to change: nothing sent.
	e.put(txn1, "2026-11-05", ptr(catGroc), true)
	outs, err = e.w.Apply(context.Background(), batch(Write{TxnID: txn1, CategoryID: catGroc, Approve: true, DecidedCategoryID: ptr(catGroc)}),
		store.ActorSystem, store.ViaScheduler)
	if err != nil || outs[0].Reason != ReasonNoChange || e.api.nCalls() != 1 {
		t.Errorf("no-op: %+v %v, %d calls", outs, err, e.api.nCalls())
	}
}

func TestApply_DropsRowsChangedDuringResync(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	for _, id := range []string{txn1, txn2, txn3} {
		e.put(id, "2026-11-05", nil, false)
	}
	e.resync = func(ctx context.Context) error {
		if err := e.db.Lock(ctx, store.HumanLock{PlanID: planID, TxnID: txn1, Field: store.LockFieldCategory, Source: store.LockYNABEdit}); err != nil {
			return err
		}
		e.put(txn2, "2026-11-05", ptr(catFun), false) // a person categorized it
		return nil
	}
	outs, err := e.w.Apply(ctx, batch(categorize(txn1, catGroc), categorize(txn2, catGroc), categorize(txn3, catGroc)),
		store.ActorSystem, store.ViaScheduler)
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Reason != ReasonLocked || outs[1].Reason != ReasonCategoryChanged || outs[2].Status != StatusDone {
		t.Errorf("outcomes %+v", outs)
	}
	if ch := e.api.calls[0]; len(ch) != 1 || ch[0].ID != txn3 {
		t.Errorf("sent %+v, want only txn3", ch)
	}
	if len(e.writes(txn1))+len(e.writes(txn2)) != 0 {
		t.Error("dropped rows were logged")
	}

	// Deleted, split, missing.
	e.resync = nil
	err = e.db.Update(ctx, func(tx *store.Tx) error {
		if err := tx.UpsertTransactions(planID, []store.Transaction{
			{ID: txn1, Date: "2026-11-05", Cleared: "cleared", AccountID: acct, Deleted: true},
		}, e.clock.Now()); err != nil {
			return err
		}
		return tx.UpsertSubtransactions(planID, []store.Subtransaction{{ID: "sub1", TransactionID: txn2, Amount: -1}})
	})
	if err != nil {
		t.Fatal(err)
	}
	e.put(txn2, "2026-11-05", nil, false)
	outs, err = e.w.Apply(ctx, batch(categorize(txn1, catGroc), categorize(txn2, catGroc), categorize("nope", catGroc)),
		store.ActorSystem, store.ViaScheduler)
	if err != nil {
		t.Fatal(err)
	}
	if outs[0].Reason != ReasonDeleted || outs[1].Reason != ReasonSplit || outs[2].Reason != ReasonMissing || e.api.nCalls() != 1 {
		t.Errorf("outcomes %+v, %d calls", outs, e.api.nCalls())
	}
}

func TestApply_IntentBeforePatchAndPartialFailure(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{txn1, txn2, txn3} {
		e.put(id, "2026-11-05", nil, false)
	}
	e.api.during = func(ch []ynab.TransactionChange) {
		open, err := e.db.UnfinishedWrites(context.Background())
		if err != nil {
			t.Error(err)
		}
		if len(open) != len(ch) {
			t.Errorf("during PATCH: %d intents, want %d", len(open), len(ch))
		}
		for _, o := range open {
			if string(o.BeforeJSON) != `{"approved":false,"category_id":null}` ||
				string(o.AfterJSON) != `{"category_id":"`+catGroc+`","approved":true}` {
				t.Errorf("intent before %s after %s", o.BeforeJSON, o.AfterJSON)
			}
		}
	}
	e.api.resp = func(ch []ynab.TransactionChange) []ynab.PatchedTransaction {
		r := echo(ch)
		r[1].CategoryID = ptr(catFun) // YNAB answered something else
		return r[:2]                  // and left txn3 out
	}
	outs, err := e.w.Apply(context.Background(), batch(categorize(txn1, catGroc), categorize(txn2, catGroc), categorize(txn3, catGroc)),
		store.ActorSystem, store.ViaScheduler)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		st     Status
		reason string
	}{{StatusDone, ""}, {StatusFailed, FailResponseDiffer}, {StatusFailed, FailNotInResponse}}
	for i, id := range []string{txn1, txn2, txn3} {
		if outs[i].Status != want[i].st || outs[i].Reason != want[i].reason {
			t.Errorf("outcome %d = %+v", i, outs[i])
		}
		if got := e.status(id); got != string(want[i].st) {
			t.Errorf("write_log %d = %s, want %s", i, got, want[i].st)
		}
	}
}

func TestApply_CrashAfterPatchThenReconcile(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.put(txn1, "2026-11-05", nil, false)
	e.put(txn2, "2026-11-05", nil, false)
	// The process dies once the PATCH has gone out, before any row is finished.
	e.api.during = func([]ynab.TransactionChange) { runtime.Goexit() }
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.w.Apply(ctx, batch(categorize(txn1, catGroc), categorize(txn2, catGroc)), store.ActorBrendan, store.ViaSlack)
	}()
	<-done
	e.api.during = nil
	if e.status(txn1) != store.WriteIntent || e.status(txn2) != store.WriteIntent {
		t.Fatal("rows finished before any sync")
	}

	// The next sync brings back YNAB's echo for txn1 only; txn2 never landed.
	syncStart := e.clock.Now()
	e.put(txn1, "2026-11-05", ptr(catGroc), true)
	if err := e.w.Reconcile(ctx, syncStart); err != nil {
		t.Fatal(err)
	}
	if e.status(txn1) != store.WriteDone {
		t.Errorf("echoed write = %s, want done", e.status(txn1))
	}
	if e.status(txn2) != store.WriteFailed {
		t.Errorf("unechoed write = %s, want failed", e.status(txn2))
	}
	if ws := e.writes(txn2); ws[0].Error == nil || *ws[0].Error != FailMirrorDiffer {
		t.Errorf("failed reason %v", ws[0].Error)
	}
	// The Slack answer that landed is now locked.
	locks, err := e.db.Locks(ctx, planID, []string{txn1, txn2})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks[txn1]) != 1 || locks[txn1][0].Source != store.LockSlack || len(locks[txn2]) != 0 {
		t.Errorf("locks %+v", locks)
	}
	if n := e.api.nCalls(); n != 1 {
		t.Errorf("%d PATCH calls, want 1: nothing is re-sent", n)
	}
}

func TestReconcile_SkipsIntentsAfterSyncStart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.put(txn1, "2026-11-05", nil, false)
	syncStart := e.clock.Now()
	e.api.during = func([]ynab.TransactionChange) { runtime.Goexit() }
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.w.Apply(ctx, batch(categorize(txn1, catGroc)), store.ActorSystem, store.ViaScheduler)
	}()
	<-done
	// A sync that started before the intent can't show its echo: not judged.
	if err := e.w.Reconcile(ctx, syncStart); err != nil {
		t.Fatal(err)
	}
	if got := e.status(txn1); got != store.WriteIntent {
		t.Errorf("status %s, want intent", got)
	}
}

func TestApply_PatchFailures(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		err     error
		status  Status
		logged  string
		unknown bool
	}{
		{"budget exhausted", ynab.ErrBudgetExhausted, StatusFailed, store.WriteFailed, false},
		{"rate limited", &ynab.RateLimitError{Op: "patch transactions"}, StatusFailed, store.WriteFailed, false},
		{"ambiguous", errors.New("patch transactions: connection reset"), StatusUnknown, store.WriteIntent, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.put(txn1, "2026-11-05", nil, false)
			e.api.err = tc.err
			outs, err := e.w.Apply(ctx, batch(categorize(txn1, catGroc)), store.ActorSystem, store.ViaScheduler)
			if err == nil || !errors.Is(err, tc.err) || errors.Is(err, ErrUnknownOutcome) != tc.unknown {
				t.Errorf("err %v", err)
			}
			assertNoIDs(t, err.Error())
			if outs[0].Status != tc.status || e.status(txn1) != tc.logged {
				t.Errorf("outcome %+v, logged %s", outs[0], e.status(txn1))
			}
			if ws := e.writes(txn1); ws[0].Error != nil {
				assertNoIDs(t, *ws[0].Error)
			}

			// Retried on a later Apply: its re-sync reconciles the old row
			// (failed, since YNAB never changed it) and sends again.
			e.api.err = nil
			outs, err = e.w.Apply(ctx, batch(categorize(txn1, catGroc)), store.ActorSystem, store.ViaScheduler)
			if err != nil || outs[0].Status != StatusDone {
				t.Errorf("retry: %+v %v", outs, err)
			}
			ws := e.writes(txn1)
			if len(ws) != 2 || ws[0].Status != store.WriteFailed || ws[1].Status != store.WriteDone {
				t.Errorf("write_log after retry %+v", ws)
			}
		})
	}
}

func TestApply_SlackLockAfterDone(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.put(txn1, "2026-11-05", nil, false)
	e.put(txn2, "2026-11-05", nil, false)
	if _, err := e.w.Apply(ctx, batch(categorize(txn1, catGroc)), store.ActorWife, store.ViaSlack); err != nil {
		t.Fatal(err)
	}
	if _, err := e.w.Apply(ctx, batch(categorize(txn2, catGroc)), store.ActorSystem, store.ViaScheduler); err != nil {
		t.Fatal(err)
	}
	locks, err := e.db.Locks(ctx, planID, []string{txn1, txn2})
	if err != nil {
		t.Fatal(err)
	}
	if len(locks[txn1]) != 1 || locks[txn1][0].Field != store.LockFieldCategory || locks[txn1][0].Source != store.LockSlack {
		t.Errorf("slack write locks %+v", locks[txn1])
	}
	if len(locks[txn2]) != 0 {
		t.Errorf("scheduler write locked %+v", locks[txn2])
	}
}

func TestApply_RejectsUnknownActor(t *testing.T) {
	e := newEnv(t)
	if _, err := e.w.Apply(context.Background(), batch(), "mallory", store.ViaSlack); err == nil {
		t.Error("unknown actor: want error")
	}
	if _, err := e.w.Apply(context.Background(), batch(), store.ActorSystem, "email"); err == nil {
		t.Error("unknown via: want error")
	}
	if e.syncs.Load() != 0 {
		t.Error("re-synced for a refused call")
	}
}

func TestApply_Serializes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	var inside, maxInside atomic.Int32
	enter := func() {
		n := inside.Add(1)
		for {
			m := maxInside.Load()
			if n <= m || maxInside.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inside.Add(-1)
	}
	e.resync = func(context.Context) error { enter(); return nil }
	e.api.during = func([]ynab.TransactionChange) { enter() }
	ids := make([]string, 8)
	for i := range ids {
		ids[i] = "txn-" + string(rune('a'+i))
		e.put(ids[i], "2026-11-05", nil, false)
	}
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := e.w.Apply(ctx, batch(categorize(ids[i], catGroc)), store.ActorSystem, store.ViaScheduler); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := e.w.Reconcile(ctx, e.clock.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m := maxInside.Load(); m != 1 {
		t.Errorf("%d calls overlapped, want 1", m)
	}
	if n := e.api.nCalls(); n != len(ids) {
		t.Errorf("%d PATCH calls, want %d", n, len(ids))
	}
}

func (e *env) loggedJSON(id string) (before, after string) {
	e.t.Helper()
	if err := e.db.Reader().QueryRow(`SELECT before_json, after_json FROM write_log WHERE txn_id = ?`, id).Scan(&before, &after); err != nil {
		e.t.Fatal(err)
	}
	return before, after
}

// write_log records both fields even when only one goes on the wire, so a later
// human edit to the other is still detected against after_json.
func TestApply_LogsWholeStateForSingleFieldWrites(t *testing.T) {
	e := newEnv(t)
	e.put(txn1, "2026-11-05", ptr(catGroc), false) // payee-memory guess we agree with: approve only
	e.put(txn2, "2026-11-05", ptr(catGroc), true)  // approved: recategorize only
	e.put(txn3, "2026-11-05", nil, false)          // categorize without approving
	_, err := e.w.Apply(context.Background(), batch(
		Write{TxnID: txn1, CategoryID: catGroc, Approve: true, DecidedCategoryID: ptr(catGroc)},
		Write{TxnID: txn2, CategoryID: catFun, Approve: true, DecidedCategoryID: ptr(catGroc)},
		Write{TxnID: txn3, CategoryID: catFun},
	), store.ActorSystem, store.ViaScheduler)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, before, after string }{
		{txn1, `{"approved":false,"category_id":"` + catGroc + `"}`, `{"category_id":"` + catGroc + `","approved":true}`},
		{txn2, `{"approved":true,"category_id":"` + catGroc + `"}`, `{"category_id":"` + catFun + `","approved":true}`},
		{txn3, `{"approved":false,"category_id":null}`, `{"category_id":"` + catFun + `","approved":false}`},
	} {
		before, after := e.loggedJSON(tc.id)
		if before != tc.before || after != tc.after {
			t.Errorf("logged before %s after %s, want %s %s", before, after, tc.before, tc.after)
		}
		if e.status(tc.id) != store.WriteDone {
			t.Errorf("status %s, want done", e.status(tc.id))
		}
	}
	ch := e.api.calls[0]
	if ch[0].CategoryID != nil || ch[1].Approved != nil || ch[2].Approved != nil {
		t.Errorf("wire carries unchanged fields: %+v", ch)
	}
}

// The slack lock is stored before the row is done: if it fails, the row stays
// an intent and Reconcile stores both later.
func TestApply_SlackLockBeforeDone(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.put(txn1, "2026-11-05", nil, false)
	if _, err := e.db.Writer().Exec(`CREATE TRIGGER no_locks BEFORE INSERT ON human_locks BEGIN SELECT RAISE(ABORT, 'locks down'); END`); err != nil {
		t.Fatal(err)
	}
	outs, err := e.w.Apply(ctx, batch(categorize(txn1, catGroc)), store.ActorBrendan, store.ViaSlack)
	if err == nil || outs[0].Status != StatusUnknown {
		t.Fatalf("lock failure: %+v %v", outs, err)
	}
	assertNoIDs(t, err.Error())
	if got := e.status(txn1); got != store.WriteIntent {
		t.Fatalf("status %s, want intent", got)
	}
	if _, err := e.db.Writer().Exec(`DROP TRIGGER no_locks`); err != nil {
		t.Fatal(err)
	}
	syncStart := e.clock.Now()
	e.put(txn1, "2026-11-05", ptr(catGroc), true) // the sync brings the echo
	if err := e.w.Reconcile(ctx, syncStart); err != nil {
		t.Fatal(err)
	}
	locks, err := e.db.Locks(ctx, planID, []string{txn1})
	if err != nil {
		t.Fatal(err)
	}
	if e.status(txn1) != store.WriteDone || len(locks[txn1]) != 1 || locks[txn1][0].Source != store.LockSlack {
		t.Errorf("after reconcile: status %s, locks %+v", e.status(txn1), locks)
	}
	if e.api.nCalls() != 1 {
		t.Errorf("%d PATCH calls, want 1", e.api.nCalls())
	}
}

func TestApply_SkipsEmptyIDs(t *testing.T) {
	e := newEnv(t)
	e.put(txn1, "2026-11-05", nil, false)
	outs, err := e.w.Apply(context.Background(), batch(categorize(txn1, ""), categorize("", catGroc)), store.ActorSystem, store.ViaScheduler)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range outs {
		if o.Status != StatusSkipped || o.Reason != ReasonInvalid {
			t.Errorf("outcome %+v", o)
		}
	}
	if e.api.nCalls() != 0 || len(e.writes(txn1)) != 0 {
		t.Error("an empty id was logged or sent")
	}
}
