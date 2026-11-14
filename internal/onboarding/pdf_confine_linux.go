//go:build linux && amd64

package onboarding

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// PDFSandboxSupported reports a platform where the PDF child can confine itself; elsewhere every
// PDF is EXTRACTION_UNAVAILABLE and the PDF tests report BLOCKED.
const PDFSandboxSupported = true

// pdfChildMemorySlack is room above the watchdog budget in both hard ceilings, so the watchdog
// normally ends a bomb first ("memory") and the kernel ceilings are the backstop it cannot outrun.
const pdfChildMemorySlack = 256 << 20

// pdfChildThreads is how many threads the child has before its filter: main (locked), sysmon,
// one for the other goroutines while main is blocked or locked, and spares. With GOMAXPROCS=1 a
// parse needs at most three; idle threads are parked and reused, never exited.
const pdfChildThreads = 8

// confineChild locks the PDF child down before it reads a byte of the file (rule 08). Any step
// that fails is an error naming the step, and the child then exits without starting: an
// unconfined parse never runs. It is applied inside the child because Go cannot run code between
// fork and exec; the child is this binary, trusted until it reads the file.
//
//   - Inherited descriptors are closed: any fd above stdio without close-on-exec crossed the exec
//     from the parent (the runtime's own fds are all close-on-exec).
//   - The local time zone is loaded now, so the parse never needs to open a file for it.
//   - Not dumpable (checked), and RLIMIT_CORE 0: a crash never writes document memory to disk,
//     not even through a piped core_pattern, and no same-uid process can read its memory. The
//     prctl allowlist cannot make it dumpable again.
//   - The runtime's threads are made now (warmThreads): the static build's filter refuses clone,
//     so a parse cannot exhaust the container's tasks and kernel memory with threads.
//   - RLIMIT_CPU: CPU seconds.
//   - RLIMIT_DATA: a hard ceiling on private writable memory (the Go heap), its start-up size plus
//     the watchdog budget plus slack. Unlike the 10 ms watchdog and the soft GOMEMLIMIT, an
//     allocation past it fails at once, so a bomb cannot outgrow the poll.
//   - RLIMIT_AS (not under -race, whose shadow memory reserves terabytes): the same ceiling over
//     all mappings, so memory cannot leave the RLIMIT_DATA count by being made read-only or
//     PROT_NONE while it stays resident.
//   - A seccomp allowlist (no_new_privs, all threads): see seccompFilter. User and network
//     namespaces were the alternative, but Docker's default seccomp profile and Ubuntu's AppArmor
//     userns restriction both refuse them to an unprivileged process; seccomp needs no privilege.
func confineChild() error {
	if err := closeInheritedFDs(); err != nil {
		return fmt.Errorf("close inherited fds: %w", err)
	}
	time.Now().Zone() // loads time.Local
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("PR_SET_DUMPABLE: %w", err)
	}
	if d, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0); err != nil || d != 0 {
		return fmt.Errorf("PR_GET_DUMPABLE: %d %v", d, err)
	}
	dataKB, err := procStatus("VmData:")
	if err != nil {
		return fmt.Errorf("read VmData: %w", err)
	}
	sizeKB, err := procStatus("VmSize:")
	if err != nil {
		return fmt.Errorf("read VmSize: %w", err)
	}
	data, size := dataKB<<10, sizeKB<<10
	limits := []struct {
		name     string
		resource int
		value    uint64
	}{
		{"RLIMIT_CPU", unix.RLIMIT_CPU, pdfChildCPUSeconds},
		{"RLIMIT_CORE", unix.RLIMIT_CORE, 0},
		{"RLIMIT_DATA", unix.RLIMIT_DATA, data + pdfChildMemoryBytes + pdfChildMemorySlack},
	}
	if pdfChildASLimit {
		limits = append(limits, struct {
			name     string
			resource int
			value    uint64
		}{"RLIMIT_AS", unix.RLIMIT_AS, size + pdfChildMemoryBytes + pdfChildMemorySlack})
	}
	for _, l := range limits {
		if err := unix.Setrlimit(l.resource, &unix.Rlimit{Cur: l.value, Max: l.value}); err != nil {
			return fmt.Errorf("%s: %w", l.name, err)
		}
	}
	// The thread budget assumes one P whatever the environment says: with more, the runtime can
	// want a ninth thread under load, and the static filter kills the child for that clone.
	runtime.GOMAXPROCS(1)
	if err := warmThreads(); err != nil {
		return fmt.Errorf("warm threads: %w", err)
	}
	runtime.LockOSThread() // no_new_privs is per thread; TSYNC carries it and the filter to all
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	filter, err := seccompFilter(uint32(os.Getpid()))
	if err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	r, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&prog)))
	if errno != 0 || r != 0 {
		return fmt.Errorf("seccomp: %v (thread %d)", errno, r)
	}
	return nil
}

