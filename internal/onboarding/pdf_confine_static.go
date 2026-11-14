//go:build linux && amd64 && !cgo

package onboarding

// Sandbox settings for the static (CGO_ENABLED=0) build, the production image's: no mprotect, no
// MAP_STACK, and RLIMIT_AS (see confineChild and seccompFilter).
const (
	pdfChildAllowMprotect = false
	pdfChildAllowMapStack = false
	pdfChildAllowClone    = false // thread masks only; else every clone is refused
	pdfChildASLimit       = true
)
