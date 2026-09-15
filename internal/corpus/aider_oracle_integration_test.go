//go:build integration

package corpus_test

import (
	"testing"

	"github.com/leejianrong/kopicode/internal/aidergen"
	"github.com/leejianrong/kopicode/internal/corpus"
)

// TestAiderOracleFailsBeforeAndPassesAfter is the frozen aider corpus's honesty
// check, the same one TestOracleFailsBeforeAndPassesAfter applies to the
// hand-authored corpus: every task's oracle must fail on the starting tree and
// pass once the reference solution is overlaid.
//
// The generator (tools/aidergen) already applies this as its admission filter,
// so a freshly generated corpus passes by construction. Re-running it here in CI
// guards against the two ways that guarantee can rot after freezing: a task file
// edited by hand (also caught by the digest, but this says *why* it matters), and
// a toolchain change that alters what the oracle concludes.
//
// It reuses runOracle, copyTree and requireToolchain from
// oracle_integration_test.go — same package, same build tag.
func TestAiderOracleFailsBeforeAndPassesAfter(t *testing.T) {
	root := aiderCorpusRoot(t)

	c, err := corpus.LoadWithPolicy(root, aidergen.FrozenComposition())
	if err != nil {
		t.Fatalf("loading the aider corpus: %v", err)
	}
	t.Logf("checking %d aider oracles in both directions, corpus %s %s",
		len(c.Tasks), c.Version, c.Digest)

	for _, task := range c.Tasks {
		t.Run(task.ID, func(t *testing.T) {
			t.Parallel()
			requireToolchain(t, task)

			work := t.TempDir()
			copyTree(t, task.RepoDir(), work)

			code, output := runOracle(t, task, work)
			if code == 0 {
				t.Fatalf("the oracle passes on the unfixed tree, so this task measures nothing\n%s", output)
			}

			copyTree(t, corpus.SolutionDir(c.Root, task.ID), work)

			code, output = runOracle(t, task, work)
			if code != 0 {
				t.Fatalf("the oracle still fails after the reference solution (exit %d)\n%s", code, output)
			}
		})
	}
}