// closeInheritedFDs closes every descriptor above stdio that lacks close-on-exec.
func closeInheritedFDs() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd <= 2 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err == nil && flags&unix.FD_CLOEXEC == 0 {
			if err := unix.Close(fd); err != nil {
				return fmt.Errorf("close inherited fd %d: %w", fd, err)
			}
		}
	}
	return nil
}

// pdfChildAllowedSyscalls are the only syscalls the confined child may make without an argument
// check. Measured with `strace -f` on the PDF children of the onboarding tests (text, scanned
// pages, bomb, refusals) and of the static production binary, after their seccomp call:
//
//	static: epoll_pwait exit_group futex getpid gettid madvise mmap nanosleep prctl read
//	        rt_sigprocmask rt_sigreturn sched_yield sigaltstack tgkill write (+ openat, refused)
//	cgo/race add: clone clone3 mprotect munmap rseq set_robust_list
//	(the static build's threads are made before the filter, see warmThreads; it then refuses clone)
//
// plus exit (a thread ending), restart_syscall (the kernel's resumption of an interrupted sleep),
// clock_gettime (the vDSO's fallback) and rt_sigaction (the runtime resets a handler before it
// dies from a signal). mprotect is allowed only in cgo and race builds (pdfChildAllowMprotect);
// the production image is built with CGO_ENABLED=0.
var pdfChildAllowedSyscalls = []uint32{
	unix.SYS_READ, unix.SYS_WRITE, // stdin file, stdout reply (only fds 0-2 and the runtime's)
	unix.SYS_FUTEX, unix.SYS_NANOSLEEP, unix.SYS_SCHED_YIELD, unix.SYS_EPOLL_PWAIT,
	unix.SYS_RESTART_SYSCALL, unix.SYS_CLOCK_GETTIME,
	unix.SYS_MUNMAP, unix.SYS_MADVISE,
	unix.SYS_RT_SIGPROCMASK, unix.SYS_RT_SIGRETURN, unix.SYS_RT_SIGACTION, unix.SYS_SIGALTSTACK,
	unix.SYS_GETPID, unix.SYS_GETTID, unix.SYS_RSEQ, unix.SYS_SET_ROBUST_LIST,
	unix.SYS_EXIT, unix.SYS_EXIT_GROUP,
}

// pdfChildRefusedSyscalls answer EPERM instead of killing the child: a parser that tries to open
// a file or make a socket gets an error it can handle, and the file is refused as unreadable.
var pdfChildRefusedSyscalls = []uint32{unix.SYS_OPEN, unix.SYS_OPENAT, unix.SYS_OPENAT2, unix.SYS_CREAT, unix.SYS_SOCKET}

// Thread creation, exact masks only: the Go runtime's (runtime.cloneFlags, plus the CLONE_SETTLS
// its amd64 clone adds), and glibc pthread_create's (cgo, race) after clone3 answers ENOSYS. A
// fork-like or namespace clone matches neither.
const (
	cloneThread      = unix.CLONE_VM | unix.CLONE_FS | unix.CLONE_FILES | unix.CLONE_SIGHAND | unix.CLONE_SYSVSEM | unix.CLONE_THREAD
	cloneGoThread    = cloneThread | unix.CLONE_SETTLS
	cloneGlibcThread = cloneGoThread | unix.CLONE_PARENT_SETTID | unix.CLONE_CHILD_CLEARTID
)

