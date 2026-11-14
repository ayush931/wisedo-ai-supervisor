//go:build linux && amd64 && cgo && !race

package onboarding

// Sandbox settings for the cgo build (see confineChild and seccompFilter): glibc's
// pthread_create needs mprotect and MAP_STACK; RLIMIT_AS still bounds every mapping.
const (
	pdfChildAllowMprotect = true
	pdfChildAllowMapStack = true
	pdfChildAllowClone    = true // thread masks only; else every clone is refused
	pdfChildASLimit       = true
)
