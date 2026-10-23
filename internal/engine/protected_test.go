package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
)

// TestAProtectedPathRefusesTheWriteAndTellsTheModelToReport is ADR-0037 end to
// end through Open: the model's write under tests/ is refused and the file is
// untouched, a write elsewhere in the root still lands, the refusal the model
// reads says to stop and say so, and the record carries what was declared.
func TestAProtectedPathRefusesTheWriteAndTellsTheModelToReport(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KOPICODE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(dir, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "assert True\n"
	if err := os.WriteFile(filepath.Join(dir, "tests", "test_a.py"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := &fakeReplyProvider{bodies: [][]byte{
		toolCallBody(t, nativeCall("c1", tools.ToolWriteFile, `{"path":"tests/test_a.py","content":"assert False\n"}`)),
		toolCallBody(t, nativeCall("c2", tools.ToolWriteFile, `{"path":"src/ok.txt","content":"fine\n"}`)),
		proseBody(t, "I could not do it as asked."),
	}}
	s, err := engine.Open(context.Background(), engine.Options{
		Dir: dir, Selection: resumeSelection(t), SessionID: "protected", Provider: prov, Now: fixedClock(),
		ProtectedPaths: []string{"tests"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), "make it pass; do not change tests/"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close(context.Background())

	if got, _ := os.ReadFile(filepath.Join(dir, "tests", "test_a.py")); string(got) != original {
		t.Errorf("a protected file was changed: %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "src", "ok.txt")); string(got) != "fine\n" {
		t.Errorf("a write outside the protected path did not land: %q", got)
	}

	// The second request carries the refusal as the first call's result.
	if len(prov.requests) < 2 {
		t.Fatalf("got %d requests, want at least 2", len(prov.requests))
	}
	var refusal string
	for _, m := range prov.requests[1].Messages {
		if strings.Contains(m.Content, "protected in this session") {
			refusal = m.Content
		}
	}
	if !strings.Contains(refusal, "stop and say so") || !strings.Contains(refusal, "tests/test_a.py") {
		t.Errorf("the model was not told to report, naming the path; saw %q", refusal)
	}

	started := sole[journal.SessionStarted](t, readJournal(t, s.Path()))
	if len(started.ProtectedPaths) != 1 || started.ProtectedPaths[0] != "tests" {
		t.Errorf("SessionStarted.ProtectedPaths = %v, want [tests]", started.ProtectedPaths)
	}
}

func TestAProtectedPathOutsideTheRootRefusesTheSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KOPICODE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	for name, paths := range map[string][]string{
		"outside the root": {"../elsewhere"},
		"the root itself":  {"."},
		"empty":            {""},
	} {
		_, err := engine.Open(context.Background(), engine.Options{
			Dir: dir, Selection: resumeSelection(t), SessionID: "bad-" + strings.ReplaceAll(name, " ", "-"),
			Provider: &fakeReplyProvider{}, Now: fixedClock(), ProtectedPaths: paths,
		})
		if !errors.Is(err, engine.ErrConfig) {
			t.Errorf("%s: Open error = %v, want ErrConfig", name, err)
		}
	}
}

func TestASessionWithNoProtectedPathsRecordsNone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KOPICODE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	prov := &fakeReplyProvider{bodies: [][]byte{proseBody(t, "ok")}}
	s, err := engine.Open(context.Background(), engine.Options{
		Dir: dir, Selection: resumeSelection(t), SessionID: "unprotected", Provider: prov, Now: fixedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.Run(context.Background(), "hi")
	_ = s.Close(context.Background())
	if got := sole[journal.SessionStarted](t, readJournal(t, s.Path())).ProtectedPaths; got != nil {
		t.Errorf("an unprotected session recorded %v", got)
	}
}

// toolCallBody is a one-shot reply body carrying one native tool call. It is
// hook_test.go's callBody, which lives in a unix-only file and so is not there
// for the Windows vet that make xbuild runs.
func toolCallBody(t *testing.T, call wireCall) []byte {
	t.Helper()
	body := wireBody{
		ID: "gen-1", Object: "chat.completion", Model: testModelID, Provider: testServedBy,
		Choices: []wireChoice{{Message: wireMessage{Role: "assistant", ToolCalls: []wireCall{call}}, FinishReason: "tool_calls"}},
		Usage:   wireUsage{Prompt: 5, Completion: 3, Total: 8},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
