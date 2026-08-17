package engine_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/lock"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// docs/SLICE-1.md §8 at the engine's front door.
//
// The front ends' version of this lives in cmd/kopicode/frontends_integration_test.go
// and drives two real processes, because an exit code and a stderr message are
// only observable from outside. What is checked here is the half that is not:
// that the refusal happens inside Open, before the journal exists, and that the
// tree is released again when the session closes.

// TestASecondSessionInOneWorkingTreeIsRefused is the requirement itself.
func TestASecondSessionInOneWorkingTreeIsRefused(t *testing.T) {
	requireLockSupport(t)

	dir := lockDir(t)
	first := openLocked(t, dir, "first")
	t.Cleanup(func() { _ = first.Close(context.Background()) })

	_, err := engine.Open(t.Context(), lockOptions(t, dir, "second"))
	if err == nil {
		t.Fatal("a second session opened in a working tree that already had one")
	}
	if !errors.Is(err, engine.ErrSessionLocked) {
		t.Fatalf("the refusal is not ErrSessionLocked, so a front end cannot recognise it: %v", err)
	}
	if !strings.Contains(err.Error(), first.ID()) {
		t.Errorf("the refusal does not name the holder's session, which is how a user "+
			"finds the record that is in the way:\n%v", err)
	}
	if !strings.Contains(err.Error(), first.Path()) {
		t.Errorf("the refusal does not point at the holder's record:\n%v", err)
	}
}

// TestARefusedSessionWritesNoRecord is ADR-0007 decision 4's ordering, applied
// to the lock.
//
// The refusal has to land before the journal is opened, or a session that never
// started leaves a directory under .kopicode/sessions/ that a reader cannot
// tell from a session that ran and did nothing.
func TestARefusedSessionWritesNoRecord(t *testing.T) {
	requireLockSupport(t)

	dir := lockDir(t)
	first := openLocked(t, dir, "first")
	t.Cleanup(func() { _ = first.Close(context.Background()) })

	before := treeUnder(t, dir)
	if _, err := engine.Open(t.Context(), lockOptions(t, dir, "second")); err == nil {
		t.Fatal("the second Open succeeded")
	}
	after := treeUnder(t, dir)

	if !sameTree(before, after) {
		t.Errorf("a refused session changed the working tree.\nbefore: %v\nafter:  %v", before, after)
	}
	if _, err := os.Stat(journal.SessionDir(dir, "second")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused session has a record directory at %s; the refusal must come "+
			"before the journal is opened", journal.SessionDir(dir, "second"))
	}
}

// TestClosingASessionFreesTheWorkingTree is the other half: the lock is a
// session-length resource, not a process-length one. The bench runner runs ten
// sessions in one process and a lock that outlived its session would be a leak
// that only shows up under load.
func TestClosingASessionFreesTheWorkingTree(t *testing.T) {
	requireLockSupport(t)

	dir := lockDir(t)
	first := openLocked(t, dir, "first")
	if err := first.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := engine.Open(t.Context(), lockOptions(t, dir, "second"))
	if err != nil {
		t.Fatalf("the working tree was still locked after the first session closed: %v", err)
	}
	if err := second.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestTheLockLivesAtTheDocumentedPath pins the location, because it is a
// user-facing path: docs/SLICE-1.md §8 names it and a refusal message points at
// it.
func TestTheLockLivesAtTheDocumentedPath(t *testing.T) {
	dir := lockDir(t)
	s := openLocked(t, dir, "only")
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	path := filepath.Join(dir, ".kopicode", "lock")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !strings.Contains(string(body), s.ID()) {
		t.Errorf("%s does not describe the running session:\n%s", path, body)
	}
	if path != filepath.Join(dir, lock.StateDir, lock.FileName) {
		t.Fatal("the literal path in this test and internal/lock's constants disagree")
	}
}

// openLocked opens a session with a distinct id, failing the test if it cannot.
func openLocked(t *testing.T, dir, id string) *engine.Session {
	t.Helper()

	s, err := engine.Open(t.Context(), lockOptions(t, dir, id))
	if err != nil {
		t.Fatalf("Open(%s): %v", id, err)
	}
	return s
}

// lockOptions is a session in dir, driven by the replay provider so that no
// traffic and no credential is involved.
func lockOptions(t *testing.T, dir, id string) engine.Options {
	t.Helper()

	prov, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading the fixture: %v", err)
	}
	return engine.Options{
		Dir:       dir,
		SessionID: id,
		Selection: testSelection(prov),
		Provider:  prov,
		Now:       fixedClock(),
	}
}

// lockDir is a working tree with no git in it.
//
// No git on purpose, for internal/engine's stated reason: repo owns the git
// side and asserts its own containment, and a git subprocess here would be a
// second place to get it wrong. It also exercises the fallback in
// repo.WorkTreeRoot — a directory outside any repository is a working tree of
// one directory, and a session there still excludes a second.
func lockDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	write(t, dir, "internal/greet/greet.go", greetGo)
	return dir
}

func treeUnder(t *testing.T, dir string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

func sameTree(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// requireLockSupport skips the cases that assert exclusion on a platform where
// there is none. See internal/lock/lock_other.go.
func requireLockSupport(t *testing.T) {
	t.Helper()
	if !lock.Supported {
		t.Skip("the session lock is a no-op on this platform (docs/SLICE-1.md §8)")
	}
}
