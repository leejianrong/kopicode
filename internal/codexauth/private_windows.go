//go:build windows

package codexauth

import (
	"errors"
	"fmt"
	"os"
)

// checkPrivate on Windows only confirms the file exists and is regular: unix
// permission bits do not describe its access control, and this package does not
// guess at ACLs.
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
	return nil
}
