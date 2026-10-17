//go:build unix

package engine_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	kh "github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/tools"
)

// ADR-0036, KOP-204: the post_tool hook. It runs after a call that changed the
// tree, cannot undo it, and what it says goes to the model as a note after the
// result. Its failure is shown, never swallowed.

func postHooks(t *testing.T, body string) kh.Hooks {
	return kh.Hooks{PostTool: hookScript(t, body)}
}

func TestPostToolOutputIsAddedAfterTheResult(t *testing.T) {
	h, spy := runHooked(t, postHooks(t, `echo "gofmt: reformatted out.txt"`), []scriptedReply{writeCall("c1", "out.txt")})

	if !exists(h.root, "out.txt") {
		t.Fatal("the call did not run")
	}
	evs := h.events()
	ran := sole[journal.HookRan](t, evs)
	if ran.Point != "post_tool" || ran.Outcome != "passed" || ran.Tool != "write_file" || ran.CallID != "c1" {
		t.Errorf("HookRan = %+v", ran)
	}

	// What the model sees: the result, then the note. What the journal holds as
	// the result: the tool's own output and nothing else.
	res := sole[journal.ToolResult](t, evs)
	if strings.Contains(res.Output.Inline, "gofmt") {
		t.Errorf("the hook's note leaked into the ToolResult: %q", res.Output.Inline)
	}
	msg := toolMessage(t, spy.requests[1], "c1")
	if !strings.HasPrefix(msg, res.Output.Inline) || !strings.HasSuffix(msg, "Note from the post_tool hook:\ngofmt: reformatted out.txt") {
		t.Errorf("the model was shown %q after a result of %q", msg, res.Output.Inline)
	}
}

func TestPostToolWithNoOutputAddsNothing(t *testing.T) {
	h, spy := runHooked(t, postHooks(t, `exit 0`), []scriptedReply{writeCall("c1", "out.txt")})
	res := sole[journal.ToolResult](t, h.events())
	if msg := toolMessage(t, spy.requests[1], "c1"); msg != res.Output.Inline {
		t.Errorf("a silent hook changed what the model saw: %q vs %q", msg, res.Output.Inline)
	}
	if ran := sole[journal.HookRan](t, h.events()); ran.Outcome != "passed" {
		t.Errorf("HookRan = %+v", ran)
	}
}

func TestAFailingPostToolIsShownAndDoesNotUndoTheCall(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"exit 1":    {`echo "lint: 3 problems" >&2; exit 1`, "exited with status 1"},
		"exit 2":    {`echo "no" >&2; exit 2`, "exited with status 2"},
		"unstarted": {``, "could not be started"},
		"signalled": {`kill -9 $$`, "killed by"},
	} {
		t.Run(name, func(t *testing.T) {
			hooks := postHooks(t, tc.body)
			if name == "unstarted" {
				hooks = kh.Hooks{PostTool: []string{"/nonexistent/kopicode-hook"}}
			}
			h, spy := runHooked(t, hooks, []scriptedReply{writeCall("c1", "out.txt")})
			if !exists(h.root, "out.txt") {
				t.Fatal("a failing post_tool hook undid, or prevented, the call")
			}
			ran := sole[journal.HookRan](t, h.events())
			if ran.Outcome != "failed" {
				t.Errorf("outcome = %q: a post_tool hook has nothing to deny, so it is recorded as failed", ran.Outcome)
			}
			msg := toolMessage(t, spy.requests[1], "c1")
			if !strings.Contains(msg, "post_tool hook failed") || !strings.Contains(msg, tc.want) || !strings.Contains(msg, "not undone") {
				t.Errorf("the model was shown %q", msg)
			}
			if name == "exit 1" && !strings.Contains(msg, "lint: 3 problems") {
				t.Errorf("the failing hook's own output is missing: %q", msg)
			}
		})
	}
}

func TestATimedOutPostToolIsShown(t *testing.T) {
	h, spy := runHooked(t, postHooks(t, `sleep 300 & wait`), []scriptedReply{writeCall("c1", "out.txt")},
		withHookTimeout(300_000_000))
	if !exists(h.root, "out.txt") {
		t.Fatal("the call did not run")
	}
	if ran := sole[journal.HookRan](t, h.events()); ran.Outcome != "timed_out" {
		t.Errorf("HookRan = %+v", ran)
	}
	if msg := toolMessage(t, spy.requests[1], "c1"); !strings.Contains(msg, "timed out") {
		t.Errorf("the model was shown %q", msg)
	}
}

func TestPostToolRunsAfterEveryTreeChangingTool(t *testing.T) {
	files := map[string]string{"old.txt": "x\n", "edit.txt": "alpha\nbeta\n"}
	replies := []scriptedReply{
		writeCall("c-write", "new.txt"),
		shellCall("c-shell", "touch made.txt"),
		{calls: []wireCall{nativeCall("c-del", tools.ToolDeleteFile, `{"path":"old.txt"}`)}},
		{calls: []wireCall{nativeCall("c-fuzzy", tools.ToolEditFileFuzzy, `{"path":"edit.txt","before":"alpha","after":"ALPHA"}`)}},
		{calls: []wireCall{nativeCall("c-read", tools.ToolReadFile, `{"path":"new.txt"}`)}},
		{text: "Done."},
	}
	p := script(t, replies, oneAttemptPerTurn(len(replies)))
	h := newHarness(t, p, files, withMaxTurns(len(replies)+1), withHooks(postHooks(t, `exit 0`)))
	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	var tools []string
	for _, r := range payloadsOf[journal.HookRan](t, h.events()) {
		tools = append(tools, r.Tool)
	}
	want := []string{"write_file", "run_shell", "delete_file", "edit_file_fuzzy"}
	if strings.Join(tools, ",") != strings.Join(want, ",") {
		t.Errorf("post_tool ran after %v, want %v (every tool that changes the tree, and no read)", tools, want)
	}
}

