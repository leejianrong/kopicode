package corpus_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/aidergen"
	"github.com/leejianrong/kopicode/internal/corpus"
)

// TestAiderCorpusLoads is the fast acceptance check for KAN-1406: the frozen
// Go+Python aider corpus loads under its own composition policy, digest and all.
//
// It runs in the fast suite because it only reads and hashes files — no oracle,
// no toolchain. The fails-before/passes-after guarantee is checked separately,
// behind the integration tag (aider_oracle_integration_test.go), the same split
// the hand-authored corpus uses.
func TestAiderCorpusLoads(t *testing.T) {
	root := aiderCorpusRoot(t)

	c, err := corpus.LoadWithPolicy(root, aidergen.FrozenComposition())
	if err != nil {
		t.Fatalf("loading the aider corpus: %v", err)
	}

	// The corpus is generated with a Load already inside the generator, so a
	// failure here means the committed tree drifted from the recorded digest —
	// which Load reports — or the policy no longer matches what was frozen.
	var goCount, pyCount int
	for _, task := range c.Tasks {
		switch task.Language {
		case "go":
			goCount++
		case "python":
			pyCount++
		default:
			t.Errorf("task %s has unexpected language %q", task.ID, task.Language)
		}
		if !task.HasTrait(corpus.TraitRequiresRead) {
			t.Errorf("task %s is not marked %s; every aider exercise reads before it edits",
				task.ID, corpus.TraitRequiresRead)
		}
	}
	if goCount == 0 || pyCount == 0 {
		t.Errorf("corpus is not stratified: %d Go, %d Python", goCount, pyCount)
	}
	t.Logf("loaded aider corpus %s %s: %d Go, %d Python", c.Version, c.Digest, goCount, pyCount)
}

// TestAiderSolutionsOnlyOverlayExistingFiles keeps the reference solutions honest:
// a solution file must have a counterpart in the starting tree, so overlaying it
// is a fix rather than a second copy of the task. It is cheap (a stat walk) so it
// stays in the fast suite.
func TestAiderSolutionsOnlyOverlayExistingFiles(t *testing.T) {
	root := aiderCorpusRoot(t)
	c, err := corpus.LoadWithPolicy(root, aidergen.FrozenComposition())
	if err != nil {
		t.Fatalf("loading the aider corpus: %v", err)
	}

	for _, task := range c.Tasks {
		dir := corpus.SolutionDir(c.Root, task.ID)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(task.RepoDir(), rel)); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			t.Errorf("%s: solution file has no counterpart in the starting tree: %v", task.ID, err)
		}
	}
}

// aiderCorpusRoot resolves the frozen aider corpus directory.
func aiderCorpusRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "bench", "aider-go-python"))
	if err != nil {
		t.Fatalf("resolving the aider corpus root: %v", err)
	}
	return root
}
