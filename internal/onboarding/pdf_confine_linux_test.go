//go:build linux && amd64

package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Test-only child modes (never shipped): the confinement probe, and the same probe after
// confinement was made impossible.
const (
	confineProbeFlag      = "--test-pdf-confine-probe"
	unconfinableProbeFlag = "--test-pdf-confine-probe-unconfinable"
)

func init() {
	testChildProbes[confineProbeFlag] = confineProbe
	testChildProbes[unconfinableProbeFlag] = func() int {
		// A hard RLIMIT_DATA already below the child's ceiling: confineChild cannot set its own. It
		// is 256 MiB, not lower: at 64 MiB the Go runtime of this test binary itself ran out of
		// memory in about one run in sixty, before it could report the sandbox step (the
		// TestPDFChildConfinement flake), which is the test's limit, not the sandbox's.
		if syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: 256 << 20, Max: 256 << 20}) != nil {
			return 1
		}
		return confineProbe()
	}
}

// probeInput is what the parent tells the probe: its loopback listener, its pid, a sentinel file
// and a descriptor of that file it leaked into the child (no close-on-exec).
type probeInput struct {
	Mode     string `json:"mode"`    // "soft", "attempt", "mprotect_trick", "alloc" or "sleep"
	Attempt  string `json:"attempt"` // for "attempt": one call the sandbox must kill the child for
	Addr     string `json:"addr"`
	Parent   int    `json:"parent"`
	Sentinel string `json:"sentinel"`
	LeakedFD int    `json:"leaked_fd"`
}

