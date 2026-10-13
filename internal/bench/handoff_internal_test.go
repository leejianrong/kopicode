package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
)

// bodyProvider serves the next canned reply to each request and keeps the
// requests, so a test can say what a session was told.
type bodyProvider struct {
	mu       sync.Mutex
	bodies   [][]byte
	requests []provider.Request
}

func (p *bodyProvider) Complete(ctx context.Context, req provider.Request) (*provider.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if len(p.requests) > len(p.bodies) {
		return nil, fmt.Errorf("bodyProvider: no reply left for request %d", len(p.requests))
	}
	return provider.NewBodyStream(ctx, p.bodies[len(p.requests)-1]), nil
}

func shellReply(t *testing.T, sel engine.Selection, n int) []byte {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"command": fmt.Sprintf("echo step-%d", n)})
	b, err := json.Marshal(wireBody{
		ID: fmt.Sprintf("gen-%d", n), Object: "chat.completion", Model: sel.ModelID, Provider: "TestProvider",
		Choices: []wireChoice{{
			Message: wireMessage{Role: "assistant", ToolCalls: []wireCall{{
				ID: fmt.Sprintf("call_%d", n), Type: "function",
				Function: wireFunction{Name: "run_shell", Arguments: args},
			}}},
			FinishReason: "tool_calls",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func proseReply(t *testing.T, sel engine.Selection, text string) []byte {
	t.Helper()
	b, err := json.Marshal(wireBody{
		ID: "gen-p", Object: "chat.completion", Model: sel.ModelID, Provider: "TestProvider",
		Choices: []wireChoice{{Message: wireMessage{Role: "assistant", Content: &text}, FinishReason: "stop"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func journalOf(t *testing.T, outDir, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(journal.SessionDir(outDir, id), journal.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func handoffSpec(t *testing.T, maxTurns int) (SessionSpec, string) {
	t.Helper()
	sel, err := engine.ResolveSelection(t.TempDir(), engine.SelectionOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	sel.Config.MaxTurns = maxTurns
	out := t.TempDir()
	return SessionSpec{
		Task: corpus.Task{ID: "kan1968", Statement: "fix the parser"},
		Dir:  t.TempDir(), Home: t.TempDir(), OutDir: out, SessionID: "kan1968-sess", Selection: sel,
	}, out
}

func TestATaskThatHitsTheCapHandsOffAndFinishesInAFreshSession(t *testing.T) {
	spec, out := handoffSpec(t, 2)
	prov := &bodyProvider{bodies: [][]byte{
		shellReply(t, spec.Selection, 1), shellReply(t, spec.Selection, 2), // the cap
		proseReply(t, spec.Selection, "## Goal\n\nfix the parser\n\n## Next step\n\nfinish\n"), // the draft
		proseReply(t, spec.Selection, "done"),                                                  // the second segment
	}}
	got, err := EngineAgent{testProvider: prov, HandoffAtCap: 1}.runSession(t.Context(), spec)
	if err != nil {
		t.Fatalf("runSession: %v", err)
	}
	if got.Handoffs != 1 || got.Turns != 3 || got.Stop != engine.StopCompleted.Reason() || got.LastSessionID != "kan1968-sess-h1" {
		t.Fatalf("outcome = %+v, want one handoff, 3 turns summed, completed, ending in -h1", got)
	}

	first, second := journalOf(t, out, "kan1968-sess"), journalOf(t, out, "kan1968-sess-h1")
	if !strings.Contains(first, `"type":"HandoffWritten"`) || !strings.Contains(first, `"type":"SessionEnded"`) {
		t.Errorf("the first segment's journal lacks HandoffWritten or SessionEnded")
	}
	if !strings.Contains(second, `"parent_session":"kan1968-sess"`) || !strings.Contains(second, `"scope":"handoff"`) {
		t.Errorf("the second segment's journal does not record where it began")
	}

	// The fresh segment was told the document, and not the old conversation.
	last := prov.requests[3]
	var seen strings.Builder
	for _, m := range last.Messages {
		seen.WriteString(m.Content + "\n")
	}
	if !strings.Contains(seen.String(), "<handoff>") || strings.Contains(seen.String(), "echo step-1") {
		t.Errorf("the second segment saw:\n%s", seen.String())
	}
}

func TestWithoutHandoffAtCapATaskThatHitsTheCapJustStops(t *testing.T) {
	spec, out := handoffSpec(t, 2)
	prov := &bodyProvider{bodies: [][]byte{shellReply(t, spec.Selection, 1), shellReply(t, spec.Selection, 2)}}
	got, err := EngineAgent{testProvider: prov}.runSession(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handoffs != 0 || got.Stop != engine.StopMaxTurns.Reason() || got.LastSessionID != "kan1968-sess" || len(prov.requests) != 2 {
		t.Fatalf("outcome = %+v after %d requests", got, len(prov.requests))
	}
	if strings.Contains(journalOf(t, out, "kan1968-sess"), "HandoffWritten") {
		t.Error("a plain run drafted a handoff")
	}
}

func TestHandoffsAreBoundedByHandoffAtCap(t *testing.T) {
	spec, _ := handoffSpec(t, 1)
	narr := proseReply(t, spec.Selection, "## Goal\n\nx\n")
	prov := &bodyProvider{bodies: [][]byte{
		shellReply(t, spec.Selection, 1), narr, // segment 0 caps, hands off
		shellReply(t, spec.Selection, 2), // segment 1 caps; no handoffs left
	}}
	got, err := EngineAgent{testProvider: prov, HandoffAtCap: 1}.runSession(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handoffs != 1 || got.Stop != engine.StopMaxTurns.Reason() || got.Turns != 2 || len(prov.requests) != 3 {
		t.Fatalf("outcome = %+v after %d requests", got, len(prov.requests))
	}
}
