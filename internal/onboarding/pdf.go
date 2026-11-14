package onboarding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/strictjson"
)

// PDF refusals (WIS-624). Each is decided by the file, so each is a 4xx with its own code: a client
// that retries a 5xx would otherwise resend a file that can never pass. Only a busy or missing
// parser is a 503.
var (
	ErrPDFEncrypted         = errors.New("PDF_ENCRYPTED")
	ErrPDFActiveContent     = errors.New("PDF_ACTIVE_CONTENT")
	ErrPDFExternalReference = errors.New("PDF_EXTERNAL_REFERENCE")
	ErrPDFNoTextLayer       = errors.New("PDF_NO_TEXT_LAYER")
	ErrPDFTooManyPages      = errors.New("PDF_TOO_MANY_PAGES")
	ErrPDFParseBudget       = errors.New("PDF_PARSE_BUDGET_EXCEEDED")
	ErrPDFTextUnreadable    = errors.New("PDF_TEXT_UNREADABLE")
	// ErrPDFTooManyImages: a typed PDF draws more figures than one upload sends to the vision model.
	ErrPDFTooManyImages = errors.New("PDF_TOO_MANY_IMAGES")
	// ErrExtractionBusy: every PDF child slot stayed taken; retrying the same request is safe.
	ErrExtractionBusy = errors.New("EXTRACTION_BUSY")
)

const (
	pdfSlotWait   = 2 * time.Second
	pdfChildSlots = 2
)

// pdfChildWallTime bounds one child, inside the API server's 30 s write timeout. A variable only so
// the decompression-bomb test can show the memory watchdog, not the clock, ends a bomb on a
// CPU-starved test host.
var pdfChildWallTime = 20 * time.Second

// pdfSlots bounds concurrent PDF children, so parallel uploads cannot multiply the memory budget
// across the host.
var pdfSlots = make(chan struct{}, pdfChildSlots)

// pdfExecutable is the binary re-executed as the child: the running supervisor (or test) binary.
var pdfExecutable = os.Executable

// extractPDF parses a PDF in a short-lived child process of this binary: stdin is the file,
// stdout one JSON reply, the environment cleared to a single-core memory-limited runtime, CPU
// and memory capped inside the child and wall time capped here. No network, database, tool or
// credential is available to it (rule 08). The text is the pages in order, one line per row; the
// figures are the raster images the pages draw that the vision model reads (WIS-625 post-merge).
func extractPDF(ctx context.Context, data []byte, l Limits) (pdfExtraction, error) {
	// A JSON reply escapes text, so allow for that above the extracted-text budget, and for the
	// figures' base64 as for a scanned PDF's pages.
	body, err := runPDFChildProcess(ctx, data, PDFChildFlag, 8*l.MaxExtractedText+1<<20+maxScanOutput)
	if err != nil {
		return pdfExtraction{}, err
	}
	var reply pdfReply
	if strictjson.Decode(body, &reply, len(body)+1) != nil || reply.SchemaVersion != 1 {
		return pdfExtraction{}, ErrExtractionUnavailable
	}
	return pdfText(reply, l)
}

// pdfExtraction is a typed PDF's text, the figures its pages draw (each still unverified bytes:
// the caller checks it as an uploaded photo) and the number of small images skipped.
type pdfExtraction struct {
	text    string
	figures []pdfFigure
	skipped int
}

// pdfFigure is image Index (from 1) of page Page.
type pdfFigure struct {
	page, index int
	image       PDFPageImage
}