// confineProbe starts exactly as the PDF child does (startPDFChild), then tries what a hostile
// parse would. "soft" reports the errors of calls the sandbox refuses with EPERM (or that fail on
// a limit); "attempt" makes one call the sandbox must kill the child for, and prints SURVIVED if
// it returns; "mprotect_trick" allocates, touches and makes read-only 256 MiB at a time, to move
// memory out of the RLIMIT_DATA count, and prints ESCAPED if 3 GiB stay resident.
func confineProbe() int {
	raw, code, ok := startPDFChild(os.Stdin, os.Stdout)
	if !ok {
		return code
	}
	var in probeInput
	if json.Unmarshal(raw, &in) != nil {
		return 1
	}
	errno := func(_, _ uintptr, e syscall.Errno) error {
		if e == 0 {
			return nil
		}
		return e
	}
	switch in.Mode {
	case "gomaxprocs":
		fmt.Println(runtime.GOMAXPROCS(0))
		return 0
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	case "alloc":
		b := make([]byte, 3<<30)
		b[len(b)-1] = 1
		fmt.Println("ALLOCATED")
		return 0
	case "mprotect_trick":
		for i := 0; i < 12; i++ {
			m, err := syscall.Mmap(-1, 0, 256<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
			if err == nil {
				for j := 0; j < len(m); j += 4096 {
					m[j] = 1
				}
				err = syscall.Mprotect(m, syscall.PROT_READ)
			}
			if err != nil {
				fmt.Println("CONTAINED", err)
				return 0
			}
		}
		fmt.Println("ESCAPED")
		return 0
	case "attempt":
		var err error
		var lim syscall.Rlimit
		switch in.Attempt {
		case "utimes":
			err = syscall.UtimesNano(in.Sentinel, []syscall.Timespec{{Sec: 1}, {Sec: 1}})
		case "setxattr":
			err = syscall.Setxattr(in.Sentinel, "user.probe", []byte("x"), 0)
		case "getxattr":
			_, err = syscall.Getxattr(in.Sentinel, "user.probe", make([]byte, 8))
		case "chmod":
			err = syscall.Chmod(in.Sentinel, 0o777)
		case "exec":
			err = syscall.Exec("/bin/true", []string{"true"}, nil) // replaces the probe if allowed
		case "fork":
			err = errno(syscall.RawSyscall6(syscall.SYS_CLONE, uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0))
		case "clone_newns":
			err = errno(syscall.RawSyscall6(syscall.SYS_CLONE, syscall.CLONE_NEWUSER|syscall.CLONE_NEWNET|uintptr(syscall.SIGCHLD), 0, 0, 0, 0, 0))
		case "unshare":
			err = syscall.Unshare(syscall.CLONE_NEWNET)
		case "kill_parent":
			err = syscall.Kill(in.Parent, 0)
		case "tgkill_parent":
			err = syscall.Tgkill(in.Parent, in.Parent, 0)
		case "prlimit_parent":
			err = errno(syscall.RawSyscall6(syscall.SYS_PRLIMIT64, uintptr(in.Parent), syscall.RLIMIT_NOFILE, 0, uintptr(unsafe.Pointer(&lim)), 0, 0))
		case "mmap_shared":
			_, err = syscall.Mmap(-1, 0, 3<<30, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_ANON)
		case "mmap_growsdown":
			_, err = syscall.Mmap(-1, 0, 1<<30, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_GROWSDOWN)
		case "clone_threads": // threads parked forever would exhaust the container's tasks
			for i := 0; i < 64 && err == nil; i++ {
				stack, merr := syscall.Mmap(-1, 0, 64<<10, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
				if merr != nil {
					err = merr
					break
				}
				if r := rawCloneParked(cloneGoThread, uintptr(unsafe.Pointer(&stack[len(stack)-128]))); r < 0 {
					err = syscall.Errno(-r)
				}
			}
		case "mprotect":
			err = syscall.Mprotect(make([]byte, 1<<20), syscall.PROT_READ)
		case "memfd":
			_, _, e := syscall.Syscall(319 /* memfd_create */, 0, 0, 0)
			err = errno(0, 0, e)
		default:
			return 1
		}
		fmt.Println("SURVIVED", err)
		return 0
	}
	result := map[string]string{}
	note := func(name string, err error) { result[name] = fmt.Sprint(err) }
	conn, err := net.DialTimeout("tcp", in.Addr, time.Second)
	if err == nil {
		conn.Close()
	}
	note("dial", err)
	_, err = syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	note("socket", err)
	_, err = os.Open("/etc/hosts")
	note("open", err)
	_, err = os.Create(filepath.Join(os.TempDir(), "pdf-probe-"+strconv.Itoa(os.Getpid())))
	note("create", err)
	_, err = syscall.Read(in.LeakedFD, make([]byte, 8))
	note("leaked_fd", err)
	private, err := syscall.Mmap(-1, 0, 3<<30, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err == nil {
		syscall.Munmap(private)
	}
	note("mmap_private", err)
	if json.NewEncoder(os.Stdout).Encode(result) != nil {
		return pdfExitBadOutput
	}
	return 0
}

// wantSandboxKill asserts that a probe was killed by the sandbox: the parent reports a 503
// "(sandbox refused a syscall)", not the file's 413, and gets no reply.
func wantSandboxKill(t *testing.T, attempt string, body []byte, err error) {
	t.Helper()
	if body != nil || !errors.Is(err, ErrExtractionUnavailable) || !strings.HasSuffix(err.Error(), "(sandbox refused a syscall)") {
		t.Errorf("%s inside the child: %q %v, want the sandbox to kill it (EXTRACTION_UNAVAILABLE)", attempt, body, err)
	}
}

// TestPDFChildConfinement (Codex reviews of #237): the PDF child, started through the real spawn
// path, reaches no network (a dial to a listening loopback port fails), opens or creates no file,
// reads no descriptor leaked from the parent, and maps no private memory past its ceiling. Every
// other hostile call (changing a parent-created sentinel's times, xattrs or mode, running a
// program, creating a process or namespace, signalling or inspecting the parent, shared, stack or
// memfd memory, mprotect in the static build) kills the child, and the parent reports the
// sandbox, not the file: 503 EXTRACTION_UNAVAILABLE, never a 413.
func TestPDFChildConfinement(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
			accepted <- struct{}{}
		}
	}()
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(sentinel)
	// A descriptor without close-on-exec crosses the exec. At 100 it cannot be confused with one
	// the child's runtime opens itself.
	file, err := syscall.Open(sentinel, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(file)
	const leaked = 100
	if err := syscall.Dup3(file, leaked, 0); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(leaked)
	input := func(mode, attempt string) []byte {
		b, _ := json.Marshal(probeInput{Mode: mode, Attempt: attempt, Addr: ln.Addr().String(), Parent: os.Getpid(), Sentinel: sentinel, LeakedFD: leaked})
		return b
	}

	body, err := runPDFChildProcess(t.Context(), input("soft", ""), confineProbeFlag, 1<<20)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("probe reply %q: %v", body, err)
	}
	const eperm = "operation not permitted"
	for name, want := range map[string]string{
		"dial": eperm, "socket": eperm, "open": eperm, "create": eperm,
		"leaked_fd": "bad file descriptor", "mmap_private": "cannot allocate memory",
	} {
		if !strings.Contains(got[name], want) {
			t.Errorf("%s inside the child: %q, want %q", name, got[name], want)
		}
	}

	attempts := []string{"utimes", "setxattr", "getxattr", "chmod", "exec", "fork", "clone_newns", "unshare",
		"kill_parent", "tgkill_parent", "prlimit_parent", "mmap_shared", "mmap_growsdown", "memfd"}
	for _, attempt := range attempts {
		body, err := runPDFChildProcess(t.Context(), input("attempt", attempt), confineProbeFlag, 1<<20)
		wantSandboxKill(t, attempt, body, err)
	}
	select {
	case <-accepted:
		t.Error("the child reached the parent's loopback listener")
	case <-time.After(100 * time.Millisecond):
	}
	if after, _ := os.Stat(sentinel); !after.ModTime().Equal(before.ModTime()) || after.Mode() != before.Mode() {
		t.Errorf("the child changed the sentinel: %v %v -> %v %v", before.ModTime(), before.Mode(), after.ModTime(), after.Mode())
	}

	// Memory made read-only stays counted: the static build kills the child at mprotect, the cgo
	// build stops it at RLIMIT_AS. The race build has neither (see pdf_confine_race.go), so the
	// trick is not run there.
	if pdfChildASLimit {
		body, err = runPDFChildProcess(t.Context(), input("mprotect_trick", ""), confineProbeFlag, 1<<20)
		contained := bytes.HasPrefix(body, []byte("CONTAINED")) && err == nil ||
			!pdfChildAllowMprotect && errors.Is(err, ErrExtractionUnavailable)
		if !contained {
			t.Errorf("read-only memory trick: %q %v, want it contained", body, err)
		}
	}

	// One allocation far past the ceiling ends the child, not the parent, as the file's 413.
	body, err = runPDFChildProcess(t.Context(), input("alloc", ""), confineProbeFlag, 1<<20)
	if bytes.Contains(body, []byte("ALLOCATED")) || !errors.Is(err, ErrPDFParseBudget) {
		t.Errorf("over-budget allocation: %q %v, want the child to fail on its memory ceiling", body, err)
	}

	// Past its wall time the child's process group is killed at once (Setpgid + Cancel), not
	// after WaitDelay, and the file gets the wall-clock 413.
	saved := pdfChildWallTime
	pdfChildWallTime = 500 * time.Millisecond
	start := time.Now()
	_, err = runPDFChildProcess(t.Context(), input("sleep", ""), confineProbeFlag, 1<<20)
	pdfChildWallTime = saved
	if took := time.Since(start); !errors.Is(err, ErrPDFParseBudget) || !strings.HasSuffix(err.Error(), "(wall clock)") || took > 1500*time.Millisecond {
		t.Errorf("stuck child: %v after %s, want a wall-clock refusal well before WaitDelay", err, took)
	}

	// Not dumpable before it reads the file: the parent, same uid, cannot read the child's
	// /proc entries (its memory, environment), and no core is written.
	// It waits on the open stdin; a deadline and a cleanup kill its process group whatever happens.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], confineProbeFlag)
	child.Env = []string{"GOMAXPROCS=1", "GOMEMLIMIT=384MiB", "GOTRACEBACK=none"}
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Cancel = func() error { return syscall.Kill(-child.Process.Pid, syscall.SIGKILL) }
	child.WaitDelay = time.Second
	stdin, _ := child.StdinPipe()
	stdout, _ := child.StdoutPipe()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		stdin.Close()
		_ = child.Wait()
	})
	ready := make([]byte, len(pdfChildReady))
	if _, err := io.ReadFull(stdout, ready); err != nil || string(ready) != pdfChildReady {
		t.Fatalf("probe did not start: %q %v", ready, err)
	}
	_, err = os.ReadFile(fmt.Sprintf("/proc/%d/environ", child.Process.Pid))
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("reading the waiting child's /proc environ: %v, want permission denied (not dumpable)", err)
	}

	// Fail closed: a child that cannot confine itself never starts, never reads the file, and the
	// upload is EXTRACTION_UNAVAILABLE naming the step that failed, not an unconfined parse.
	body, err = runPDFChildProcess(t.Context(), input("soft", ""), unconfinableProbeFlag, 1<<20)
	if body != nil || !errors.Is(err, ErrExtractionUnavailable) || !strings.HasSuffix(err.Error(), "(sandbox unavailable: RLIMIT_DATA: operation not permitted)") {
		t.Fatalf("unconfinable child: %q %v, want EXTRACTION_UNAVAILABLE (sandbox unavailable: RLIMIT_DATA: ...)", body, err)
	}
}

