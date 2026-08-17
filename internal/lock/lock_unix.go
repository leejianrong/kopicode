//go:build unix

package lock

import (
	"errors"
	"os"
	"syscall"
)

// supported is true here: flock(2) is a real kernel lock on every unix target.
const supported = true

// tryLock takes an exclusive advisory lock without blocking, reporting
// (false, nil) when someone else holds it.
//
// flock rather than fcntl/POSIX record locking, and the choice is not
// cosmetic. A POSIX lock is owned by the *process*, so it is dropped the moment
// any file descriptor for that file is closed anywhere in the process — a
// second Open of .kopicode/lock, an unrelated read of it in a test — and two
// sessions in one process cannot exclude each other at all. flock's lock
// belongs to the open file description, so a second open in the same process is
// refused exactly as a second process would be, which is what makes the
// exclusion testable without spawning anything.
//
// syscall rather than golang.org/x/sys: flock, LOCK_EX, LOCK_NB and LOCK_UN are
// in the standard library on every unix Go supports, so this needs no
// dependency and no CGo. ADR-0001's distribution promise is cross-compilation
// and this file is exactly where that gets broken.
func tryLock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			// A signal arrived mid-call. Retrying is the documented response
			// and is not a busy loop: LOCK_NB means the call never blocks, so
			// this can only spin as fast as signals arrive.
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			// Held by someone else. EAGAIN and EWOULDBLOCK are the same value
			// on every platform this builds for.
			return false, nil
		default:
			return false, err
		}
	}
}

// unlock drops the lock explicitly. Closing the file would do it too; see
// [Lock.Release] on why both happen.
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
