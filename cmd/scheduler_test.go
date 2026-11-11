package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	syncEvery   = 15 * time.Minute
	importEvery = 6 * time.Hour
	wait        = 2 * time.Second
	quiet       = 50 * time.Millisecond
)

// fakeTicker mimics time.Ticker: a one-slot channel and a send that drops
// the tick when the slot is full.
type fakeTicker struct {
	c       chan time.Time
	stopped atomic.Bool
}

func (f *fakeTicker) C() <-chan time.Time { return f.c }
func (f *fakeTicker) Stop()               { f.stopped.Store(true) }
func (f *fakeTicker) tick() {
	select {
	case f.c <- time.Time{}:
	default:
	}
}

type tickers struct {
	mu sync.Mutex
	by map[time.Duration]*fakeTicker
}

func (t *tickers) new(d time.Duration) Ticker {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.by == nil {
		t.by = map[time.Duration]*fakeTicker{}
	}
	f := &fakeTicker{c: make(chan time.Time, 1)}
	t.by[d] = f
	return f
}

func (t *tickers) get(d time.Duration) *fakeTicker {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.by[d]
}

// job counts calls, tracks concurrency across all jobs sharing inflight,
// and announces each start.
type job struct {
	calls    atomic.Int32
	started  chan struct{}
	inflight *atomic.Int32
	maxSeen  *atomic.Int32
	gate     chan struct{} // if set, each call blocks until it receives
	err      error
}

func newJob(inflight, maxSeen *atomic.Int32) *job {
	return &job{started: make(chan struct{}, 16), inflight: inflight, maxSeen: maxSeen}
}

func (j *job) run(ctx context.Context) error {
	n := j.inflight.Add(1)
	defer j.inflight.Add(-1)
	for {
		m := j.maxSeen.Load()
		if n <= m || j.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	j.calls.Add(1)
	j.started <- struct{}{}
	if j.gate != nil {
		select {
		case <-j.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return j.err
}

func expectStart(t *testing.T, j *job, what string) {
	t.Helper()
	select {
	case <-j.started:
	case <-time.After(wait):
		t.Fatalf("%s: job did not start", what)
	}
}

func expectNoStart(t *testing.T, j *job, what string) {
	t.Helper()
	select {
	case <-j.started:
		t.Fatalf("%s: job started unexpectedly", what)
	case <-time.After(quiet):
	}
}

func start(t *testing.T, s *Scheduler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(wait):
			t.Fatal("scheduler did not stop")
		}
	}
}

func TestScheduler_FirstSyncIsImmediate(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	sj := newJob(&inflight, &maxSeen)
	var tk tickers
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, NewTicker: tk.new})
	defer stop()

	expectStart(t, sj, "first tick without any ticker fire")
	expectNoStart(t, sj, "no second sync before a tick")
	tk.get(syncEvery).tick()
	expectStart(t, sj, "sync tick")
}

func TestScheduler_NoOverlappingTicks(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	sj := newJob(&inflight, &maxSeen)
	ij := newJob(&inflight, &maxSeen)
	sj.gate = make(chan struct{})
	var tk tickers
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, Import: ij.run, ImportEvery: importEvery,
		NewTicker: tk.new})
	defer stop()

	expectStart(t, sj, "first sync")
	// Ticks that come due while the first sync is still running.
	for range 3 {
		tk.get(syncEvery).tick()
	}
	tk.get(importEvery).tick()
	expectNoStart(t, sj, "sync tick during a running sync")
	expectNoStart(t, ij, "import tick during a running sync")

	sj.gate <- struct{}{}
	// The sync ticks that came due while it ran were dropped; the import
	// tick (a different job) still runs, but only after the sync finished.
	expectStart(t, ij, "import after the sync finished")
	expectNoStart(t, sj, "dropped sync ticks")

	tk.get(syncEvery).tick()
	expectStart(t, sj, "next sync tick")
	sj.gate <- struct{}{}

	if got := maxSeen.Load(); got != 1 {
		t.Errorf("max concurrent jobs = %d, want 1", got)
	}
	if got := sj.calls.Load(); got != 2 {
		t.Errorf("sync calls = %d, want 2", got)
	}
}

func TestScheduler_ImportDisabledNeverRuns(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	sj := newJob(&inflight, &maxSeen)
	var tk tickers
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, ImportEvery: importEvery, NewTicker: tk.new})
	expectStart(t, sj, "first sync")
	for range 3 {
		tk.get(syncEvery).tick()
		expectStart(t, sj, "sync tick")
	}
	stop()

	if tk.get(importEvery) != nil {
		t.Error("an import ticker was created while import is disabled")
	}
	if !tk.get(syncEvery).stopped.Load() {
		t.Error("sync ticker not stopped on shutdown")
	}
}