// WIS-624 post-merge review: a caller that gives up (the upload's client went away) kills the
// running child and frees its slot at once, and is told so, not that the file is over budget.
// Before the fix the child ran on a context of its own until the 20 s wall time, holding one of
// the two slots, and was then reported as PDF_PARSE_BUDGET_EXCEEDED.
func TestPDFChildEndsWithTheCaller(t *testing.T) {
	requirePDFSandbox(t)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := runPDFChildProcess(ctx, []byte(`{"mode":"sleep"}`), confineProbeFlag, 1<<20)
	if took := time.Since(start); !errors.Is(err, context.Canceled) || took > 1500*time.Millisecond {
		t.Fatalf("canceled child: %v after %s, want context.Canceled well before the wall time", err, took)
	}
	if n := len(pdfSlots); n != 0 {
		t.Fatalf("%d slots still taken after the child ended", n)
	}
}

// The child's warm threads are sized for one P, and the static filter kills any later clone. A
// child started with a larger GOMAXPROCS (the built-binary test passes no environment) was
// killed by SIGSYS for a ninth thread's clone under load (docs gate, 2026-11-09), so the child
// sets one P itself instead of trusting its parent's environment.
func TestPDFChildUsesOnePWhateverItsEnvironment(t *testing.T) {
	child := exec.CommandContext(t.Context(), os.Args[0], confineProbeFlag)
	child.Env = []string{"GOMAXPROCS=24"}
	child.Stdin = strings.NewReader(`{"mode":"gomaxprocs"}`)
	out, err := child.Output()
	if err != nil || string(out) != pdfChildReady+"1\n" {
		t.Fatalf("child: %q %v, want one P after %q", out, err, pdfChildReady)
	}
}
