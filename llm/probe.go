package llm

import (
	"context"
	"strings"
)

// ProbePrompt is sent on stdin. A bare "ok" made Haiku chat in M0-4b.
const ProbePrompt = "Reply with exactly: ok"

// probeArgs has no --json-schema, so --max-turns 1 is allowed here.
var probeArgs = append(append([]string{}, baseFlags...), "--max-turns", "1")

// Probe checks that the OAuth token still works. It passes only if rc==0,
// !is_error and the trimmed, lowercased result is "ok". On failure the error
// is a *CallError whose APIErrorStatus carries the CLI's api_error_status
// (e.g. 401 for a bad token), for the Slack alert.
func (r *Runner) Probe(ctx context.Context) error {
	return r.call(ctx, probeArgs, ProbePrompt, func(o output) error {
		if strings.ToLower(strings.TrimSpace(o.Result)) != "ok" {
			return &CallError{Reason: "probe reply was not ok"}
		}
		return nil
	})
}
