package bench

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/leejianrong/kopicode/internal/corpus"
)

// restorePristineTests overwrites the test files in a task's working tree with
// the pristine copies frozen alongside it, immediately before the oracle grades
// it. It is the structural half of test-file integrity
// (docs/aider-polyglot-integration-scoping.md §4): the oracle runs the
// agent-mutated tree, and nothing stops a model from editing the test file to
// pass trivially, so the grader restores the original tests first.
//
// repoDir is the task's starting tree, [corpus.RepoDirName] inside its task
// directory. The pristine copy is its sibling, [corpus.PristineTestsDirName],
// keyed by repo-relative path — laid down by the aidergen generator (KAN-1406)
// from each exercise's own upstream manifest, or by hand for bench/tasks
// (KAN-1434) from each task's declared test_files — and, in a bench run,
// checked out from the frozen commit into the same worktree, so it is as
// immutable as the corpus itself.
//
// A task that ships no pristine copy is left untouched: the function is a
// no-op, so this changes nothing for a corpus that does not opt in. Every task
// in bench/tasks now opts in (KAN-1434); see KAN-1407 for the gap this closed.
func restorePristineTests(repoDir string) error {
	pristine := filepath.Join(filepath.Dir(repoDir), corpus.PristineTestsDirName)

	info, err := os.Stat(pristine)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bench: locating pristine tests at %s: %w", pristine, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("bench: %s exists but is not a directory", pristine)
	}

	restored := 0
	err = filepath.WalkDir(pristine, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(pristine, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(repoDir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return err
		}
		restored++
		return nil
	})
	if err != nil {
		return fmt.Errorf("bench: restoring pristine tests from %s: %w", pristine, err)
	}
	if restored == 0 {
		// A present-but-empty pristine directory is a generation bug, not a task
		// that opted out: fail loudly rather than grade a tree whose tests were
		// never protected.
		return fmt.Errorf("bench: %s is empty: no pristine test to restore", pristine)
	}
	return nil
}