func TestScheduler_ImportRunsOnItsInterval(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	sj := newJob(&inflight, &maxSeen)
	ij := newJob(&inflight, &maxSeen)
	var tk tickers
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, Import: ij.run, ImportEvery: importEvery,
		NewTicker: tk.new})
	defer stop()

	expectStart(t, sj, "first sync")
	expectNoStart(t, ij, "import before its first interval")
	tk.get(syncEvery).tick()
	expectStart(t, sj, "sync tick")
	expectNoStart(t, ij, "import on a sync tick")

	tk.get(importEvery).tick()
	expectStart(t, ij, "import tick")
	expectNoStart(t, sj, "sync on an import tick")
	if got := ij.calls.Load(); got != 1 {
		t.Errorf("import calls = %d, want 1", got)
	}
}

func TestScheduler_ShutdownCancelsRunningTick(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	sj := newJob(&inflight, &maxSeen)
	sj.gate = make(chan struct{}) // never released: only ctx can end it
	var tk tickers
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, NewTicker: tk.new})
	expectStart(t, sj, "first sync")
	stop() // fails the test if Run doesn't return
	if inflight.Load() != 0 {
		t.Error("job still running after Run returned")
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func getHealth(t *testing.T, h *Health) (int, healthBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body healthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body %q: %v", rec.Body.String(), err)
	}
	return rec.Code, body
}

func TestHealthz_Transitions(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	h := &Health{StaleAfter: 2 * syncEvery, Now: clk.now}
	var inflight, maxSeen atomic.Int32
	sj := newJob(&inflight, &maxSeen)
	sj.gate = make(chan struct{})
	var tk tickers
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, Health: h, NewTicker: tk.new})
	defer stop()

	// Before any sync has finished.
	expectStart(t, sj, "first sync")
	if code, body := getHealth(t, h); code != http.StatusServiceUnavailable || body.LastSyncOK != "" {
		t.Fatalf("before first sync: %d %+v, want 503 with no last_sync_ok", code, body)
	}

	sj.gate <- struct{}{}
	waitFor(t, func() bool { code, _ := getHealth(t, h); return code == http.StatusOK })
	code, body := getHealth(t, h)
	if body.LastSyncOK != "2026-10-29T12:00:00Z" || body.LastError != "" {
		t.Fatalf("after first sync: %d %+v", code, body)
	}

	// A failed tick records the error, but the last success is still fresh.
	clk.add(syncEvery)
	sj.err = errors.New("plan delta 0f8fad5b-d9cb-469f-a165-70867728950e: boom")
	tk.get(syncEvery).tick()
	expectStart(t, sj, "failing tick")
	sj.gate <- struct{}{}
	waitFor(t, func() bool { _, b := getHealth(t, h); return b.LastError != "" })
	code, body = getHealth(t, h)
	if code != http.StatusOK || body.LastSyncOK != "2026-10-29T12:00:00Z" {
		t.Fatalf("after one failure: %d %+v, want 200 with the old last_sync_ok", code, body)
	}
	if strings.Contains(body.LastError, "0f8fad5b") || !strings.Contains(body.LastError, "boom") {
		t.Errorf("last_error not redacted: %q", body.LastError)
	}

	// No success for more than 2×SYNC_INTERVAL: stale.
	clk.add(syncEvery + time.Second)
	if code, _ := getHealth(t, h); code != http.StatusServiceUnavailable {
		t.Fatalf("stale: got %d, want 503", code)
	}

	// Recovery.
	sj.err = nil
	tk.get(syncEvery).tick()
	expectStart(t, sj, "recovering tick")
	sj.gate <- struct{}{}
	waitFor(t, func() bool { code, _ := getHealth(t, h); return code == http.StatusOK })
	if _, body := getHealth(t, h); body.LastError != "" {
		t.Errorf("last_error not cleared on success: %q", body.LastError)
	}
}