// runPDFChildProcess runs one child of this binary in the given mode: stdin is the file, stdout
// one JSON reply of at most limit bytes, the environment cleared to a single-core memory-limited
// runtime, CPU and memory capped inside the child and wall time capped here. The child confines
// itself (confineChild) before reading: no network, no filesystem, no program, a hard memory
// ceiling (rule 08).
//
// The caller's context ends both the wait for a slot and the child (killed with its process
// group), so an upload whose client went away frees its slot at once (WIS-624 post-merge review);
// the wall-time limit holds whatever the caller's deadline. A caller that gave up gets its
// context's error, never a verdict on the file.
func runPDFChildProcess(parent context.Context, data []byte, flag string, limit int64) ([]byte, error) {
	select {
	case pdfSlots <- struct{}{}:
		defer func() { <-pdfSlots }()
	case <-parent.Done():
		return nil, parent.Err()
	case <-time.After(pdfSlotWait):
		return nil, ErrExtractionBusy
	}
	bin, err := pdfExecutable()
	if err != nil {
		return nil, ErrExtractionUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, pdfChildWallTime)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, flag)
	cmd.Env = []string{"GOMAXPROCS=1", "GOMEMLIMIT=384MiB", "GOTRACEBACK=none"}
	cmd.Dir = "/" // exists in every image (a scratch image has no /tmp); the child opens nothing
	// Its own process group, killed whole on timeout: defence in depth, as the sandbox already
	// refuses fork-like clones.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Stdin = bytes.NewReader(data)
	var stdout boundedBuffer
	stdout.limit = limit
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard // a parser's diagnostics can quote document text: never kept
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	switch {
	case parent.Err() != nil:
		return nil, parent.Err() // the caller gave up: says nothing about the file
	case ctx.Err() != nil:
		return nil, budgetExceeded("wall clock") // the file needs more than the budget
	case stdout.overflow:
		return nil, ErrTooLarge
	}
	body, started := bytes.CutPrefix(stdout.Bytes(), []byte(pdfChildReady))
	if !started {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) && exit.ExitCode() == pdfExitNoSandbox {
			// The OS refused the sandbox (confineChild): fail closed, never parse unconfined.
			step, _ := bytes.CutPrefix(stdout.Bytes(), []byte(pdfChildNoSandbox))
			return nil, fmt.Errorf("%w (sandbox unavailable: %s)", ErrExtractionUnavailable, diagnostic(step))
		}
		return nil, ErrExtractionUnavailable // could not start, or not a child-capable binary
	}
	if runErr != nil {
		var exit *exec.ExitError
		switch {
		case errors.As(runErr, &exit) && exit.ExitCode() == pdfExitBudget:
			return nil, budgetExceeded("memory")
		case errors.As(runErr, &exit) && killedForCPU(exit):
			return nil, budgetExceeded("cpu")
		case errors.As(runErr, &exit) && killedBySandbox(exit):
			// A syscall outside the allowlist: the sandbox's verdict, not the file's, so a 503 the
			// operator sees in the log (the kernel audit log names the syscall), never a 413.
			return nil, fmt.Errorf("%w (sandbox refused a syscall)", ErrExtractionUnavailable)
		}
		// It started, then died parsing this file (a runtime out-of-memory crash, an OS kill):
		// the file decided it, so it is a 4xx, never a 503 that invites resending it.
		return nil, budgetExceeded("child failure")
	}
	return body, nil
}

// pdfText applies the reply: a refusal maps to its error; otherwise no page may hold content that
// neither the text nor the vision model can read ("unreadable"), some page must have typed text
// (a PDF of pictures alone is a scan: /source-scan-batches), and each figure is kept for the
// vision model.
func pdfText(reply pdfReply, l Limits) (pdfExtraction, error) {
	var out pdfExtraction
	if reply.Status == "refused" {
		switch reply.Reason {
		case pdfReasonEncrypted:
			return out, ErrPDFEncrypted
		case pdfReasonActive:
			return out, ErrPDFActiveContent
		case pdfReasonExternal:
			return out, ErrPDFExternalReference
		case pdfReasonPages:
			return out, ErrPDFTooManyPages
		case pdfReasonBadText:
			return out, ErrPDFTextUnreadable
		case pdfReasonTooManyImages:
			return out, ErrPDFTooManyImages
		case pdfReasonScanHidden:
			return out, ErrPDFScanHiddenContent
		case pdfReasonScanImage:
			return out, ErrPDFScanImageUnsupported
		case pdfReasonScanLarge:
			return out, ErrTooLarge
		default:
			return out, ErrUnreadableSource
		}
	}
	if reply.Status != "ok" || len(reply.Pages) == 0 || len(reply.Pages) > MaxPDFPages {
		return out, ErrExtractionUnavailable
	}
	var b strings.Builder
	text := false
	for i, p := range reply.Pages {
		if p.Number != i+1 || p.Skipped < 0 {
			return pdfExtraction{}, ErrExtractionUnavailable
		}
		switch p.Status {
		case "unreadable":
			return pdfExtraction{}, ErrPDFNoTextLayer
		case "blank", "image":
		case "text":
			text = true
			b.WriteString(p.Text)
			if !strings.HasSuffix(p.Text, "\n") {
				b.WriteByte('\n')
			}
		default:
			return pdfExtraction{}, ErrExtractionUnavailable
		}
		if int64(b.Len()) > l.MaxExtractedText {
			return pdfExtraction{}, ErrTooLarge
		}
		out.skipped += p.Skipped
		for k, img := range p.Images {
			if img.Number != k+1 {
				return pdfExtraction{}, ErrExtractionUnavailable
			}
			out.figures = append(out.figures, pdfFigure{page: p.Number, index: img.Number, image: img})
		}
	}
	if len(out.figures) > maxDocumentImages {
		return pdfExtraction{}, ErrExtractionUnavailable // the child bounds them: a reply over it is not trusted
	}
	if !text {
		return pdfExtraction{}, ErrPDFNoTextLayer
	}
	out.text = b.String()
	return out, nil
}

// budgetExceeded is ErrPDFParseBudget with the limit that ended the child, for diagnostics and
// tests; callers match it with errors.Is.
func budgetExceeded(cause string) error { return fmt.Errorf("%w (%s)", ErrPDFParseBudget, cause) }

// diagnostic is the first line of a child's own diagnostic, printable ASCII, at most 200 bytes.
func diagnostic(b []byte) string {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	if len(line) > 200 {
		line = line[:200]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, string(line))
}

// boundedBuffer keeps at most limit bytes and records an overflow instead of growing.
type boundedBuffer struct {
	bytes.Buffer
	limit    int64
	overflow bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if int64(b.Len()+len(p)) > b.limit {
		b.overflow = true
		return 0, errors.New("child output over budget")
	}
	return b.Buffer.Write(p)
}
