package bench

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/corpus"
)

// writeTaskTree lays out a task directory with a repo/ tree and, optionally, a
// pristine test copy, and returns the repo/ path (what the runner grades in).
func writeTaskTree(t *testing.T, repoFiles, pristineFiles map[string]string) string {
	t.Helper()
	taskDir := t.TempDir()

	write := func(base string, files map[string]string) {
		for rel, content := range files {
			p := filepath.Join(base, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatalf("mkdir for %s: %v", rel, err)
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatalf("write %s: %v", rel, err)
			}
		}
	}

	repoDir := filepath.Join(taskDir, corpus.RepoDirName)
	write(repoDir, repoFiles)
	if pristineFiles != nil {
		write(filepath.Join(taskDir, corpus.PristineTestsDirName), pristineFiles)
	}
	return repoDir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestRestorePristineTestsOverwritesMutatedTest is the core of the test-integrity
// guarantee: an agent-edited test file is replaced by the pristine copy before
// grading.
func TestRestorePristineTestsOverwritesMutatedTest(t *testing.T) {
	repoDir := writeTaskTree(t,
		map[string]string{
			"solution.go":   "package x\n// unfixed\n",
			"x_test.go":     "package x\n// AGENT EDITED THIS TO PASS TRIVIALLY\n",
			"sub/y_test.go": "package y\n// also edited\n",
		},
		map[string]string{
			"x_test.go":     "package x\n// the real, failing test\n",
			"sub/y_test.go": "package y\n// the real nested test\n",
		},
	)

	if err := restorePristineTests(repoDir); err != nil {
		t.Fatalf("restorePristineTests: %v", err)
	}

	if got := readFile(t, filepath.Join(repoDir, "x_test.go")); got != "package x\n// the real, failing test\n" {
		t.Errorf("x_test.go was not restored to pristine:\n%s", got)
	}
	if got := readFile(t, filepath.Join(repoDir, "sub", "y_test.go")); got != "package y\n// the real nested test\n" {
		t.Errorf("nested test was not restored to pristine:\n%s", got)
	}
	// A non-test file the agent changed must be left exactly as the session left
	// it — restore touches only what the pristine copy names.
	if got := readFile(t, filepath.Join(repoDir, "solution.go")); got != "package x\n// unfixed\n" {
		t.Errorf("restore clobbered a non-test file:\n%s", got)
	}
}

// TestRestorePristineTestsNoopWhenAbsent proves the change is inert for a task
// that ships no pristine copy — an opt-out this synthetic fixture exercises
// directly, even though no task in the real corpora takes it (KAN-1434).
func TestRestorePristineTestsNoopWhenAbsent(t *testing.T) {
	repoDir := writeTaskTree(t,
		map[string]string{"x_test.go": "package x\n// left alone\n"},
		nil,
	)

	if err := restorePristineTests(repoDir); err != nil {
		t.Fatalf("restorePristineTests should be a no-op, got: %v", err)
	}
	if got := readFile(t, filepath.Join(repoDir, "x_test.go")); got != "package x\n// left alone\n" {
		t.Errorf("no-op restore changed a file:\n%s", got)
	}
}

// TestRestorePristineTestsEmptyDirErrors treats a present-but-empty pristine
// directory as a generation bug, not an opt-out: it must fail loudly rather than
// silently grade an unprotected tree.
func TestRestorePristineTestsEmptyDirErrors(t *testing.T) {
	repoDir := writeTaskTree(t,
		map[string]string{"x_test.go": "package x\n"},
		nil,
	)
	// Create an empty pristine directory alongside repo/.
	if err := os.MkdirAll(filepath.Join(filepath.Dir(repoDir), corpus.PristineTestsDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := restorePristineTests(repoDir); err == nil {
		t.Fatal("restorePristineTests accepted an empty pristine directory")
	}
}
