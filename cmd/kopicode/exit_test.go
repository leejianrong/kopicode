package main

import (
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// TestTheSurfacesExitCodesAreTheEngines holds exit.go to engine.Stop.ExitCode.
//
// The two are written down separately on purpose — the engine cannot know
// about a usage error, which happens before an engine exists (ADR-0007
// decision 4) — and separately written down is exactly how two tables stop
// agreeing. This is the check that they have not.
func TestTheSurfacesExitCodesAreTheEngines(t *testing.T) {
	want := map[engine.Stop]int{
		engine.StopCompleted:       exitSuccess,
		engine.StopBudgetExhausted: exitNotCompleted,
		engine.StopCancelled:       exitNotCompleted,
		engine.StopProviderError:   exitProvider,
		engine.StopMaxTurns:        exitHarness,
		engine.StopHarnessError:    exitHarness,
		// The fail-closed default: an outcome nobody mapped is a harness
		// defect, and reporting success for it is how a broken loop passes a
		// smoke test.
		engine.StopUnspecified: exitHarness,
	}

	for stop, code := range want {
		if got := stop.ExitCode(); got != code {
			t.Errorf("%v.ExitCode() = %d, want %d", stop, got, code)
		}
	}
}

// TestUsageIsNotReachableFromTheEngine. Exit 2 means "nothing was opened,
// locked or written", which is only true of a failure that happened before the
// engine was built. A Stop that mapped onto it would be claiming that about a
// session that had already started.
func TestUsageIsNotReachableFromTheEngine(t *testing.T) {
	stops := []engine.Stop{
		engine.StopUnspecified, engine.StopCompleted, engine.StopMaxTurns,
		engine.StopBudgetExhausted, engine.StopCancelled, engine.StopProviderError,
		engine.StopHarnessError,
	}
	for _, stop := range stops {
		if stop.ExitCode() == exitUsage {
			t.Errorf("%v maps to the usage exit code, which means nothing was written — "+
				"but the engine had already started (ADR-0007 decision 4)", stop)
		}
	}
}

// TestAnUnknownCommandIsAUsageError, and it names the ones that exist. A
// subcommand dispatcher whose failure does not list the alternatives sends the
// user to the source.
func TestAnUnknownCommandIsAUsageError(t *testing.T) {
	var stdout, stderr strings.Builder

	if code := run([]string{"summarise"}, &stdout, &stderr); code != exitUsage {
		t.Errorf("exit code = %d, want %d. stderr:\n%s", code, exitUsage, stderr.String())
	}
	for _, want := range []string{"summarise", "repl", "run", "version"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, stderr.String())
		}
	}
	if stdout.String() != "" {
		t.Errorf("a usage error wrote to stdout, which --print owns:\n%s", stdout.String())
	}
}

// TestTheDefaultCommandIsTheREPL, and a leading flag does not become one.
func TestTheDefaultCommandIsTheREPL(t *testing.T) {
	tests := []struct {
		args     []string
		wantName string
		wantRest []string
	}{
		{nil, "repl", nil},
		{[]string{"--model", "x"}, "repl", []string{"--model", "x"}},
		{[]string{"-debug"}, "repl", []string{"-debug"}},
		{[]string{"run", "--print"}, "run", []string{"--print"}},
		{[]string{"version"}, "version", []string{}},
		{[]string{"--version"}, "version", []string{}},
	}

	for _, tc := range tests {
		name, rest := command(tc.args)
		if name != tc.wantName {
			t.Errorf("command(%v) name = %q, want %q", tc.args, name, tc.wantName)
		}
		if len(rest) != len(tc.wantRest) {
			t.Errorf("command(%v) rest = %v, want %v", tc.args, rest, tc.wantRest)
			continue
		}
		for i := range rest {
			if rest[i] != tc.wantRest[i] {
				t.Errorf("command(%v) rest = %v, want %v", tc.args, rest, tc.wantRest)
				break
			}
		}
	}
}

// TestVersionGoesToStdout. It is the one thing this binary prints that a
// script reads.
func TestVersionGoesToStdout(t *testing.T) {
	var stdout, stderr strings.Builder

	if code := run([]string{"--version"}, &stdout, &stderr); code != exitSuccess {
		t.Errorf("exit code = %d, want %d", code, exitSuccess)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Errorf("--version printed nothing to stdout; stderr was:\n%s", stderr.String())
	}
}
