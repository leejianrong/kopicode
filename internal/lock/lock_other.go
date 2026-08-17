//go:build !unix

package lock

import "os"

// Windows — and any other non-unix target — gets a no-op, and the degradation
// is stated rather than implied. docs/SLICE-1.md §8 chose it explicitly,
// matching satay-runtime's posture, and internal/procgroup's Windows file is
// the same shape of honest shortfall.
//
// What is the same: the lock file is still created at .kopicode/lock and still
// carries this process's holder record, so a user who goes looking can see who
// started a session and where its journal is.
//
// What is weaker, and matters: nothing is excluded. Two kopicode sessions in
// one working tree on Windows both start, interleave their edits, and write two
// journals describing one filesystem. That is the failure docs/SLICE-1.md §8
// exists to prevent, and on this platform it is prevented by nobody.
//
// A real fix is LockFileEx with LOCKFILE_FAIL_IMMEDIATELY, which is
// byte-range locking on a HANDLE and behaves closely enough to flock for this
// purpose — including release on process death, which is the property the whole
// design rests on. It needs golang.org/x/sys/windows, because syscall's Windows
// surface does not export it, and CLAUDE.md wants a reason in the PR before a
// dependency lands. Until a Windows user asks, the honest position is the one
// written here: [Supported] is false, and a caller that cares can say so.
const supported = false

func tryLock(*os.File) (bool, error) { return true, nil }

func unlock(*os.File) error { return nil }