func TestHealthz_SkippedTickIsNotFresh(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	h := &Health{StaleAfter: 2 * syncEvery, Now: clk.now}
	h.Fail(errSkipped)
	if code, body := getHealth(t, h); code != http.StatusServiceUnavailable || body.LastError == "" {
		t.Fatalf("skipped first tick: %d %+v, want 503 with last_error", code, body)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestScheduler_BackupFiresOnDelayAndRearms(t *testing.T) {
	const firstDelay, secondDelay = 2 * time.Hour, 24 * time.Hour
	var inflight, maxSeen atomic.Int32
	syncJob, backupJob := newJob(&inflight, &maxSeen), newJob(&inflight, &maxSeen)
	var delays atomic.Int32
	tk := &tickers{}
	stop := start(t, &Scheduler{
		Sync: syncJob.run, SyncEvery: syncEvery,
		Backup: backupJob.run,
		BackupDelay: func() time.Duration {
			if delays.Add(1) == 1 {
				return firstDelay
			}
			return secondDelay
		},
		NewTicker: tk.new,
	})
	defer stop()
	expectStart(t, syncJob, "startup sync")
	expectNoStart(t, backupJob, "before the delay")

	first := tk.get(firstDelay)
	first.tick()
	expectStart(t, backupJob, "first backup")
	deadline := time.Now().Add(wait)
	for tk.get(secondDelay) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	second := tk.get(secondDelay)
	if second == nil {
		t.Fatal("no ticker re-armed for the next backup")
	}
	if !first.stopped.Load() {
		t.Error("first backup ticker not stopped")
	}
	second.tick()
	expectStart(t, backupJob, "second backup")
}

func TestScheduler_BackupDueDuringSyncRunsAfterIt(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	syncJob, backupJob := newJob(&inflight, &maxSeen), newJob(&inflight, &maxSeen)
	syncJob.gate = make(chan struct{})
	tk := &tickers{}
	stop := start(t, &Scheduler{
		Sync: syncJob.run, SyncEvery: syncEvery,
		Backup: backupJob.run, BackupDelay: func() time.Duration { return time.Hour },
		NewTicker: tk.new,
	})
	defer stop()
	expectStart(t, syncJob, "startup sync")
	tk.get(time.Hour).tick()
	expectNoStart(t, backupJob, "while sync runs")
	close(syncJob.gate)
	expectStart(t, backupJob, "backup after sync")
	if maxSeen.Load() != 1 {
		t.Errorf("max concurrent jobs = %d, want 1", maxSeen.Load())
	}
}

func TestNextDaily(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	for _, tc := range []struct {
		name      string
		now, want time.Time
	}{
		{"later today", time.Date(2026, 6, 1, 1, 0, 0, 0, ny), time.Date(2026, 6, 1, 3, 17, 0, 0, ny)},
		{"exactly now rolls over", time.Date(2026, 6, 1, 3, 17, 0, 0, ny), time.Date(2026, 6, 2, 3, 17, 0, 0, ny)},
		{"already past", time.Date(2026, 6, 1, 12, 0, 0, 0, ny), time.Date(2026, 6, 2, 3, 17, 0, 0, ny)},
		{"month end", time.Date(2026, 6, 30, 12, 0, 0, 0, ny), time.Date(2026, 7, 1, 3, 17, 0, 0, ny)},
		// 2026-11-01 is the fall-back day: wall-clock time stays 03:17 across a 25-hour day.
		{"across DST", time.Date(2026, 10, 31, 12, 0, 0, 0, ny), time.Date(2026, 11, 1, 3, 17, 0, 0, ny)},
	} {
		got := nextDaily(tc.now, 3, 17)
		if !got.Equal(tc.want) {
			t.Errorf("%s: nextDaily(%v) = %v, want %v", tc.name, tc.now, got, tc.want)
		}
		if !got.After(tc.now) {
			t.Errorf("%s: %v not after now", tc.name, got)
		}
	}
}

func TestScheduler_PruneRunsAtStartupAndOnItsTick(t *testing.T) {
	const pruneEvery = 24 * time.Hour
	var inflight, maxSeen atomic.Int32
	sj, pj := newJob(&inflight, &maxSeen), newJob(&inflight, &maxSeen)
	tk := &tickers{}
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, Prune: pj.run, PruneEvery: pruneEvery, NewTicker: tk.new})
	defer stop()
	expectStart(t, pj, "pj")
	expectStart(t, sj, "sj")
	tk.get(pruneEvery).tick()
	expectStart(t, pj, "pj")
	if pj.calls.Load() != 2 || sj.calls.Load() != 1 || maxSeen.Load() != 1 {
		t.Errorf("prune %d, sync %d, overlap %d", pj.calls.Load(), sj.calls.Load(), maxSeen.Load())
	}
}

func TestScheduler_PruneErrorLeavesSyncHealthy(t *testing.T) {
	const pruneEvery = 24 * time.Hour
	var inflight, maxSeen atomic.Int32
	sj, pj := newJob(&inflight, &maxSeen), newJob(&inflight, &maxSeen)
	pj.err = errors.New("prune boom")
	h := &Health{StaleAfter: time.Hour}
	tk := &tickers{}
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, Prune: pj.run, PruneEvery: pruneEvery, Health: h, NewTicker: tk.new})
	defer stop()
	expectStart(t, pj, "pj")
	expectStart(t, sj, "sj")
	waitHealthOK(t, h)
	// A later prune starting means the earlier one, and whatever followed it, is over.
	for deadline := time.Now().Add(5 * time.Second); pj.calls.Load() < 3 && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		tk.get(pruneEvery).tick()
		select {
		case <-pj.started:
		default:
		}
	}
	if pj.calls.Load() < 3 {
		t.Fatal("prune didn't run on its tick")
	}
	if code, body := getHealth(t, h); code != 200 || body.LastError != "" {
		t.Errorf("after a prune error: %d, last_error %q", code, body.LastError)
	}
}

