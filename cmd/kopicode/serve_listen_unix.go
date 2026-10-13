//go:build unix

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// listenSocket claims path and listens on it, mode 0600. release removes the
// socket and gives the claim up.
//
// Who holds a path is decided by an advisory lock on <path>.lock, not by
// connecting to the socket: a probe connection would be accepted by the live
// server and replace its client. The lock dies with its holder, so a socket file
// no process holds is a crash's leftover and is replaced, while a path a live
// process holds is refused. Anything at path that is not a socket is refused
// too, never removed. The lock file stays where it is after exit: unlinking it
// would let two processes lock two different files of the same name.
func listenSocket(path string) (net.Listener, func(), error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	lockFile, err := os.OpenFile(abs+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the lock file: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil, socketHeld(abs)
		}
		return nil, nil, fmt.Errorf("locking %s.lock: %w", abs, err)
	}
	fail := func(err error) (net.Listener, func(), error) {
		_ = lockFile.Close() // releases the lock
		return nil, nil, err
	}

	switch fi, err := os.Lstat(abs); {
	case err == nil && fi.Mode()&os.ModeSocket == 0:
		return fail(fmt.Errorf("%s exists and is not a socket; refusing to replace it", abs))
	case err == nil:
		if err := os.Remove(abs); err != nil {
			return fail(fmt.Errorf("removing the stale socket: %w", err))
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fail(err)
	}

	// The mode a socket is created with comes from the umask, so it is set for
	// the one call that creates it. This runs once, before any session or
	// goroutine of this process creates a file.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", abs)
	syscall.Umask(old)
	if err != nil {
		return fail(err)
	}
	if err := os.Chmod(abs, 0o600); err != nil {
		_ = ln.Close()
		return fail(fmt.Errorf("setting the socket's mode: %w", err))
	}
	// net.Listener's Close removes the socket file for a listener it created;
	// release also covers a path the runtime did not unlink.
	return ln, func() {
		_ = ln.Close()
		_ = os.Remove(abs)
		_ = lockFile.Close()
	}, nil
}
