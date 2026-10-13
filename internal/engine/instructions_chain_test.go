package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	harnesscfg "github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
)

// ADR-0032: the AGENTS.md chain, its byte cap, and what replay does with a
// dropped file.

// nestedRepo makes a repository with a .git entry at its root and a
// services/api subdirectory, and returns both.
func nestedRepo(t *testing.T) (root, nested string) {
	t.Helper()
	root = t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested = filepath.Join(root, "services", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, nested
}

// TestInstructionsChainLoadsEveryFileNearestLast is ADR-0032 decisions 1 to 3:
// a session started in a subdirectory gets the repository root's file and the
// nested one, one event each, the nearest last, and a file above the repository
// root is never read.
func TestInstructionsChainLoadsEveryFileNearestLast(t *testing.T) {
	outer := t.TempDir()
	root := filepath.Join(outer, "repo")
	nested := filepath.Join(root, "services", "api")
	for _, d := range []string{filepath.Join(root, ".git"), nested} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeAgentsFile(t, outer, "ABOVE the repository: must never load\n")
	writeAgentsFile(t, root, "ROOT rule\n")
	writeAgentsFile(t, nested, "NESTED rule\n")
	ctx := context.Background()

	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s, err := engine.Open(ctx, engine.Options{
		Dir: nested, Selection: resumeSelection(t), SessionID: "chain", Provider: prov, Now: fixedClock(),
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

	got := payloadsOf[journal.ProjectInstructionsLoaded](t, readJournal(t, s.Path()))
	if len(got) != 2 || got[0].Content.Inline != "ROOT rule\n" || got[1].Content.Inline != "NESTED rule\n" {
		t.Fatalf("events = %+v, want the root's file then the nested one", got)
	}
	sent := prov.requests[0].Messages
	r, n, h := indexOfContent(sent, "ROOT rule"), indexOfContent(sent, "NESTED rule"), indexOfContent(sent, "hello")
	if r < 0 || r >= n || n >= h {
		t.Errorf("message order root=%d nested=%d prompt=%d, want root < nested < prompt", r, n, h)
	}
	if indexOfContent(sent, "ABOVE") >= 0 {
		t.Error("a file above the repository root reached the model")
	}
	files := s.Instructions()
	if len(files) != 2 || !files[0].Loaded || !files[1].Loaded || files[1].Bytes != len("NESTED rule\n") {
		t.Errorf("Instructions() = %+v", files)
	}
}

// TestInstructionsOverTheCapDropTheFurthestWhole is ADR-0032 decisions 4 and 6,
// and the replay half: the root file is dropped whole and journalled as dropped,
// the model never sees it, and a resumed session does not either.
func TestInstructionsOverTheCapDropTheFurthestWhole(t *testing.T) {
	root, nested := nestedRepo(t)
	big := strings.Repeat("x", harnesscfg.AgentsCapBytes) + "\n"
	writeAgentsFile(t, root, big)
	writeAgentsFile(t, nested, "NESTED rule\n")
	ctx := context.Background()
	sel := resumeSelection(t)

	first := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s, err := engine.Open(ctx, engine.Options{
		Dir: nested, Selection: sel, SessionID: "cap", Provider: first, Now: fixedClock(),
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

	got := payloadsOf[journal.ProjectInstructionsLoaded](t, readJournal(t, s.Path()))
	if len(got) != 2 || !got[0].Dropped || got[0].Content.Inline != "" || got[0].Bytes != int64(len(big)) || got[1].Dropped {
		t.Fatalf("events = %+v, want the root dropped with its size and no content, the nested one loaded", got)
	}
	if indexOfContent(first.requests[0].Messages, "xxxx") >= 0 {
		t.Error("a dropped file reached the model")
	}
	if files := s.Instructions(); len(files) != 2 || files[0].Loaded || !files[1].Loaded || files[0].Bytes != len(big) {
		t.Errorf("Instructions() = %+v", files)
	}

	second := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "again")}}
	s2, err := engine.Open(ctx, engine.Options{
		Dir: nested, Selection: sel, SessionID: "cap", Resume: true, Provider: second, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("resume Open: %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()
	if _, err := s2.Run(ctx, "and now"); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	sent := second.requests[0].Messages
	if indexOfContent(sent, "xxxx") >= 0 || indexOfContent(sent, "NESTED rule") < 0 {
		t.Errorf("resumed history = %+v, want the nested file and not the dropped one", sent)
	}
}

// TestALoneOversizedNearestFileLoadsWhole: the nearest file is the person's own
// instruction, so it is never dropped or cut, even over the cap on its own.
func TestALoneOversizedNearestFileLoadsWhole(t *testing.T) {
	dir := writeAgentsFile(t, t.TempDir(), strings.Repeat("y", 2*harnesscfg.AgentsCapBytes))
	ctx := context.Background()
	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s, err := engine.Open(ctx, engine.Options{
		Dir: dir, Selection: resumeSelection(t), SessionID: "lone", Provider: prov, Now: fixedClock(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close(ctx) }()
	got := sole[journal.ProjectInstructionsLoaded](t, readJournal(t, s.Path()))
	if got.Dropped || len(got.Content.Inline) != 2*harnesscfg.AgentsCapBytes {
		t.Errorf("event = dropped %v, %d bytes, want the whole file loaded", got.Dropped, len(got.Content.Inline))
	}
}
