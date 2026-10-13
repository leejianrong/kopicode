package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// TestRunPrintTellsANewSessionWhichSkillsTheRepositoryShips is KOP-159: the
// catalogue the REPL gives a new session reaches `run --print` too, through the
// real entry point, so a headless caller is not left to remember to list_dir.
func TestRunPrintTellsANewSessionWhichSkillsTheRepositoryShips(t *testing.T) {
	srv := scriptedProvider(t, sseBody("done"))
	t.Setenv(engine.APIKeyEnv, "kopicode-test-credential")
	t.Setenv("KOPICODE_HOME", t.TempDir())

	dir := t.TempDir()
	skill := filepath.Join(dir, ".agents", "skills", "release")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: release\ndescription: cut a release\n---\nsteps\n"
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var stdout, stderr strings.Builder
	code := runPrint([]string{"--print", "--provider-url", srv.URL, "--model", "qwen/qwen3-coder-next", "say done"}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("exit %d\nstderr: %s\nstdout: %s", code, stderr.String(), stdout.String())
	}
	if j := findJournal(t, dir); !strings.Contains(j, "- release: cut a release (.agents/skills/release/SKILL.md)") {
		t.Errorf("the journal holds no skills catalogue:\n%s", j)
	}
}
