//go:build linux && amd64 && race

package onboarding

// Sandbox settings for the race build (see confineChild and seccompFilter). Test-only residual:
// the race runtime reserves terabytes of shadow memory, so there is no RLIMIT_AS, and it needs
// mprotect, so a hostile parse could move memory out of the RLIMIT_DATA count by making it
// read-only. No shipped binary is a race build.
const (
	pdfChildAllowMprotect = true
	pdfChildAllowMapStack = true
	pdfChildAllowClone    = true // thread masks only; else every clone is refused
	pdfChildASLimit       = false
)
