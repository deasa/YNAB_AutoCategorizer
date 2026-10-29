package cmd

import (
	"context"
	"time"
)

// Ticker is the part of *time.Ticker the scheduler uses, so tests can drive
// it by hand.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTicker struct{ *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.Ticker.C }

func newRealTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

// Scheduler runs the sync job immediately and then every SyncEvery, and the
// import job every ImportEvery when Import is set. All jobs run on the
// scheduler's one goroutine, so no two ticks ever overlap; a tick that comes
// due while a job runs is dropped, not queued.
type Scheduler struct {
	Sync        func(ctx context.Context) error
	SyncEvery   time.Duration
	Import      func(ctx context.Context) error // nil: import disabled
	ImportEvery time.Duration

	// Health, if set, records each sync's outcome.
	Health *Health

	// NewTicker defaults to time.NewTicker (tests inject fakes).
	NewTicker func(time.Duration) Ticker
}

// Run blocks until ctx ends. The job in progress when ctx ends sees the
// cancellation through its own ctx.
func (s *Scheduler) Run(ctx context.Context) {
	newTicker := s.NewTicker
	if newTicker == nil {
		newTicker = newRealTicker
	}

	syncT := newTicker(s.SyncEvery)
	defer syncT.Stop()
	var importC <-chan time.Time // nil (never fires) when import is disabled
	if s.Import != nil {
		importT := newTicker(s.ImportEvery)
		defer importT.Stop()
		importC = importT.C()
	}

	s.runSync(ctx)
	drain(syncT.C())
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncT.C():
			s.runSync(ctx)
			drain(syncT.C())
		case <-importC:
			_ = s.Import(ctx) // the job logs its own outcome
			drain(importC)
		}
	}
}

func (s *Scheduler) runSync(ctx context.Context) {
	err := s.Sync(ctx)
	if s.Health == nil || ctx.Err() != nil {
		return
	}
	if err != nil {
		s.Health.Fail(err)
	} else {
		s.Health.OK()
	}
}

// drain drops a tick that came due while a job ran, so a slow job isn't
// followed straight away by another.
func drain(c <-chan time.Time) {
	select {
	case <-c:
	default:
	}
}
