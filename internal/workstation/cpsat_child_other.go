//go:build !linux

package workstation

// addressSpaceLimit: the solver ships for Linux only; other platforms (development) do not
// enforce RLIMIT_AS reliably, so only the portable limits apply there.
func addressSpaceLimit() []rlimit { return nil }
