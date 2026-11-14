//go:build !(linux && amd64)

package onboarding

import "errors"

// PDFSandboxSupported reports a platform where the PDF child can confine itself; elsewhere every
// PDF is EXTRACTION_UNAVAILABLE and the PDF tests report BLOCKED.
const PDFSandboxSupported = false

// confineChild has no sandbox for this platform, so the PDF child refuses to start and every PDF
// is EXTRACTION_UNAVAILABLE (rule 08: a parse without the network and filesystem boundary never
// runs). The supported target is linux/amd64 (pdf_confine_linux.go).
func confineChild() error { return errors.New("no PDF parser sandbox on this platform") }
