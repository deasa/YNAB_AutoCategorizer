//go:build unix

package llm

import "syscall"

var syscallZero = syscall.Signal(0)
