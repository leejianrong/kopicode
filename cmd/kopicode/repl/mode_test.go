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

func TestBudgetExhaustedExplainsItself(t *testing.T) {
	var out strings.Builder
	loop, err := repl.New(repl.Config{
		In: strings.NewReader("go on\n"), Out: &out,
		Turn: func(context.Context, string, repl.Surface) (engine.Result, error) {
			return engine.Result{Stop: engine.StopBudgetExhausted}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[stopped] budget_exhausted", "cannot continue", "--token-budget"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestAVerificationThatNeverRanIsNotShownAsAFailure(t *testing.T) {
	var out strings.Builder
	loop, err := repl.New(repl.Config{
		In: strings.NewReader("hi\n"), Out: &out,
		Turn: func(_ context.Context, _ string, s repl.Surface) (engine.Result, error) {
			s.Render(engine.Event{Kind: engine.EventVerification, ExitCode: -1})
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "exit -1") || !strings.Contains(out.String(), "skipped: no verification command") {
		t.Errorf("output:\n%s", out.String())
	}
}

// KOP-209: the two stops that return a nil error carry their reason in the
// result, and the stopped line shows it.
func runStopped(t *testing.T, res engine.Result) string {
	t.Helper()
	var out strings.Builder
	loop, err := repl.New(repl.Config{
		In: strings.NewReader("go on\n"), Out: &out,
		Turn: func(context.Context, string, repl.Surface) (engine.Result, error) { return res, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestAStoppedLineSaysWhyForVerificationAndTheStopGate(t *testing.T) {
	for _, tc := range []struct {
		stop engine.Stop
		want string
	}{
		{engine.StopVerificationFailed, "[stopped] verification_failed: `go test ./...` exited 1"},
		{engine.StopGateFailed, "[stopped] stop_gate_failed: `make lint` refused the stop 3 times in a row"},
	} {
		detail := tc.want[strings.Index(tc.want, ": ")+2:]
		out := runStopped(t, engine.Result{Stop: tc.stop, Detail: detail})
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: output lacks %q:\n%s", tc.stop, tc.want, out)
		}
	}
}

func TestAStopWithNoDetailKeepsItsBareLine(t *testing.T) {
	out := runStopped(t, engine.Result{Stop: engine.StopGateFailed})
	if !strings.Contains(out, "[stopped] stop_gate_failed") || strings.Contains(out, "stop_gate_failed:") {
		t.Errorf("output = %q", out)
	}
	if out := runStopped(t, engine.Result{Stop: engine.StopCompleted, Detail: "x"}); strings.Contains(out, "[stopped]") {
		t.Errorf("a completed stop printed a stopped line: %q", out)
	}
}