func TestPostToolDoesNotRunForACallThatNeverRanOrFailed(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	hooks := postHooks(t, `echo x >> `+marker)

	// A consent denial: the call never ran.
	p := script(t, []scriptedReply{shellCall("c1", "touch made.txt"), {text: "Done."}}, oneAttemptPerTurn(2))
	h := newHarness(t, p, nil, withMaxTurns(3), withHooks(hooks))
	h.policy.verdict = permission.VerdictDeny
	if _, err := h.eng.Run(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if lines(t, marker) != 0 {
		t.Error("post_tool ran for a call the consent gate refused")
	}

	// A pre_tool denial: the call never ran either.
	pre := kh.Hooks{PreTool: hookScript(t, `exit 2`), PostTool: hooks.PostTool}
	runHooked(t, pre, []scriptedReply{writeCall("c1", "a.txt")})
	if lines(t, marker) != 0 {
		t.Error("post_tool ran for a call a pre_tool hook refused")
	}

	// An edit whose anchors do not match a real file: it failed, and changed nothing.
	edit := []scriptedReply{{calls: []wireCall{
		nativeCall("c2", tools.ToolEditFile, `{"path":"real.txt","anchor_start":"1#zzz","anchor_end":"1#zzz","new_text":"x"}`)}}, {text: "Done."}}
	p2 := script(t, edit, oneAttemptPerTurn(len(edit)))
	h2 := newHarness(t, p2, map[string]string{"real.txt": "one\ntwo\n"}, withMaxTurns(3), withHooks(hooks))
	if _, err := h2.eng.Run(t.Context(), "go"); err != nil {
		t.Fatal(err)
	}
	if len(payloadsOf[journal.EditRejected](t, h2.events())) != 1 {
		t.Fatal("test setup: the edit was meant to be rejected on its anchors, not to fail some other way")
	}
	if lines(t, marker) != 0 {
		t.Errorf("post_tool ran for an edit that failed: %+v", payloadsOf[journal.HookRan](t, h2.events()))
	}
}

func TestHookToolsScopesThePostToolHook(t *testing.T) {
	hooks := kh.Hooks{PostTool: hookScript(t, `exit 0`), Tools: []string{"run_shell"}}
	h, _ := runHooked(t, hooks, []scriptedReply{writeCall("c1", "a.txt"), shellCall("c2", "true")})
	ran := payloadsOf[journal.HookRan](t, h.events())
	if len(ran) != 1 || ran[0].Tool != "run_shell" {
		t.Errorf("hook runs = %+v, want one, for run_shell only", ran)
	}
}

func TestALongPostToolNoteIsCappedForTheModelAndWholeInTheRecord(t *testing.T) {
	body := `i=0; while [ $i -lt 2000 ]; do echo "problem $i found by the linter, with a long description"; i=$((i+1)); done`
	h, spy := runHooked(t, postHooks(t, body), []scriptedReply{writeCall("c1", "a.txt")},
		func(c *engine.Config) { c.Selection.Config.OutputCap = 4096 })

	ran := sole[journal.HookRan](t, h.events())
	if ran.Stdout.Size < 100*1024 || int64(len(ran.Stdout.Inline)) != ran.Stdout.Size {
		t.Fatalf("the record does not hold the whole output: %d inline of %d", len(ran.Stdout.Inline), ran.Stdout.Size)
	}
	msg := toolMessage(t, spy.requests[1], "c1")
	if len(msg) > 4096*2+512 || !strings.Contains(msg, "omitted") || !strings.Contains(msg, "problem 0 ") || !strings.Contains(msg, "problem 1999 ") {
		t.Errorf("the model was shown %d bytes; want a capped note with its start and end", len(msg))
	}
}

func TestResumeShowsThePostToolNoteAgainWithoutRunningTheHook(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	sel := resumeSelection(t)
	sel.Config.Hooks = kh.Hooks{PostTool: hookScript(t, `echo x >> `+marker+`; echo "formatted"`)}
	dir := t.TempDir()
	ctx := context.Background()

	first := &fakeReplyProvider{bodies: [][]byte{callBody(t, writeCall("c1", "a.txt").calls[0]), proseBody(t, "Done.")}}
	s1, err := engine.Open(ctx, engine.Options{Dir: dir, Selection: sel, SessionID: "post-resume", Provider: first, Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Run(ctx, "write it"); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatal(err)
	}

	second := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "Still done.")}}
	s2, err := engine.Open(ctx, engine.Options{Dir: dir, Selection: sel, SessionID: "post-resume", Resume: true, Provider: second, Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(ctx) }()
	if _, err := s2.Run(ctx, "anything else?"); err != nil {
		t.Fatal(err)
	}

	var saw string
	for _, m := range second.requests[0].Messages {
		if m.Role == provider.RoleTool && m.ToolCallID == "c1" {
			saw = m.Content
		}
	}
	if !strings.HasSuffix(saw, "Note from the post_tool hook:\nformatted") {
		t.Errorf("the resumed conversation has the result as %q; the model saw the note the first time", saw)
	}
	if got := lines(t, marker); got != 1 {
		t.Errorf("resuming reran the post_tool hook: %d runs, want 1", got)
	}
}
