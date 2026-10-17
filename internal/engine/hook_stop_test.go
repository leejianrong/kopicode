//go:build unix

package engine_test

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
	kh "github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
)

// ADR-0036, KOP-205: the stop gate. It runs when the model finishes without a
// tool call, after forced verification. A failure refuses the stop and feeds the
// hook's output back; three refusals in a row end the session with
// stop_gate_failed, which is never a success.

// stopHook is a stop hook that refuses its first failFirst runs, with output the
// model can be shown, and passes after that. A counter file outside the tree
// carries the count between runs.
func stopHook(t *testing.T, failFirst int) kh.Hooks {
	t.Helper()
	counter := filepath.Join(t.TempDir(), "count")
	return kh.Hooks{Stop: hookScript(t, `
n=$(cat `+counter+` 2>/dev/null || echo 0); n=$((n+1)); echo $n > `+counter+`
if [ $n -le `+strconv.Itoa(failFirst)+` ]; then echo "lint still failing (run $n)" >&2; exit 1; fi
echo "lint clean"`)}
}

func proseReplies(n int) []scriptedReply {
	out := make([]scriptedReply, n)
	for i := range out {
		out[i] = scriptedReply{text: "All done, " + strconv.Itoa(i+1) + "."}
	}
	return out
}

func stopHarness(t *testing.T, hooks kh.Hooks, replies []scriptedReply, opts ...harnessOption) (*harness, *recordingProvider) {
	t.Helper()
	p := script(t, replies, oneAttemptPerTurn(len(replies)))
	spy := &recordingProvider{inner: p}
	opts = append([]harnessOption{withMaxTurns(len(replies) + 1), withProvider(spy), withHooks(hooks)}, opts...)
	return newHarness(t, p, nil, opts...), spy
}

func stopRuns(t *testing.T, h *harness) []journal.HookRan {
	t.Helper()
	var out []journal.HookRan
	for _, r := range payloadsOf[journal.HookRan](t, h.events()) {
		if r.Point == "stop" {
			out = append(out, r)
		}
	}
	return out
}

