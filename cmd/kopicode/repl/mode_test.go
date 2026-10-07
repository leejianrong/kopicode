package repl_test

import (
	"context"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

func runModeLines(t *testing.T, input string, mc repl.ModeControl) (string, int) {
	t.Helper()
	var out strings.Builder
	turns := 0
	loop, err := repl.New(repl.Config{
		In: strings.NewReader(input), Out: &out, Mode: mc,
		Turn: func(context.Context, string, repl.Surface) (engine.Result, error) {
			turns++
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return out.String(), turns
}

func TestSlashModeSwitchesAndNeverReachesTheModel(t *testing.T) {
	auto := false
	mc := repl.ModeControl{
		Auto: func() bool { return auto },
		Set:  func(on bool) bool { auto = on; return true },
	}
	out, turns := runModeLines(t, "/mode\n/mode auto\n/MODE  Default\n/mode plan\n/exit\n", mc)
	if turns != 0 {
		t.Errorf("/mode ran %d turns, want 0", turns)
	}
	for _, want := range []string{
		"mode: default (/mode default or /mode auto",
		"mode: auto. auto mode runs shell commands",
		"mode: default. You will be asked",
		`unknown mode "plan": the modes are default and auto`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if auto {
		t.Error("the session was left in auto after /mode default")
	}
}

func TestSlashModeWithoutASessionToSwitchSaysSo(t *testing.T) {
	out, _ := runModeLines(t, "/mode auto\n", repl.ModeControl{})
	if !strings.Contains(out, "/mode is not available") {
		t.Errorf("output = %q", out)
	}
	out, _ = runModeLines(t, "/mode auto\n", repl.ModeControl{Auto: func() bool { return false }, Set: func(bool) bool { return false }})
	if !strings.Contains(out, "cannot switch to auto mode") {
		t.Errorf("a refused switch must say so: %q", out)
	}
}
