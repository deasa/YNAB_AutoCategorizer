//go:build !unix

package llm

import "os/exec"

// setGroupKill falls back to exec.CommandContext's default (kill the child only).
func setGroupKill(*exec.Cmd) {}
