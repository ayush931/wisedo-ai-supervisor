//go:build unix

package onboarding

import (
	"os/exec"
	"syscall"
)

// killedForCPU reports a child ended by its CPU-time limit (SIGXCPU, then SIGKILL at the hard
// limit): the file needed more than the parse budget.
func killedForCPU(exit *exec.ExitError) bool {
	ws, ok := exit.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && (ws.Signal() == syscall.SIGXCPU || ws.Signal() == syscall.SIGKILL)
}

// killedBySandbox reports a child killed by its seccomp filter (SIGSYS from SECCOMP_RET_KILL).
func killedBySandbox(exit *exec.ExitError) bool {
	ws, ok := exit.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGSYS
}