func waitHealthOK(t *testing.T, h *Health) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if code, _ := getHealth(t, h); code == 200 {
			return
		}
	}
	t.Fatal("health never went OK")
}

func TestScheduler_ImportRestartMidIntervalFiresAtRemainingDelay(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	sj, ij := newJob(&inflight, &maxSeen), newJob(&inflight, &maxSeen)
	var tk tickers
	remaining := 2 * time.Hour
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, Import: ij.run, ImportEvery: importEvery,
		ImportDelay: func() time.Duration { return remaining }, NewTicker: tk.new})
	defer stop()

	expectStart(t, sj, "first sync")
	expectNoStart(t, ij, "import before the remaining delay")
	waitFor(t, func() bool { return tk.get(remaining) != nil })
	if tk.get(importEvery) != nil {
		t.Fatal("full-interval import ticker armed before the first import")
	}
	first := tk.get(remaining)
	first.tick()
	expectStart(t, ij, "import at the remaining delay")
	// Then every ImportEvery, and the one-shot timer is gone.
	waitFor(t, func() bool { return tk.get(importEvery) != nil })
	if !first.stopped.Load() {
		t.Error("first-import timer not stopped")
	}
	tk.get(importEvery).tick()
	expectStart(t, ij, "import on its interval")
}

func TestScheduler_ImportOverdueFiresPromptly(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	sj, ij := newJob(&inflight, &maxSeen), newJob(&inflight, &maxSeen)
	var tk tickers
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, Import: ij.run, ImportEvery: importEvery,
		ImportDelay: func() time.Duration { return 0 }, NewTicker: tk.new})
	defer stop()

	expectStart(t, sj, "first sync")
	expectStart(t, ij, "overdue import right after the first sync")
	expectNoStart(t, ij, "a second import")
	tk.get(importEvery).tick()
	expectStart(t, ij, "import on its interval")
}

// Restarts every 10 minutes for two intervals: the import job records its
// attempt time, and each start computes its delay from it, as serve does.
func TestScheduler_RepeatedRestartsImportOncePerInterval(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)}
	var last time.Time // import_state.last_attempt_at
	var imports []time.Time
	importJob := func(context.Context) error {
		last = clk.now()
		imports = append(imports, last)
		return errors.New("import failed") // a failure counts too
	}
	delay := func() time.Duration {
		if last.IsZero() {
			return 0 // first ever start: run soon, so the test sees one
		}
		return firstImportDelay(last, clk.now(), importEvery)
	}

	end := clk.now().Add(2 * importEvery)
	for clk.now().Before(end) {
		var tk tickers
		var inflight, maxSeen atomic.Int32
		sj := newJob(&inflight, &maxSeen)
		d := delay()
		stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, Import: importJob, ImportEvery: importEvery,
			ImportDelay: func() time.Duration { return d }, NewTicker: tk.new})
		expectStart(t, sj, "first sync")
		// The first-import timer fires if its delay elapses before the next
		// restart, 10 minutes on.
		if d > 0 && d <= 10*time.Minute {
			clk.add(d)
			waitFor(t, func() bool { return tk.get(d) != nil })
			tk.get(d).tick()
			waitFor(t, func() bool { return tk.get(importEvery) != nil })
			stop()
			clk.add(10*time.Minute - d)
			continue
		}
		time.Sleep(quiet)
		stop()
		clk.add(10 * time.Minute)
	}

	// One at the first start, then one per interval: at 0, 6h and 12h.
	if len(imports) != 3 {
		t.Fatalf("imports over two intervals of restarts = %d, want 3 (%v)", len(imports), imports)
	}
	for i := 1; i < len(imports); i++ {
		if gap := imports[i].Sub(imports[i-1]); gap != importEvery {
			t.Errorf("imports %d and %d are %v apart, want %v", i-1, i, gap, importEvery)
		}
	}
}

func TestScheduler_ImportDisabledIgnoresDelay(t *testing.T) {
	var inflight, maxSeen atomic.Int32
	sj := newJob(&inflight, &maxSeen)
	var tk tickers
	var asked atomic.Bool
	stop := start(t, &Scheduler{Sync: sj.run, SyncEvery: syncEvery, ImportEvery: importEvery,
		ImportDelay: func() time.Duration { asked.Store(true); return 0 }, NewTicker: tk.new})
	expectStart(t, sj, "first sync")
	time.Sleep(quiet)
	stop()
	if asked.Load() {
		t.Error("ImportDelay read while import is disabled")
	}
	if len(tk.by) != 1 {
		t.Errorf("tickers = %d, want only the sync ticker", len(tk.by))
	}
}
