package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	harnesscfg "github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
)

// This file is KAN-1025's end-to-end proof of KAN-1024's design: a genuinely
// new session's bootstrap finds the repository's own AGENTS.md, journals it
// once, and feeds it to the model before the first real user turn — and a
// resumed session sees the *same* content again, replayed from the journal
// rather than re-read from disk (see instructions.go and resume.go's new
// switch case).
//
// It shares resume_test.go's fakeReplyProvider/resumeSelection/proseBody
// machinery rather than the mock/fixture harness: none of what this file
// checks depends on a recorded provider exchange, and a git repository is
// only needed for the fork half, in fork_instructions_test.go.

// writeAgentsFile puts an AGENTS.md at dir's root and returns dir.
func writeAgentsFile(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, harnesscfg.AgentsFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", harnesscfg.AgentsFileName, err)
	}
	return dir
}

// TestProjectInstructionsLoadedOnFreshSession is the bootstrap half: a
// session opened against a repository with an AGENTS.md journals it once and
// carries it into the very first provider request, ahead of the human's own
// prompt.
func TestProjectInstructionsLoadedOnFreshSession(t *testing.T) {
	content := "Run `make test` before every commit.\n"
	dir := writeAgentsFile(t, t.TempDir(), content)
	sel := resumeSelection(t)
	ctx := context.Background()

	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "agents-fresh", Provider: prov, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Run(ctx, "hello"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(prov.requests) == 0 {
		t.Fatal("the session never called its provider")
	}
	sent := prov.requests[0].Messages
	agentsIdx := indexOfContent(sent, content)
	if agentsIdx < 0 {
		t.Fatalf("the first request does not carry the AGENTS.md content; messages: %+v", sent)
	}
	want := engine.WrapProjectInstructionsForTest(content)
	if sent[agentsIdx].Role != provider.RoleUser || sent[agentsIdx].Content != want {
		t.Errorf("message at %d = %+v, want a user turn with content %q", agentsIdx, sent[agentsIdx], want)
	}
	if helloIdx := indexOfContent(sent, "hello"); agentsIdx >= helloIdx {
		t.Errorf("the AGENTS.md turn (at %d) does not precede the human's own prompt (at %d)", agentsIdx, helloIdx)
	}

	events := readJournal(t, s.Path())
	loaded := sole[journal.ProjectInstructionsLoaded](t, events)
	if loaded.Content.Inline != content {
		t.Errorf("journaled content = %q, want %q", loaded.Content.Inline, content)
	}
	if !strings.HasSuffix(loaded.Path, harnesscfg.AgentsFileName) {
		t.Errorf("journaled path = %q, want it to name %s", loaded.Path, harnesscfg.AgentsFileName)
	}
	if got, want := types(events)[:2], []journal.Type{journal.TypeSessionStarted, journal.TypeProjectInstructionsLoaded}; !slices.Equal(got, want) {
		t.Errorf("event order = %v, want %v — the bootstrap must land before the human's own turn", got, want)
	}
}

// TestProjectInstructionsLoadedIsNotDuplicatedByRun holds the "once" half of
// the design to account: a session that runs more than one exchange must not
// re-discover or re-journal the file on a later turn.
func TestProjectInstructionsLoadedIsNotDuplicatedByRun(t *testing.T) {
	dir := writeAgentsFile(t, t.TempDir(), "one instruction\n")
	sel := resumeSelection(t)
	ctx := context.Background()

	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "first"), proseBody(t, "second")}}
	s, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "agents-twice", Provider: prov, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Run(ctx, "first prompt"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := s.Run(ctx, "second prompt"); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readJournal(t, s.Path())
	got := payloadsOf[journal.ProjectInstructionsLoaded](t, events)
	if len(got) != 1 {
		t.Errorf("journaled %d ProjectInstructionsLoaded events across two exchanges, want exactly 1", len(got))
	}
}

// TestResumeReplaysTheOriginalProjectInstructions is KAN-1025's acceptance
// criterion in full: a session opened, run once and closed; the file on disk
// then changed; the same session id resumed. The resumed session's first
// request must carry the *original* content, because it came from the
// journal's own recorded Content, not from a fresh read of AGENTS.md.
func TestResumeReplaysTheOriginalProjectInstructions(t *testing.T) {
	original := "Run `make test` before every commit.\n"
	dir := writeAgentsFile(t, t.TempDir(), original)
	sel := resumeSelection(t)
	ctx := context.Background()

	first := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "Hi, ready to help.")}}
	s1, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "agents-resume", Provider: first, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s1.Run(ctx, "hello"); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := s1.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// The file changes on disk between the original run and the resume. A
	// resume that re-read it would see this text instead.
	changed := writeAgentsFile(t, dir, "CHANGED: this must never reach the resumed request\n")
	if changed != dir {
		t.Fatal("writeAgentsFile changed the directory it wrote to")
	}

	second := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "You said hello.")}}
	s2, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "agents-resume", Resume: true, Provider: second, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("resuming Open: %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()

	if _, err := s2.Run(ctx, "what did I just say?"); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}

	if len(second.requests) == 0 {
		t.Fatal("the resumed session never called its provider")
	}
	sent := second.requests[0].Messages

	want := engine.WrapProjectInstructionsForTest(original)
	idx := indexOfContent(sent, original)
	if idx < 0 || sent[idx].Content != want {
		t.Fatalf("the resumed session's first request does not carry the original AGENTS.md content; "+
			"messages: %+v", sent)
	}
	if indexOfContent(sent, "CHANGED") >= 0 {
		t.Errorf("the resumed session's first request carries the file's post-close edit, "+
			"not the original recorded content; messages: %+v", sent)
	}

	// The resumed session's own bootstrap must not have re-discovered the file
	// and journaled a second copy.
	got := payloadsOf[journal.ProjectInstructionsLoaded](t, readJournal(t, s2.Path()))
	if len(got) != 1 {
		t.Errorf("journaled %d ProjectInstructionsLoaded events across the original run and its resume, want exactly 1", len(got))
	}
}
