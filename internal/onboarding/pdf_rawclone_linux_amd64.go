package onboarding

// rawCloneParked is test support for the PDF sandbox's thread-exhaustion probe and is never
// called by shipped code (the linker drops it). It clones a thread with flags onto stack (the top
// of a separate mapping) with every signal blocked, and the new thread parks for ever in a futex
// wait on its own stack. It returns the new thread id or a negative errno.
func rawCloneParked(flags, stack uintptr) int64
