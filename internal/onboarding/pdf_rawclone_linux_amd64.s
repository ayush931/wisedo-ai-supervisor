#include "textflag.h"

// func rawCloneParked(flags, stack uintptr) int64
TEXT ·rawCloneParked(SB), NOSPLIT, $16-24
	// Block every signal around the clone, so the new thread never runs a signal handler.
	MOVQ $-1, 0(SP)
	MOVQ $14, AX // rt_sigprocmask
	MOVQ $2, DI  // SIG_SETMASK
	LEAQ 0(SP), SI
	LEAQ 8(SP), DX
	MOVQ $8, R10
	SYSCALL
	MOVQ flags+0(FP), DI
	MOVQ stack+8(FP), SI
	XORQ DX, DX
	XORQ R10, R10
	XORQ R8, R8
	MOVQ $56, AX // clone
	SYSCALL
	CMPQ AX, $0
	JEQ park
	MOVQ AX, R12
	MOVQ $14, AX // restore this thread's mask
	MOVQ $2, DI
	LEAQ 8(SP), SI
	XORQ DX, DX
	MOVQ $8, R10
	SYSCALL
	MOVQ R12, ret+16(FP)
	RET

park:
	// The new thread, on its own zeroed stack: wait on a zero word below SP, for ever.
	MOVQ SP, DI
	SUBQ $64, DI
	MOVQ $128, SI // FUTEX_WAIT_PRIVATE
	XORQ DX, DX
	XORQ R10, R10
	MOVQ $202, AX // futex
	SYSCALL
	JMP park