func TestAPassingStopHookLetsTheSessionComplete(t *testing.T) {
	h, _ := stopHarness(t, stopHook(t, 0), proseReplies(1))
	res, err := h.eng.Run(t.Context(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != engine.StopCompleted || res.Turns != 1 {
		t.Errorf("result = %+v, want completed in 1 turn", res)
	}
	runs := stopRuns(t, h)
	if len(runs) != 1 || runs[0].Outcome != "passed" || runs[0].Tool != "" || runs[0].CallID != "" || runs[0].Stdout.Inline != "lint clean\n" {
		t.Errorf("HookRan = %+v", runs)
	}
}

func TestARefusedStopFeedsTheOutputBackAndTheModelCanFinishAgain(t *testing.T) {
	h, spy := stopHarness(t, stopHook(t, 1), proseReplies(2))
	res, err := h.eng.Run(t.Context(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != engine.StopCompleted || res.Turns != 2 {
		t.Errorf("result = %+v, want completed on the second try", res)
	}
	runs := stopRuns(t, h)
	if len(runs) != 2 || runs[0].Outcome != "failed" || runs[0].ExitCode != 1 || runs[1].Outcome != "passed" {
		t.Fatalf("HookRan = %+v", runs)
	}
	// The second request carries the refusal, with the hook's own words, as a user
	// turn after the model's first "done".
	var saw string
	for _, m := range spy.requests[1].Messages {
		if m.Role == provider.RoleUser && strings.Contains(m.Content, "stop hook refused") {
			saw = m.Content
		}
	}
	if !strings.Contains(saw, "lint still failing (run 1)") || !strings.Contains(saw, "finish again") {
		t.Errorf("the model was told %q", saw)
	}
}

func TestStopRefusalsAreBoundedAtThreeAndNeverReportSuccess(t *testing.T) {
	h, spy := stopHarness(t, stopHook(t, 100), proseReplies(5))
	res, err := h.eng.Run(t.Context(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != engine.StopGateFailed {
		t.Fatalf("Stop = %s, want stop_gate_failed", res.Stop)
	}
	if res.Stop == engine.StopCompleted || res.Stop.ExitCode() != 1 {
		t.Errorf("exit code = %d: a refused stop must never be a success", res.Stop.ExitCode())
	}
	if res.Turns != 3 || len(spy.requests) != 3 {
		t.Errorf("turns = %d, provider calls = %d, want exactly 3 of each", res.Turns, len(spy.requests))
	}
	if got := len(stopRuns(t, h)); got != 3 {
		t.Errorf("the hook ran %d times, want 3", got)
	}

	if err := h.eng.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	ended := sole[journal.SessionEnded](t, h.events())
	if ended.Reason != "stop_gate_failed" || ended.ExitCode != 1 ||
		!strings.Contains(ended.Detail.Inline, "3 times in a row") || !strings.Contains(ended.Detail.Inline, "hook.sh") {
		t.Errorf("SessionEnded = %+v", ended)
	}
	if res.Detail != ended.Detail.Inline {
		t.Errorf("Result.Detail = %q, SessionEnded.Detail = %q", res.Detail, ended.Detail.Inline)
	}
}

func TestACleanStopHasNoResultDetail(t *testing.T) {
	h, _ := stopHarness(t, stopHook(t, 0), proseReplies(1))
	res, err := h.eng.Run(t.Context(), "go")
	if err != nil || res.Stop != engine.StopCompleted {
		t.Fatalf("Run = %+v, %v", res, err)
	}
	if res.Detail != "" {
		t.Errorf("Result.Detail = %q for a completed stop", res.Detail)
	}
}

func TestTheRefusalCountIsPerExchangeAndAPassResetsIt(t *testing.T) {
	// Two refusals then the turn cap, twice over: the second exchange starts at
	// zero, so it is not ended by a third refusal the first exchange left behind.
	h, _ := stopHarness(t, stopHook(t, 100), proseReplies(4), withMaxTurns(2))
	for i := 1; i <= 2; i++ {
		res, err := h.eng.Run(t.Context(), "go")
		if err != nil {
			t.Fatal(err)
		}
		if res.Stop != engine.StopMaxTurns {
			t.Fatalf("exchange %d: Stop = %s, want max_turns after two refusals", i, res.Stop)
		}
	}
	if got := len(stopRuns(t, h)); got != 4 {
		t.Errorf("the hook ran %d times, want 4", got)
	}

	// A pass resets the count: fail, pass.
	h2, _ := stopHarness(t, stopHook(t, 2), proseReplies(3))
	if res, _ := h2.eng.Run(t.Context(), "go"); res.Stop != engine.StopCompleted {
		t.Errorf("fail, fail, pass ended %s, want completed", res.Stop)
	}
}

func TestAStopHookThatErrorsRefusesTheStop(t *testing.T) {
	for name, tc := range map[string]struct {
		hooks kh.Hooks
		want  string
		opts  []harnessOption
	}{
		"exit 2":    {kh.Hooks{Stop: hookScript(t, `echo "no" >&2; exit 2`)}, "exited with status 2", nil},
		"unstarted": {kh.Hooks{Stop: []string{"/nonexistent/kopicode-hook"}}, "could not be started", nil},
		"signalled": {kh.Hooks{Stop: hookScript(t, `kill -9 $$`)}, "killed by", nil},
		"timeout":   {kh.Hooks{Stop: hookScript(t, `sleep 300 & wait`)}, "timed out", []harnessOption{withHookTimeout(200 * time.Millisecond)}},
	} {
		t.Run(name, func(t *testing.T) {
			h, spy := stopHarness(t, tc.hooks, proseReplies(3), tc.opts...)
			res, err := h.eng.Run(t.Context(), "go")
			if err != nil {
				t.Fatal(err)
			}
			if res.Stop != engine.StopGateFailed {
				t.Errorf("Stop = %s: a broken gate must refuse, not wave the stop through", res.Stop)
			}
			var told bool
			for _, m := range spy.requests[1].Messages {
				told = told || (m.Role == provider.RoleUser && strings.Contains(m.Content, tc.want))
			}
			if !told {
				t.Errorf("the model was not told %q", tc.want)
			}
		})
	}
}

func TestTheStopHookOnlyRunsWhenTheModelStopsAndNotOnToolTurns(t *testing.T) {
	replies := []scriptedReply{writeCall("c1", "a.txt"), {text: "Done."}}
	h, _ := stopHarness(t, stopHook(t, 0), replies)
	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if got := len(stopRuns(t, h)); got != 1 {
		t.Errorf("the stop hook ran %d times over a tool turn and a stop, want 1", got)
	}
}

// Forced verification comes first (docs/SLICE-1.md §5, ADR-0036 decision 1).
func TestAStopHookRunsAfterVerificationNotInsteadOfIt(t *testing.T) {
	h, _ := stopHarness(t, stopHook(t, 0), writeThenStop(), withHelperVerification(t, "pass"))
	res, err := h.eng.Run(t.Context(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("Stop = %s", res.Stop)
	}
	vAt, hAt := -1, -1
	for i, ev := range h.events() {
		switch p := ev.Payload.(type) {
		case journal.VerificationRun:
			vAt = i
		case journal.HookRan:
			if p.Point == "stop" {
				hAt = i
			}
		}
	}
	if vAt < 0 || hAt < 0 || vAt > hAt {
		t.Errorf("verification at event %d, stop hook at %d: the gate must follow verification", vAt, hAt)
	}
}

func TestAStopHookIsNotConsultedOverATreeVerificationRejected(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	hooks := kh.Hooks{Stop: hookScript(t, `echo x >> `+marker+`; exit 0`)}
	h, _ := stopHarness(t, hooks, writeThenStop(), withHelperVerification(t, "fail"))
	res, err := h.eng.Run(t.Context(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop != engine.StopVerificationFailed {
		t.Errorf("Stop = %s, want verification_failed: a passing stop hook cannot launder a rejected tree", res.Stop)
	}
	if lines(t, marker) != 0 {
		t.Error("the stop hook ran over a tree verification had rejected")
	}
}

func TestACancelledStopHookIsNotAPassAndIsNotLeftRunning(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	hooks := kh.Hooks{Stop: hookScript(t, `sleep 300 & echo $! > `+pidFile+`; wait`)}
	h, _ := stopHarness(t, hooks, proseReplies(2))

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for !exists(filepath.Dir(pidFile), "pid") {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	res, _ := h.eng.Run(ctx, "go")
	if res.Stop != engine.StopCancelled {
		t.Errorf("Stop = %s, want cancelled", res.Stop)
	}
	if c := sole[journal.TurnCancelled](t, h.events()); c.Phase != "stop_hook" {
		t.Errorf("TurnCancelled phase = %q, want stop_hook", c.Phase)
	}
}

func TestResumeShowsARefusedStopAgainWithoutRunningTheHook(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	sel := resumeSelection(t)
	sel.Config.Hooks = kh.Hooks{Stop: hookScript(t, `echo x >> `+marker+`; echo "lint failing" >&2; exit 1`)}
	dir := t.TempDir()
	ctx := context.Background()

	first := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "done 1"), proseBody(t, "done 2"), proseBody(t, "done 3")}}
	s1, err := engine.Open(ctx, engine.Options{Dir: dir, Selection: sel, SessionID: "stop-resume", Provider: first, Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := s1.Run(ctx, "finish up"); err != nil || res.Stop != engine.StopGateFailed {
		t.Fatalf("first session: %v, %v", res, err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, marker); got != 3 {
		t.Fatalf("the hook ran %d times, want 3", got)
	}

	// Resumed with no stop hook declared: nothing may run, and the model's history
	// still holds the three refusals it was given.
	sel2 := resumeSelection(t)
	second := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s2, err := engine.Open(ctx, engine.Options{Dir: dir, Selection: sel2, SessionID: "stop-resume", Resume: true, Provider: second, Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(ctx) }()
	if _, err := s2.Run(ctx, "what happened?"); err != nil {
		t.Fatal(err)
	}
	refusals := 0
	for _, m := range second.requests[0].Messages {
		if m.Role == provider.RoleUser && strings.Contains(m.Content, "lint failing") {
			refusals++
		}
	}
	if refusals != 3 {
		t.Errorf("the resumed history holds %d refusals, want 3", refusals)
	}
	if got := lines(t, marker); got != 3 {
		t.Errorf("resuming reran the stop hook: %d runs, want 3", got)
	}
}
