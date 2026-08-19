package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// This file is KAN-941's engine-level test half: [engine.ListSessions] over a
// handful of real sessions opened, run and closed through the same
// engine.Open and engine.Fork this package's other tests already drive, so
// what is asserted is what a caller of ListSessions actually sees rather
// than a shape assumed from the doc comment.
//
// It borrows fakeReplyProvider, proseBody and resumeSelection from
// resume_test.go, and the git fixture helpers (newForkFixtureRepo,
// openTrivialSession, forkSelection) from fork_test.go — all in this same
// test package, so there is no second provider stub or git fixture to keep
// in sync with those files' own.

// TestListSessionsOnADirectoryThatNeverRanASession holds ListSessions to the
// same "not a failure" treatment Open gives a directory that is not a git
// repository: a directory with no .kopicode/sessions at all is zero
// sessions and no error, not something a caller has to special-case.
func TestListSessionsOnADirectoryThatNeverRanASession(t *testing.T) {
	got, err := engine.ListSessions(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("ListSessions on an unused directory: %v", err)
	}
	if got != nil {
		t.Errorf("ListSessions on an unused directory = %#v, want nil", got)
	}
}

// TestListSessionsReportsAnOrdinarySession is the basic case: one session,
// opened, run once and closed cleanly, and every scalar ListSessions
// documents actually came from that session's own record.
func TestListSessionsReportsAnOrdinarySession(t *testing.T) {
	dir := t.TempDir()
	sel := resumeSelection(t)
	ctx := context.Background()

	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "hi there")}}
	s, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "list-me", Provider: prov, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Run(ctx, "please help"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := engine.ListSessions(ctx, dir)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListSessions = %#v, want exactly one summary", got)
	}
	sum := got[0]

	switch {
	case sum.ID != "list-me":
		t.Errorf("ID = %q, want %q", sum.ID, "list-me")
	case sum.ModelID != sel.ModelID:
		t.Errorf("ModelID = %q, want %q", sum.ModelID, sel.ModelID)
	case sum.HarnessConfigHash != sel.HarnessConfigHash:
		t.Errorf("HarnessConfigHash = %q, want %q", sum.HarnessConfigHash, sel.HarnessConfigHash)
	case sum.Turns != 1:
		t.Errorf("Turns = %d, want 1", sum.Turns)
	case !sum.Ended:
		t.Errorf("Ended = false, want true")
	case sum.EndReason != "completed":
		t.Errorf("EndReason = %q, want %q", sum.EndReason, "completed")
	case sum.FirstMessage != "please help":
		t.Errorf("FirstMessage = %q, want %q", sum.FirstMessage, "please help")
	case sum.Forked:
		t.Errorf("Forked = true for an ordinary session")
	case sum.StartedAt.IsZero():
		t.Errorf("StartedAt is zero")
	}
}

// TestListSessionsSkipsAStrayNonSessionDirectory holds the walk in
// ListSessions to the same rule its own doc comment states: a directory
// under sessions/ with no events.jsonl is skipped rather than reported,
// because it is not a session at all.
func TestListSessionsSkipsAStrayNonSessionDirectory(t *testing.T) {
	dir := t.TempDir()
	sel := resumeSelection(t)
	ctx := context.Background()

	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: sel, SessionID: "real-session", Provider: prov, Now: fixedClock(),
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

	strayDir := filepath.Join(dir, ".kopicode", "sessions", "not-a-session")
	if err := os.MkdirAll(strayDir, 0o700); err != nil {
		t.Fatalf("creating a stray directory: %v", err)
	}

	got, err := engine.ListSessions(ctx, dir)
	if err != nil {
		t.Fatalf("ListSessions with a stray directory present: %v", err)
	}
	if len(got) != 1 || got[0].ID != "real-session" {
		t.Errorf("ListSessions = %#v, want exactly the one real session", got)
	}
}

// TestListSessionsOrdersOldestFirst opens two sessions on one shared,
// monotonically advancing clock — so the second Open's SessionStarted is
// unambiguously later than the first's regardless of how many times either
// call reads the clock internally — and checks the summaries come back in
// that order.
func TestListSessionsOrdersOldestFirst(t *testing.T) {
	dir := t.TempDir()
	sel := resumeSelection(t)
	ctx := context.Background()
	clock := fixedClock()

	openAndClose := func(id string) {
		t.Helper()
		prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
		s, err := engine.Open(ctx, engine.Options{
			Dir: dir, Selection: sel, SessionID: id, Provider: prov, Now: clock,
		})
		if err != nil {
			t.Fatalf("Open(%s): %v", id, err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close(%s): %v", id, err)
		}
	}
	openAndClose("first")
	openAndClose("second")

	got, err := engine.ListSessions(ctx, dir)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListSessions = %#v, want two summaries", got)
	}
	if got[0].ID != "first" || got[1].ID != "second" {
		t.Errorf("order = [%s, %s], want [first, second] (oldest first)", got[0].ID, got[1].ID)
	}
	if !got[0].StartedAt.Before(got[1].StartedAt) {
		t.Errorf("StartedAt not strictly increasing: %s then %s", got[0].StartedAt, got[1].StartedAt)
	}
}

// TestListSessionsReportsAForkedSession holds SessionSummary.Forked and its
// two companion fields to the journal.SessionForked marker [Fork] writes —
// checked over a real fork rather than assumed from ListSessions' own
// reading of the payload switch.
func TestListSessionsReportsAForkedSession(t *testing.T) {
	dir := newForkFixtureRepo(t)
	openTrivialSession(t, dir, "source") // one prose-only turn, reaches turn 1

	sel, prov := forkSelection(t)
	forked, err := engine.Fork(t.Context(), engine.Options{
		Dir: dir, SessionID: "forked", Selection: sel, Provider: prov, Now: fixedClock(),
		Fork: &engine.ForkSource{SessionID: "source", Turn: 1},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if err := forked.Close(t.Context()); err != nil {
		t.Fatalf("closing the forked session: %v", err)
	}

	got, err := engine.ListSessions(t.Context(), dir)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var sum *engine.SessionSummary
	for i := range got {
		if got[i].ID == "forked" {
			sum = &got[i]
		}
	}
	if sum == nil {
		t.Fatalf("ListSessions did not report the forked session among %#v", got)
	}
	if !sum.Forked {
		t.Errorf("Forked = false, want true")
	}
	if sum.ForkedFrom != "source" {
		t.Errorf("ForkedFrom = %q, want %q", sum.ForkedFrom, "source")
	}
	if sum.ForkedTurn != 1 {
		t.Errorf("ForkedTurn = %d, want 1", sum.ForkedTurn)
	}
}
