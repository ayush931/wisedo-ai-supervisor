package workstation

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// CPSATChildFlag makes the supervisor binary act as the solver's confinement step (WIS-699): it
// sets resource limits on itself and then executes the solver in place, so the solver inherits the
// limits and the cleared environment the parent gave it. Rule 01 bounds the solver to a cleared
// environment and a short life; before this it had no resource limits at all.
const CPSATChildFlag = "__supervisor-workstation-cpsat-child"

// cpsatChildExitUnconfined is the exit code when a limit cannot be set: the solver is never run
// unconfined, and the parent reports the solver unavailable (manual planning is unaffected).
const cpsatChildExitUnconfined = 70

// Solver resource limits. The call's wall deadline (cpsatLimit, 6 s) still ends it first; these
// are backstops for a solver that misbehaves inside its deadline or outlives a lost parent.
const (
	cpsatCPUSeconds   = 8       // CPU time, above the 6 s wall deadline
	cpsatOpenFiles    = 64      // descriptors: stdin, stdout, stderr and the libraries
	cpsatAddressSpace = 4 << 30 // bytes of address space (Linux), well above a one-worker solve
)

// init runs the confinement step in any binary that links this package (the supervisor binary
// and every test binary that can call PlanCPsat), before main and before any configuration is
// read. Without it, a test binary re-executed with CPSATChildFlag would run its tests again.
func init() { RunCPSATChildIfRequested() }

// RunCPSATChildIfRequested runs the confinement step and never returns when argv[1] is
// CPSATChildFlag; otherwise it returns at once.
func RunCPSATChildIfRequested() {
	if len(os.Args) < 2 || os.Args[1] != CPSATChildFlag {
		return
	}
	os.Exit(runCPSATChild(os.Args[2:], os.Environ(), syscall.Exec))
}

// runCPSATChild confines the process, then executes args[0] (an absolute solver path) with the
// remaining arguments and env. exec only returns on failure.
func runCPSATChild(args, env []string, exec func(string, []string, []string) error) int {
	if len(args) == 0 || !filepath.IsAbs(args[0]) {
		fmt.Fprintln(os.Stderr, "cpsat child: an absolute solver path is required")
		return cpsatChildExitUnconfined
	}
	if err := confineCPSAT(); err != nil {
		fmt.Fprintln(os.Stderr, "cpsat child: cannot confine the solver:", err)
		return cpsatChildExitUnconfined
	}
	if err := exec(args[0], args, env); err != nil {
		fmt.Fprintln(os.Stderr, "cpsat child: cannot start the solver:", err)
		return cpsatChildExitUnconfined
	}
	return 0
}

// cpsatLimits are the limits every platform supports; addressSpaceLimit adds Linux's.
func cpsatLimits() []rlimit {
	return append([]rlimit{
		{"RLIMIT_CPU", unix.RLIMIT_CPU, cpsatCPUSeconds},
		{"RLIMIT_CORE", unix.RLIMIT_CORE, 0},   // a crash never writes request data to disk
		{"RLIMIT_FSIZE", unix.RLIMIT_FSIZE, 0}, // the solver writes no file; stdout is a pipe
		{"RLIMIT_NOFILE", unix.RLIMIT_NOFILE, cpsatOpenFiles},
	}, addressSpaceLimit()...)
}

type rlimit struct {
	name     string
	resource int
	value    uint64
}

func confineCPSAT() error {
	for _, l := range cpsatLimits() {
		if err := unix.Setrlimit(l.resource, &unix.Rlimit{Cur: l.value, Max: l.value}); err != nil {
			return fmt.Errorf("%s: %w", l.name, err)
		}
	}
	return nil
}
