package workstation

import "golang.org/x/sys/unix"

// addressSpaceLimit caps the solver's address space on Linux, where the solver ships.
func addressSpaceLimit() []rlimit {
	return []rlimit{{"RLIMIT_AS", unix.RLIMIT_AS, cpsatAddressSpace}}
}
