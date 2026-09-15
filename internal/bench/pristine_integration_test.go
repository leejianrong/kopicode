//go:build integration

package bench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/corpus"
)

// TestPristineRestoreDefeatsTestTampering is the acceptance test for KAN-1407: a
// model that rewrites the test file to pass trivially is still graded against the
// real suite, because the grader restores the pristine test first.
//
// It builds a minimal Python task by hand, runs the real oracle three times, and
// asserts the sequence that matters: the honest tree fails, a tampered tree
// would pass (proving the tamper is effective, so the guard is not testing
// nothing), and the guard turns that false pass back into the true failure. It
// uses the same runOracle the runner uses, so it exercises the real grading path.
func TestPristineRestoreDefeatsTestTampering(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH; this test grades a Python task")
	}

	const stub = "def add(a, b):\n    return 0  # deliberately wrong\n"
	const realTest = `import unittest
from solution import add


class AddTest(unittest.TestCase):
    def test_add(self):
        self.assertEqual(add(2, 3), 5)


if __name__ == "__main__":
    unittest.main()
`
	const tamperedTest = `import unittest


class AddTest(unittest.TestCase):
    def test_add(self):
        self.assertTrue(True)


if __name__ == "__main__":
    unittest.main()
`

	taskDir := t.TempDir()
	repoDir := filepath.Join(taskDir, corpus.RepoDirName)
	pristineDir := filepath.Join(taskDir, corpus.PristineTestsDirName)
	writeAll(t, repoDir, map[string]string{
		"solution.py":      stub,
		"solution_test.py": realTest,
	})
	writeAll(t, pristineDir, map[string]string{
		"solution_test.py": realTest,
	})

	task := corpus.Task{
		ID:       "pristine-accept",
		Language: "python",
		Requires: []string{"python3"},
		Oracle: corpus.Oracle{
			Argv:           []string{"python3", "-m", "unittest", "solution_test"},
			TimeoutSeconds: 30,
		},
	}

	grade := func() OracleResult {
		return runOracle(context.Background(), task, repoDir, t.TempDir(), goCaches{}, time.Now)
	}

	// 1. The honest starting tree fails: the stub is wrong.
	if res := grade(); res.Err != nil || res.Passed {
		t.Fatalf("honest tree should fail; passed=%v err=%v\n%s", res.Passed, res.Err, res.Output)
	}

	// 2. The agent tampers with the test so it passes trivially. Absent the
	//    guard, this is a false pass — which is exactly the hole KAN-1407 closes.
	writeAll(t, repoDir, map[string]string{"solution_test.py": tamperedTest})
	if res := grade(); res.Err != nil || !res.Passed {
		t.Fatalf("tampered tree should pass (proving the tamper works); passed=%v err=%v\n%s",
			res.Passed, res.Err, res.Output)
	}

	// 3. Restore the pristine test, then grade: the false pass becomes the true
	//    failure. This is the guarantee.
	if err := restorePristineTests(repoDir); err != nil {
		t.Fatalf("restorePristineTests: %v", err)
	}
	if res := grade(); res.Err != nil || res.Passed {
		t.Fatalf("after restore the tampered tree should fail again; passed=%v err=%v\n%s",
			res.Passed, res.Err, res.Output)
	}
}

func writeAll(t *testing.T, base string, files map[string]string) {
	t.Helper()
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
