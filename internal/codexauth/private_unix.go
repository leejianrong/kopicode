//go:build !windows

package codexauth

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// checkPrivate refuses a credential file anyone but its owner can reach, or one
// the current user does not own. Both are what Codex CLI itself avoids, and a
// token in a group-readable file is already shared.
func checkPrivate(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w (looked at %s)", ErrNotFound, path)
		}
		return fmt.Errorf("codexauth: checking %s: %w", path, pathError(err))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w (%s is not a regular file)", ErrInsecure, path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w (%s has mode %04o; chmod 600 it)", ErrInsecure, path, info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w (%s is owned by another user)", ErrInsecure, path)
	}
	return nil
}
