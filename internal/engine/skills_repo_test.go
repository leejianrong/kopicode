package engine_test

import (
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// TestRepoSkillsIsTheRepositorysOwnAndNothingPersonal: what a non-interactive
// session is told about is the skills inside its root. A personal skill in the
// user's own directory is discovered for the REPL's /skills but never reaches
// an orchestrator's session (ADR-0024 decision 4).
func TestRepoSkillsIsTheRepositorysOwnAndNothingPersonal(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("KOPICODE_HOME", home)
	t.Setenv("HOME", t.TempDir())
	writeSkill(t, filepath.Join(dir, ".agents", "skills"), "release", "cut a release", "x")
	writeSkill(t, filepath.Join(home, "skills"), "personal", "my own habit", "x")

	if all, _ := engine.DiscoverSkills(dir, nil); len(all) != 2 {
		t.Fatalf("setup: DiscoverSkills found %d skills, want the repository's and the personal one", len(all))
	}
	got := engine.RepoSkills(dir)
	if len(got) != 1 || got[0].Name != "release" {
		t.Errorf("RepoSkills = %+v, want only the repository's release skill", got)
	}
	if none := engine.RepoSkills(t.TempDir()); len(none) != 0 {
		t.Errorf("a directory with no skills gave %+v", none)
	}
}
