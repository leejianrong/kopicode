//go:build integration

package corpus_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/corpus"
)

// TestOracleFailsBeforeAndPassesAfter is the check that makes the corpus worth
// anything.
//
// An oracle that already passes on the starting tree measures nothing and
// inflates every arm's pass rate identically, which is the worst kind of
// broken: it looks like a working benchmark. An oracle that fails even after a
// correct fix caps every arm below 100% for a reason that has nothing to do
// with the model. So both directions are asserted here, for every task the
// loader finds, rather than checked once by hand when the task was written.
//
// The "after" tree is produced by overlaying the task's reference solution,
// which lives outside the corpus tree precisely so a run cannot read it.
//
// It is behind the integration tag because it compiles ten small projects and
// runs their suites twice over; `make test` stays the fast inner loop and
// `make test-all` — which is what CI runs — carries this.
func TestOracleFailsBeforeAndPassesAfter(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "bench", "tasks"))
	if err != nil {
		t.Fatalf("resolving the corpus root: %v", err)
	}

	c, err := corpus.Load(root)
	if err != nil {
		t.Fatalf("loading the corpus: %v", err)
	}
	if len(c.Tasks) < corpus.MinTasks {
		t.Fatalf("loaded %d tasks, want at least %d", len(c.Tasks), corpus.MinTasks)
	}
	t.Logf("checking %d oracles in both directions, corpus %s %s",
		len(c.Tasks), c.Version, c.Digest)

	for _, task := range c.Tasks {
		t.Run(task.ID, func(t *testing.T) {
			t.Parallel()
			requireToolchain(t, task)

			work := filepath.Join(t.TempDir(), "repo")
			copyTree(t, task.RepoDir(), work)

			code, output := runOracle(t, task, work)
			if code == 0 {
				t.Fatalf("the oracle passes on the unfixed tree, so this task measures nothing\n%s", output)
			}
			t.Logf("unfixed: exit %d", code)

			copyTree(t, corpus.SolutionDir(c.Root, task.ID), work)

			code, output = runOracle(t, task, work)
			if code != 0 {
				t.Fatalf("the oracle still fails after the reference solution (exit %d)\n%s", code, output)
			}
		})
	}
}

// requireToolchain skips rather than fails when a task's declared requirement
// is missing, and says so loudly. A missing toolchain is an environment fact,
// but a corpus that silently drops tasks breaks the pairing ADR-0005 §1 is
// built on, so the skip message is written to be noticed in a CI log.
func requireToolchain(t *testing.T, task corpus.Task) {
	t.Helper()
	for _, need := range task.Requires {
		if _, err := exec.LookPath(need); err != nil {
			t.Skipf("%s is not on PATH, so task %s cannot be verified here: "+
				"a benchmark run on this machine would be missing a task and its "+
				"results would not be paired with a run that has it", need, task.ID)
		}
	}
}

// runOracle runs the task's oracle in dir and returns its exit code and
// combined output.
//
// The environment is built rather than inherited, for the same reason
// internal/repo builds its git environment: an inherited variable — a
// GOFLAGS, a PYTHONPATH, a GOPROXY pointing somewhere — changes what the
// oracle measures without changing anything visible in the command. Only the
// variables the toolchains genuinely need to find themselves are passed
// through, and the manifest's own environment goes on top.
func runOracle(t *testing.T, task corpus.Task, dir string) (int, string) {
	t.Helper()

	timeout := time.Duration(task.Oracle.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, task.Oracle.Argv[0], task.Oracle.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = oracleEnv(task)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("oracle for %s timed out after %s\n%s", task.ID, timeout, buf.String())
	}

	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, buf.String()
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), buf.String()
	default:
		t.Fatalf("running the oracle for %s: %v\n%s", task.ID, err, buf.String())
		return -1, ""
	}
}

// passThrough are the variables a toolchain needs to find itself and its
// caches. Everything else is dropped.
var passThrough = []string{
	"PATH", "HOME", "TMPDIR", "TEMP", "TMP",
	"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE",
	// Windows needs these to start a process at all.
	"SystemRoot", "USERPROFILE", "LOCALAPPDATA", "APPDATA", "ComSpec",
}

func oracleEnv(task corpus.Task) []string {
	env := make([]string, 0, len(passThrough)+len(task.Oracle.Env))
	for _, name := range passThrough {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	for name, value := range task.Oracle.Env {
		env = append(env, name+"="+value)
	}
	return env
}

// copyTree copies every file under src into dst, creating directories as it
// goes and overwriting what is already there. That overwriting is what makes a
// reference solution an overlay rather than a patch: no patch tool, no
// line-offset arithmetic, and the same behaviour on every platform.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatalf("copying %s to %s: %v", src, dst, err)
	}
}

// TestSolutionsOnlyOverlayExistingFiles keeps the reference solutions honest
// about what they are: a fix to the starting tree, not a second copy of the
// task. It runs here rather than in the fast suite because it is the same
// walk the overlay above does.
func TestSolutionsOnlyOverlayExistingFiles(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "bench", "tasks"))
	if err != nil {
		t.Fatalf("resolving the corpus root: %v", err)
	}
	c, err := corpus.Load(root)
	if err != nil {
		t.Fatalf("loading the corpus: %v", err)
	}

	for _, task := range c.Tasks {
		dir := corpus.SolutionDir(c.Root, task.ID)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(task.RepoDir(), rel)); err != nil {
				return fmt.Errorf("solution file %s has no counterpart in the starting tree",
					filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Errorf("%s: %v", task.ID, err)
		}
	}
}

// TestTaskStatementsDoNotLeakTheFix is a small honesty check on the corpus
// text. A statement that names the exact line to change turns a coding task
// into a transcription task, and the pass rate stops meaning what the report
// says it means.
func TestTaskStatementsDoNotLeakTheFix(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "bench", "tasks"))
	if err != nil {
		t.Fatalf("resolving the corpus root: %v", err)
	}
	c, err := corpus.Load(root)
	if err != nil {
		t.Fatalf("loading the corpus: %v", err)
	}

	for _, task := range c.Tasks {
		if strings.Contains(task.Statement, "```") {
			t.Errorf("%s: the statement holds a fenced code block, which is close to "+
				"handing over the diff", task.ID)
		}
		if lines := strings.Count(task.Statement, "\n"); lines > 6 {
			t.Errorf("%s: the statement is %d lines; a user would type fewer", task.ID, lines+1)
		}
	}
}
