package repl_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

// skillDir makes a user-path skills directory the engine's own discovery reads.
func skillFunc(t *testing.T, names ...string) repl.SkillsFunc {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KOPICODE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for _, n := range names {
		writeSkillIn(t, dir, n, "does "+n, "BODY OF "+n)
	}
	return func() ([]engine.Skill, []string) { return engine.DiscoverSkills(t.TempDir(), []string{dir}) }
}

func runSkillLines(t *testing.T, input string, sk repl.SkillsFunc, interactive bool) (out string, prompts []string) {
	t.Helper()
	var w strings.Builder
	cfg := repl.Config{
		In: strings.NewReader(input), Out: &w, Skills: sk,
		Turn: func(_ context.Context, p string, _ repl.Surface) (engine.Result, error) {
			prompts = append(prompts, p)
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	}
	if interactive {
		cfg.Terminal, cfg.Interactive = fakeTerminal{}, true
	}
	loop, err := repl.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w.String(), prompts
}

func TestSlashSkillsListsThemWithoutATurn(t *testing.T) {
	out, prompts := runSkillLines(t, "/skills\n", skillFunc(t, "deploy", "mode"), false)
	if len(prompts) != 0 {
		t.Errorf("/skills ran %d turns", len(prompts))
	}
	for _, want := range []string{"/deploy  does deploy", "/mode (cannot be invoked: the name is a built-in command)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestSlashSkillRunsATurnWithTheBodyAndTheTask(t *testing.T) {
	_, prompts := runSkillLines(t, "/deploy to staging\n", skillFunc(t, "deploy"), false)
	if len(prompts) != 1 || !strings.Contains(prompts[0], "BODY OF deploy") || !strings.Contains(prompts[0], "Task: to staging") {
		t.Fatalf("prompts = %q", prompts)
	}
}

func TestABuiltinWinsOverASkillOfTheSameName(t *testing.T) {
	_, prompts := runSkillLines(t, "/mode\n", skillFunc(t, "mode"), false)
	if len(prompts) != 0 {
		t.Errorf("/mode ran a skill turn: %q", prompts)
	}
}

func TestUnknownSlashAndOrdinaryTextGoToTheModelAsTyped(t *testing.T) {
	_, prompts := runSkillLines(t, "/nothing here\nfix the bug\n", skillFunc(t, "deploy"), false)
	if len(prompts) != 2 || prompts[0] != "/nothing here" || prompts[1] != "fix the bug" {
		t.Errorf("prompts = %q", prompts)
	}
}

func TestTabCompletesSkillsAndCommands(t *testing.T) {
	_, prompts := runSkillLines(t, "/dep\t\r", skillFunc(t, "deploy"), true)
	if len(prompts) != 1 || !strings.Contains(prompts[0], "Follow the skill \"deploy\"") {
		t.Errorf("Tab did not complete /dep to the deploy skill: %q", prompts)
	}
	out, _ := runSkillLines(t, "/s\t\r\x04", skillFunc(t), true)
	if !strings.Contains(out, "/skills") {
		t.Errorf("Tab did not offer /skills: %q", out)
	}
}

func TestSkillsAreRediscoveredEachTime(t *testing.T) {
	calls := 0
	sk := func() ([]engine.Skill, []string) { calls++; return nil, nil }
	runSkillLines(t, "/skills\n/skills\n", sk, false)
	if calls != 2 {
		t.Errorf("discovery ran %d times, want once per /skills", calls)
	}
}

func writeSkillIn(t *testing.T, root, name, desc, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(root, name, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
