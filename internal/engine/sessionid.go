package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/leejianrong/kopicode/internal/journal"
)

// sessionIDPattern is what a caller-chosen session id may look like: it becomes
// a directory name under .kopicode/sessions, so it starts with a letter or digit
// and holds only letters, digits, dot, underscore and hyphen, at most 128 long.
// No separator, no "..", nothing that could name another directory.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidateNewSessionID checks an id a person or orchestrator chose for a session
// that does not exist yet (--session-id, KOP-227). It is read-only: it refuses an
// id that is not a safe directory name, and one that already has a record,
// because Open would otherwise append to that record and a new session would
// silently continue an old one. Resuming is --resume's job and not checked here.
//
// The check and the Open that follows are not atomic. The working tree's lock
// is what keeps two sessions from opening in one tree, so the window is the one
// the generated ids already live in.
func ValidateNewSessionID(dir, id string) error {
	if !sessionIDPattern.MatchString(id) {
		return fmt.Errorf("session id %q is not usable: it names a directory, so use 1 to 128 letters, digits, "+
			"dots, underscores or hyphens, starting with a letter or digit", id)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("engine: resolving %q: %w", dir, err)
	}
	events := filepath.Join(journal.SessionDir(abs, id), journal.EventsFile)
	switch _, err := os.Stat(events); {
	case err == nil:
		return fmt.Errorf("a session %q already exists in this directory; --session-id names a new session, "+
			"use --resume to continue one", id)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("engine: checking for an existing session at %s: %w", events, err)
	}
	return nil
}
