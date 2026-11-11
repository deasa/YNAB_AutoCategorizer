package writer

import "sync/atomic"

// Mode is the effective categorization mode. Only ModeAuto lets the writer
// send anything.
type Mode string

const (
	ModeOff    Mode = "off"
	ModeDryRun Mode = "dry_run"
	ModeAuto   Mode = "auto"
)

// Gate holds the effective mode the writer checks before every write. It
// starts at dry_run (the zero value is dry_run too) and is set on every
// pipeline run, never fixed at startup. Safe for concurrent use.
type Gate struct {
	mode atomic.Pointer[Mode]
}

// NewGate returns a Gate in dry_run.
func NewGate() *Gate { return &Gate{} }

// Set publishes the effective mode.
func (g *Gate) Set(m Mode) { g.mode.Store(&m) }

// Mode returns the effective mode: dry_run until Set says otherwise.
func (g *Gate) Mode() Mode {
	if p := g.mode.Load(); p != nil {
		return *p
	}
	return ModeDryRun
}
