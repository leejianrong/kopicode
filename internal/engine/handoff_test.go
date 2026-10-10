package engine_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

const narrativeReply = "## Goal\n\nShip the thing.\n\n## Done\n\n- wrote it\n\n## Next step\n\nrun the tests\n"

func openForHandoff(t *testing.T, dir, id string, prov *fakeReplyProvider, mutate func(*engine.Options)) *engine.Session {
	t.Helper()
	t.Setenv("KOPICODE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	opts := engine.Options{Dir: dir, Selection: resumeSelection(t), SessionID: id, Provider: prov, Now: fixedClock()}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := engine.Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestHandoffDraftsADocumentWithoutTouchingTheConversation(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeReplyProvider{bodies: [][]byte{
		proseBody(t, "first answer"), proseBody(t, narrativeReply), proseBody(t, "second answer"),
	}}
	s := openForHandoff(t, dir, "parent", prov, nil)
	ctx := context.Background()
	if _, err := s.Run(ctx, "do the thing"); err != nil {
		t.Fatal(err)
	}

	res, err := s.Handoff(ctx, "finish it")
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	for _, want := range []string{"Ship the thing.", "run the tests", "## Recorded by kopicode"} {
		if !strings.Contains(res.Document, want) {
			t.Errorf("document lacks %q:\n%s", want, res.Document)
		}
	}
	if got, _ := os.ReadFile(res.Path); string(got) != res.Document {
		t.Errorf("the projection file differs from the document")
	}
	if res.Path != engine.HandoffPath(dir, "parent") {
		t.Errorf("path = %s", res.Path)
	}

	// The handoff call is tool-less and asks for the narrative.
	req := prov.requests[1]
	if len(req.Tools) != 0 {
		t.Errorf("the handoff request offered %d tools, want none", len(req.Tools))
	}
	if last := req.Messages[len(req.Messages)-1]; !strings.Contains(last.Content, "## Goal") || !strings.Contains(last.Content, "finish it") {
		t.Errorf("the last message is not the handoff prompt: %q", last.Content)
	}

	// The session carries on exactly as it was: neither the prompt nor the reply
	// is in the history the next turn sends.
	if _, err := s.Run(ctx, "and then?"); err != nil {
		t.Fatal(err)
	}
	for _, m := range prov.requests[2].Messages {
		if strings.Contains(m.Content, "## Goal") || strings.Contains(m.Content, "Ship the thing.") {
			t.Fatalf("the handoff leaked into the conversation: %q", m.Content)
		}
	}
	_ = s.Close(ctx)

	written := payloadsOf[journal.HandoffWritten](t, readJournal(t, s.Path()))
	if len(written) != 1 || written[0].Goal != "finish it" || written[0].Path != res.Path || written[0].Document.Inline != res.Document {
		t.Fatalf("HandoffWritten = %+v", written)
	}
}

func TestHandoffBeforeAnyTurnIsRefused(t *testing.T) {
	s := openForHandoff(t, t.TempDir(), "empty", &fakeReplyProvider{}, nil)
	defer func() { _ = s.Close(context.Background()) }()
	if _, err := s.Handoff(context.Background(), ""); !errors.Is(err, engine.ErrNothingToHand) {
		t.Fatalf("err = %v, want ErrNothingToHand", err)
	}
}

func TestANewSessionStartsFromAHandoffAndSaysWhere(t *testing.T) {
	dir := t.TempDir()
	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "picked up")}}
	doc := "# Handoff\n\n## Goal\n\nShip the thing.\n"
	s := openForHandoff(t, dir, "child", prov, func(o *engine.Options) {
		o.Handoff, o.ParentSession = doc, "parent"
	})
	if _, err := s.Run(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close(context.Background())

	idx := indexOfContent(prov.requests[0].Messages, "<handoff>\n"+strings.TrimSpace(doc))
	if idx < 0 {
		t.Fatalf("the model was not given the handoff: %+v", prov.requests[0].Messages)
	}
	if idx >= len(prov.requests[0].Messages)-1 {
		t.Errorf("the handoff must come before the user's first message")
	}

	evs := readJournal(t, s.Path())
	loaded := payloadsOf[journal.ProjectInstructionsLoaded](t, evs)
	if len(loaded) != 1 || loaded[0].Scope != journal.InstructionsScopeHandoff {
		t.Fatalf("loaded = %+v, want one handoff-scope event", loaded)
	}
	started := payloadsOf[journal.SessionStarted](t, evs)
	if len(started) != 1 || started[0].ParentSession != "parent" {
		t.Fatalf("SessionStarted = %+v, want parent_session = parent", started)
	}
}

func TestASessionWithoutAHandoffRecordsNoParent(t *testing.T) {
	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s := openForHandoff(t, t.TempDir(), "plain", prov, func(o *engine.Options) { o.ParentSession = "ignored" })
	if _, err := s.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close(context.Background())
	started := payloadsOf[journal.SessionStarted](t, readJournal(t, s.Path()))
	if started[0].ParentSession != "" {
		t.Errorf("parent_session = %q on a session with no handoff", started[0].ParentSession)
	}
}
