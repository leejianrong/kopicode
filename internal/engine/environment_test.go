package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

func environmentEvents(t *testing.T, evs []journal.Event) []journal.ProjectInstructionsLoaded {
	t.Helper()
	var out []journal.ProjectInstructionsLoaded
	for _, p := range payloadsOf[journal.ProjectInstructionsLoaded](t, evs) {
		if p.Scope == journal.InstructionsScopeEnvironment {
			out = append(out, p)
		}
	}
	return out
}

// TestTheModelIsToldTheRepositoryRootOnce is KOP-216: a model that is not told
// where run_shell starts guesses a path from whatever benchmark it was tuned on
// (`cd /testbed`, the first shell command in 3 of 6 driven sessions). The note
// is journalled once, before the user's first message, and shown to the model.
func TestTheModelIsToldTheRepositoryRootOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KOPICODE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok"), proseBody(t, "ok again")}}
	s, err := engine.Open(context.Background(), engine.Options{
		Dir: dir, Selection: resumeSelection(t), SessionID: "env", Provider: prov, Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"please fix pagination", "and again"} {
		if _, err := s.Run(context.Background(), prompt); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close(context.Background())

	evs := readJournal(t, s.Path())
	got := environmentEvents(t, evs)
	if len(got) != 1 {
		t.Fatalf("got %d environment events across two exchanges, want 1: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Content.Inline, dir) {
		t.Errorf("the note %q does not name the root %q", got[0].Content.Inline, dir)
	}
	if evs[1].Type() != journal.TypeProjectInstructionsLoaded || evs[1].Turn != 0 {
		t.Errorf("event 2 is %s at turn %d, want the environment note right after SessionStarted", evs[1].Type(), evs[1].Turn)
	}

	sent := prov.requests[0].Messages
	note := indexOfContent(sent, got[0].Content.Inline)
	user := indexOfContent(sent, "please fix pagination")
	if note < 0 || user < 0 || note > user {
		t.Errorf("the model was not shown the note before the task (note at %d, task at %d): %+v", note, user, sent)
	}
}

// TestAResumedSessionKeepsItsEnvironmentNote: the note is replayed from the
// journal and not written a second time.
func TestAResumedSessionKeepsItsEnvironmentNote(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KOPICODE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	sel := resumeSelection(t)

	first := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s1, err := engine.Open(ctx, engine.Options{Dir: dir, Selection: sel, SessionID: "env-resume", Provider: first, Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Run(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	_ = s1.Close(ctx)

	second := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s2, err := engine.Open(ctx, engine.Options{Dir: dir, Selection: sel, SessionID: "env-resume", Resume: true, Provider: second, Now: fixedClock()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close(ctx) }()
	if _, err := s2.Run(ctx, "again"); err != nil {
		t.Fatal(err)
	}

	if n := len(environmentEvents(t, readJournal(t, s2.Path()))); n != 1 {
		t.Errorf("journaled %d environment events across the original run and its resume, want 1", n)
	}
	if indexOfContent(second.requests[0].Messages, "Environment: the repository root is "+dir) < 0 {
		t.Errorf("the resumed request does not carry the note: %+v", second.requests[0].Messages)
	}
}
