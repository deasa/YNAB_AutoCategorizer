// Package llm runs the pinned headless `claude` CLI (architecture §5a) for the
// classifier second opinion and the daily auth probe.
//
// Every call uses an absolute binary path, `--model haiku`, the prompt on stdin
// (never argv), an empty dedicated cwd, and an environment built from an
// allowlist, so ANTHROPIC_API_KEY (which would outrank the OAuth token) never
// reaches the child. Errors carry only fixed text, exit codes and API status
// codes: never the token, the prompt or the model's output.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// PinnedVersion is the claude CLI version finbot is built and tested against.
const PinnedVersion = "2.1.312"

// ModelID names this model in Predictions and `decisions`.
const ModelID = "claude-cli-" + PinnedVersion + "/haiku"

// Config sets up a Runner.
type Config struct {
	Binary  string // absolute path to the pinned claude binary
	Token   string // CLAUDE_CODE_OAUTH_TOKEN
	WorkDir string // empty dedicated directory used as cwd; created 0700 if missing

	// Getenv reads the parent environment for HOME, PATH and CLAUDE_CONFIG_DIR.
	// Nil means os.Getenv. Nothing else is read from it.
	Getenv func(string) string

	Exec    Execer        // nil means OSExec
	Timeout time.Duration // per attempt; 0 means DefaultTimeout
	// RetryDelay is the pause before the single retry; 0 means none.
	RetryDelay time.Duration
}

// DefaultTimeout bounds one attempt. M0 measured ~5 s per batch of 10.
const DefaultTimeout = 90 * time.Second

// Runner runs claude calls one at a time (a single-worker queue).
type Runner struct {
	cfg  Config
	env  []string
	slot chan struct{}
}

// passthrough are the only parent variables the child may see.
var passthrough = []string{"HOME", "PATH", "CLAUDE_CONFIG_DIR"}

// New validates cfg and prepares the work directory.
func New(cfg Config) (*Runner, error) {
	if !filepath.IsAbs(cfg.Binary) {
		return nil, errors.New("llm: claude binary path must be absolute")
	}
	if cfg.Token == "" {
		return nil, errors.New("llm: empty CLAUDE_CODE_OAUTH_TOKEN")
	}
	if !filepath.IsAbs(cfg.WorkDir) {
		return nil, errors.New("llm: work directory must be an absolute path")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		return nil, errors.New("llm: cannot create the work directory")
	}
	if err := checkEmpty(cfg.WorkDir); err != nil {
		return nil, err
	}
	if cfg.Getenv == nil {
		cfg.Getenv = os.Getenv
	}
	if cfg.Exec == nil {
		cfg.Exec = OSExec{}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	return &Runner{cfg: cfg, env: buildEnv(cfg), slot: make(chan struct{}, 1)}, nil
}

func buildEnv(cfg Config) []string {
	env := []string{
		"DISABLE_UPDATES=1",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
		"CLAUDE_CODE_OAUTH_TOKEN=" + cfg.Token,
	}
	for _, k := range passthrough {
		if v := cfg.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func checkEmpty(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return errors.New("llm: cannot read the work directory")
	}
	if len(ents) > 0 {
		return errors.New("llm: work directory is not empty")
	}
	return nil
}

// baseFlags are shared by both call sites, after `-p --model haiku`.
var baseFlags = []string{
	"-p", "--model", "haiku", "--output-format", "json",
	"--tools", "", "--strict-mcp-config", "--setting-sources", "",
	"--disable-slash-commands", "--no-session-persistence",
}

// output is the part of `--output-format json` finbot reads.
type output struct {
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	APIErrorStatus   int             `json:"api_error_status"`
}

// CallError is a failed claude call. Its message never includes the prompt,
// the token or the model's text.
type CallError struct {
	Reason         string // fixed text
	ExitCode       int
	APIErrorStatus int // from the CLI's JSON, 0 if absent
}

func (e *CallError) Error() string {
	s := "llm: " + e.Reason
	if e.ExitCode != 0 {
		s += fmt.Sprintf(" (rc %d)", e.ExitCode)
	}
	if e.APIErrorStatus != 0 {
		s += fmt.Sprintf(" (api status %d)", e.APIErrorStatus)
	}
	return s
}

// call runs claude with args and prompt on stdin, holding the single worker
// slot, with a per-attempt timeout and one retry. check validates a decoded
// output; its error triggers the retry like any other failure.
func (r *Runner) call(ctx context.Context, args []string, prompt string, check func(output) error) error {
	select {
	case r.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.slot }()

	var err error
	for attempt := range 2 {
		if attempt > 0 && r.cfg.RetryDelay > 0 {
			t := time.NewTimer(r.cfg.RetryDelay)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			}
		}
		err = r.attempt(ctx, args, prompt, check)
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (r *Runner) attempt(ctx context.Context, args []string, prompt string, check func(output) error) error {
	if err := checkEmpty(r.cfg.WorkDir); err != nil {
		return err
	}
	actx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	res, err := r.cfg.Exec.Run(actx, Cmd{
		Path:  r.cfg.Binary,
		Args:  args,
		Env:   r.env,
		Dir:   r.cfg.WorkDir,
		Stdin: []byte(prompt),
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if actx.Err() != nil {
			return &CallError{Reason: "timed out"}
		}
		return &CallError{Reason: "could not run claude"}
	}
	var out output
	jerr := json.Unmarshal(res.Stdout, &out)
	switch {
	case res.ExitCode != 0:
		return &CallError{Reason: "claude exited nonzero", ExitCode: res.ExitCode, APIErrorStatus: out.APIErrorStatus}
	case jerr != nil:
		return &CallError{Reason: "malformed claude output"}
	case out.IsError:
		return &CallError{Reason: "claude reported an error", APIErrorStatus: out.APIErrorStatus}
	}
	return check(out)
}
