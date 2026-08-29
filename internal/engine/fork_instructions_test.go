package engine_test

import (
	"context"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// TestForkCopiesTheSourcesProjectInstructions is KAN-1025's fork half of the
// same acceptance criterion resume_test.go's TestResumeReplaysTheOriginal-
// ProjectInstructions holds for Options.Resume: a forked session must carry
// the *source's* originally recorded AGENTS.md content, not a fresh read of
// the file (which loadProjectInstructions never even attempts on this path —
// see instructions.go's own doc comment on why) and not nothing at all (the
// gap this file's own readForkSource fix in fork.go closes: Turn 0 is
// otherwise excluded from what a fork copies).
func TestForkCopiesTheSourcesProjectInstructions(t *testing.T) {
	dir := newForkFixtureRepo(t)
	original := "Run `make test` before every commit.\n"
	writeAgentsFile(t, dir, original)

	openTrivialSession(t, dir, "source") // one prose-only turn: reaches turn 1

	// The file changes on disk after the source session ran and before the
	// fork. A fork that re-read it would see this text instead.
	writeAgentsFile(t, dir, "CHANGED: this must never reach the forked request\n")

	ctx := context.Background()
	forkProv := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "Forked and done.")}}
	forked, err := engine.Fork(ctx, engine.Options{
		Dir: dir, SessionID: "forked", Selection: resumeSelection(t), Provider: forkProv, Now: fixedClock(),
		Fork: &engine.ForkSource{SessionID: "source", Turn: 1},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Cleanup(func() { _ = forked.Close(context.Background()) })

	if _, err := forked.Run(ctx, "continue"); err != nil {
		t.Fatalf("running the forked session: %v", err)
	}

	if len(forkProv.requests) == 0 {
		t.Fatal("the forked session never called its provider")
	}
	sent := forkProv.requests[0].Messages

	want := engine.WrapProjectInstructionsForTest(original)
	idx := indexOfContent(sent, original)
	if idx < 0 || sent[idx].Content != want {
		t.Fatalf("the forked session's first request does not carry the source's original AGENTS.md "+
			"content; messages: %+v", sent)
	}
	if indexOfContent(sent, "CHANGED") >= 0 {
		t.Errorf("the forked session's first request carries the file's post-fork edit, "+
			"not the source's copied recorded content; messages: %+v", sent)
	}

	got := payloadsOf[journal.ProjectInstructionsLoaded](t, readJournal(t, forked.Path()))
	if len(got) != 1 {
		t.Errorf("forked journal carries %d ProjectInstructionsLoaded events, want exactly 1 "+
			"(copied from the source, never rediscovered)", len(got))
	}
	if got[0].Content.Inline != original {
		t.Errorf("the forked journal's own copy holds %q, want the source's original %q",
			got[0].Content.Inline, original)
	}
}
