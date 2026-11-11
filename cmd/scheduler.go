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
// import job every ImportEvery when Import is set, the first one after
// ImportDelay. All jobs run on the
// scheduler's one goroutine, so no two ticks ever overlap. A job's own tick that
// comes due while it runs is dropped, not queued; another job's tick that comes
// due meanwhile runs right after. The backup job, when set, runs once per
// BackupDelay. The prune job, when set, runs at startup and then every
// PruneEvery; its outcome never touches Health.
type Scheduler struct {
	Sync        func(ctx context.Context) error
	SyncEvery   time.Duration
	Import      func(ctx context.Context) error // nil: import disabled
	ImportEvery time.Duration
	// ImportDelay, if set, is the time until the first import; zero or less
	// runs it right after the first sync. Nil waits a full ImportEvery.
	ImportDelay func() time.Duration
	Backup      func(ctx context.Context) error // nil: backups disabled
	BackupDelay func() time.Duration            // time until the next backup; must be positive
	Prune       func(ctx context.Context) error // nil: MCP disabled, nothing to prune
	PruneEvery  time.Duration
	Probe       func(ctx context.Context) error // nil: no claude, nothing to probe
	ProbeEvery  time.Duration

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
	var importT Ticker
	var importC <-chan time.Time // nil (never fires) when import is disabled
	importFirst := false         // importT is the one-shot first-import timer
	importNow := false           // the first import is overdue: run it after the first sync
	armImport := func(d time.Duration) {
		if importT != nil {
			importT.Stop()
		}
		importT = newTicker(d)
		importC = importT.C()
	}
	if s.Import != nil {
		d := s.ImportEvery
		if s.ImportDelay != nil {
			d = s.ImportDelay()
		}
		switch {
		case d <= 0:
			armImport(s.ImportEvery)
			importNow = true
		case d != s.ImportEvery:
			armImport(d)
			importFirst = true
		default:
			armImport(s.ImportEvery)
		}
		defer func() { importT.Stop() }()
	}
	runImport := func() {
		_ = s.Import(ctx) // the job logs its own outcome
		if importFirst {
			importFirst = false
			armImport(s.ImportEvery)
		}
		drain(importC)
	}
	var backupT Ticker
	var backupC <-chan time.Time // nil (never fires) when backups are disabled
	armBackup := func() {
		if backupT != nil {
			backupT.Stop()
		}
		backupT = newTicker(s.BackupDelay())
		backupC = backupT.C()
	}
	if s.Backup != nil {
		armBackup()
		defer func() { backupT.Stop() }()
	}

	var pruneC <-chan time.Time // nil (never fires) when there's nothing to prune
	if s.Prune != nil {
		pruneT := newTicker(s.PruneEvery)
		defer pruneT.Stop()
		pruneC = pruneT.C()
		_ = s.Prune(ctx) // the job logs its own outcome
		drain(pruneC)
	}

	var probeC <-chan time.Time // nil (never fires) when there's nothing to probe
	if s.Probe != nil {
		probeT := newTicker(s.ProbeEvery)
		defer probeT.Stop()
		probeC = probeT.C()
	}

	s.runSync(ctx)
	drain(syncT.C())
	if s.Probe != nil && ctx.Err() == nil {
		_ = s.Probe(ctx) // the job logs and alerts its own outcome
		drain(probeC)
	}
	if importNow && ctx.Err() == nil {
		runImport()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-syncT.C():
			s.runSync(ctx)
			drain(syncT.C())
		case <-importC:
			runImport()
		case <-backupC:
			_ = s.Backup(ctx) // the job logs its own outcome
			armBackup()
		case <-pruneC:
			_ = s.Prune(ctx) // the job logs its own outcome
			drain(pruneC)
		case <-probeC:
			_ = s.Probe(ctx) // the job logs and alerts its own outcome
			drain(probeC)
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

// nextDaily returns the first hour:minute local time strictly after now.
func nextDaily(now time.Time, hour, minute int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year(), now.Month(), now.Day()+1, hour, minute, 0, 0, now.Location())
	}
	return next
}