// seccompFilter is the child's allowlist. clone3 is ENOSYS (glibc then uses clone); allowlisted
// calls pass; file opens and socket are EPERM; clone passes only with a thread mask; mmap only
// MAP_PRIVATE without MAP_GROWSDOWN (a stack mapping escapes RLIMIT_DATA) and, in the static
// build, without MAP_STACK; kill and tgkill only to this process; prctl only PR_SET_VMA (the
// runtime names its mappings). Anything else, a foreign architecture or x32 call included, kills
// the child with SIGSYS, which the parent reports as the sandbox refusing a syscall, not as the
// file's fault. seccomp_data: nr at 0, arch at 4, args from 16 (8 bytes each, low half first).
// Every checked argument is an int or uses only its low 32 bits.
func seccompFilter(pid uint32) ([]unix.SockFilter, error) {
	const (
		ld   = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq  = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		jge  = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		and  = unix.BPF_ALU | unix.BPF_AND | unix.BPF_K
		ret  = unix.BPF_RET | unix.BPF_K
		arg0 = 16
		arg3 = 16 + 3*8
	)
	type insn struct {
		code   uint16
		k      uint32
		jt, jf string // label to jump to; "" is the next instruction
	}
	var prog []insn
	labels := map[string]int{}
	emit := func(i ...insn) { prog = append(prog, i...) }
	label := func(name string) { labels[name] = len(prog) }
	is := func(k uint32, to string) insn { return insn{code: jeq, k: k, jt: to} }

	emit(insn{code: ld, k: 4}, insn{code: jeq, k: unix.AUDIT_ARCH_X86_64, jf: "kill"},
		insn{code: ld, k: 0}, insn{code: jge, k: 0x40000000, jt: "kill"}, // __X32_SYSCALL_BIT
		is(unix.SYS_CLONE3, "enosys"))
	allowed := pdfChildAllowedSyscalls
	if pdfChildAllowMprotect {
		allowed = append(allowed[:len(allowed):len(allowed)], unix.SYS_MPROTECT)
	}
	for _, nr := range allowed {
		emit(is(nr, "allow"))
	}
	for _, nr := range pdfChildRefusedSyscalls {
		emit(is(nr, "eperm"))
	}
	mmapFlags := uint32(unix.MAP_SHARED | unix.MAP_PRIVATE | unix.MAP_GROWSDOWN)
	if !pdfChildAllowMapStack {
		mmapFlags |= unix.MAP_STACK
	}
	if pdfChildAllowClone {
		emit(is(unix.SYS_CLONE, "clone"))
	}
	emit(is(unix.SYS_MMAP, "mmap"), is(unix.SYS_KILL, "self"),
		is(unix.SYS_TGKILL, "self"), is(unix.SYS_PRCTL, "prctl"), insn{code: ret, k: unix.SECCOMP_RET_KILL_PROCESS})
	label("clone")
	emit(insn{code: ld, k: arg0}, is(cloneGoThread, "allow"), is(cloneGlibcThread, "allow"), insn{code: ret, k: unix.SECCOMP_RET_KILL_PROCESS})
	label("mmap")
	emit(insn{code: ld, k: arg3}, insn{code: and, k: mmapFlags}, is(unix.MAP_PRIVATE, "allow"), insn{code: ret, k: unix.SECCOMP_RET_KILL_PROCESS})
	label("self")
	emit(insn{code: ld, k: arg0}, is(pid, "allow"), insn{code: ret, k: unix.SECCOMP_RET_KILL_PROCESS})
	label("prctl")
	emit(insn{code: ld, k: arg0}, is(unix.PR_SET_VMA, "allow"), insn{code: ret, k: unix.SECCOMP_RET_KILL_PROCESS})
	label("eperm")
	emit(insn{code: ret, k: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	label("allow")
	emit(insn{code: ret, k: unix.SECCOMP_RET_ALLOW})
	label("kill")
	emit(insn{code: ret, k: unix.SECCOMP_RET_KILL_PROCESS})
	label("enosys")
	emit(insn{code: ret, k: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)})

	out := make([]unix.SockFilter, len(prog))
	for i, p := range prog {
		out[i] = unix.SockFilter{Code: p.code, K: p.k}
		for _, j := range []struct {
			to  string
			dst *uint8
		}{{p.jt, &out[i].Jt}, {p.jf, &out[i].Jf}} {
			if j.to == "" {
				continue
			}
			off := labels[j.to] - i - 1
			if off < 0 || off > 255 {
				return nil, fmt.Errorf("seccomp jump to %s out of range", j.to)
			}
			*j.dst = uint8(off)
		}
	}
	return out, nil
}

// procStatus is one figure of /proc/self/status now: VmData and VmSize in kB, Threads a count.
func procStatus(field string) (uint64, error) {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range bytes.Split(status, []byte("\n")) {
		if v, ok := bytes.CutPrefix(line, []byte(field)); ok {
			return strconv.ParseUint(string(bytes.TrimSuffix(bytes.TrimSpace(v), []byte(" kB"))), 10, 64)
		}
	}
	return 0, errors.New("no " + field + " in /proc/self/status")
}

// warmThreads makes the runtime's threads while clone is still allowed (the static build's filter
// refuses every clone): goroutines blocked in a raw sleep each hold a thread, so the scheduler
// starts new ones, which then stay parked for reuse. It stops at pdfChildThreads threads.
func warmThreads() error {
	for round := 0; round < 20; round++ {
		if n, err := procStatus("Threads:"); err != nil || n >= pdfChildThreads {
			return err
		}
		var wg sync.WaitGroup
		for i := 0; i < pdfChildThreads; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ts := unix.Timespec{Nsec: 30e6}
				_ = unix.Nanosleep(&ts, nil)
			}()
		}
		wg.Wait()
	}
	return fmt.Errorf("only reached fewer than %d threads", pdfChildThreads)
}
