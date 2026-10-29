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
