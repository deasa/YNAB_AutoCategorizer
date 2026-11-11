package llm

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Cmd is one child process to run. Env is the complete environment: nothing is
// inherited from the parent.
type Cmd struct {
	Path  string
	Args  []string // without argv[0]
	Env   []string
	Dir   string
	Stdin []byte
}

// Result is what a finished child produced.
type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Execer runs a child process to completion. Cancelling ctx must end the child.
// An error means the child could not be started or waited for (or ctx ended);
// a nonzero exit is a Result, not an error.
type Execer interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// maxOutput caps what is kept of each output stream.
const maxOutput = 4 << 20

// OSExec runs real processes. Each child gets its own process group, and
// cancelling ctx kills the whole group.
type OSExec struct{}

func (OSExec) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	if cmd.Env == nil {
		cmd.Env = []string{} // never inherit
	}
	cmd.Dir = c.Dir
	cmd.Stdin = bytes.NewReader(c.Stdin)
	var stdout, stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	setGroupKill(cmd)
	cmd.WaitDelay = 2 * time.Second

	err := cmd.Run()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	res := Result{Stdout: stdout.b.Bytes(), Stderr: stderr.b.Bytes()}
	var ee *exec.ExitError
	switch {
	case err == nil:
		return res, nil
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		return res, nil
	default:
		return Result{}, errors.New("could not run the claude binary")
	}
}

// capped keeps the first maxOutput bytes and discards the rest.
type capped struct{ b bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxOutput - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
