package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/onboarding"
)

// TestBuiltBinaryRunsThePDFChild (WIS-624): the production binary's main runs the PDF child
// before any configuration, with an empty environment, and answers one JSON reply. Unit tests
// exercise their own test binary's hook; only this proves the shipped one.
func TestBuiltBinaryRunsThePDFChild(t *testing.T) {
	if !onboarding.PDFSandboxSupported {
		t.Fatalf("BLOCKED: the PDF parser sandbox needs linux/amd64, this is %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	bin := filepath.Join(t.TempDir(), "supervisor")
	// CGO_ENABLED=0 as in the production image, so the strict static sandbox is the one tested.
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	content := "BT /F1 12 Tf 72 720 Td (Inspect the weld seam.) Tj ET"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.7\n")
	offsets := []int{}
	for i, o := range objs {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)

	child := exec.Command(bin, onboarding.PDFChildFlag)
	child.Env = []string{} // no configuration at all: main must not reach config loading
	child.Stdin = &pdf
	out, err := child.Output()
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	var reply struct {
		SchemaVersion int    `json:"schema_version"`
		Status        string `json:"status"`
		Pages         []struct {
			Number int    `json:"number"`
			Status string `json:"status"`
			Text   string `json:"text"`
		} `json:"pages"`
	}
	body, ready := bytes.CutPrefix(out, []byte("PDF-CHILD-READY\n"))
	if !ready {
		t.Fatalf("the child did not announce itself first: %q", out)
	}
	if err = json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("reply %q: %v", out, err)
	}
	if reply.SchemaVersion != 1 || reply.Status != "ok" || len(reply.Pages) != 1 || strings.TrimSpace(reply.Pages[0].Text) != "Inspect the weld seam." {
		t.Fatalf("reply %+v", reply)
	}
}
