package handoff_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/handoff"
	"github.com/leejianrong/kopicode/internal/journal"
)

func ev(turn int, p journal.Payload) journal.Event { return journal.Event{Turn: turn, Payload: p} }

func call(turn int, id, tool, args string) []journal.Event {
	return []journal.Event{
		ev(turn, journal.ToolCallParsed{CallID: id, Tool: tool, Args: json.RawMessage(args)}),
	}
}

func result(turn int, id, tool, kind string) journal.Event {
	return ev(turn, journal.ToolResult{CallID: id, Tool: tool, ErrorKind: kind, Output: journal.InlineText("out")})
}

func TestFactsAreDerivedFromTheJournalAlone(t *testing.T) {
	var events []journal.Event
	events = append(events, call(1, "c1", "write_file", `{"path":"b.go","content":"x"}`)...)
	events = append(events, result(1, "c1", "write_file", ""))
	// A write that failed did not write anything.
	events = append(events, call(1, "c2", "write_file", `{"path":"never.go","content":"x"}`)...)
	events = append(events, result(1, "c2", "write_file", "task"))
	events = append(events, call(2, "c3", "delete_file", `{"path":"old.go"}`)...)
	events = append(events, result(2, "c3", "delete_file", ""))
	events = append(events,
		ev(2, journal.EditApplied{CallID: "c4", Path: "a.go"}),
		ev(3, journal.EditApplied{CallID: "c5", Path: "a.go"}),
		ev(3, journal.EditRejected{CallID: "c6", Path: "a.go", Reason: "anchor_drift"}),
		ev(3, journal.VerificationRun{Command: []string{"go", "test", "./..."}, Source: "discovered", ExitCode: 1}),
		ev(4, journal.VerificationRun{Command: []string{"go", "test", "./..."}, Source: "discovered", ExitCode: 0}),
		ev(1, journal.ProviderResponse{Tokens: journal.TokenCounts{Prompt: 100, Completion: 10, Total: 110}}),
		ev(2, journal.ProviderResponse{Tokens: journal.TokenCounts{Prompt: 150, Completion: 20, Total: 170}}),
	)

	f := handoff.FactsFrom(events)

	if got := strings.Join(f.FilesWritten, ","); got != "a.go,b.go" {
		t.Errorf("FilesWritten = %q, want a.go,b.go sorted and unique, and never.go absent", got)
	}
	if got := strings.Join(f.FilesDeleted, ","); got != "old.go" {
		t.Errorf("FilesDeleted = %q", got)
	}
	if f.RejectedEdits != 1 {
		t.Errorf("RejectedEdits = %d", f.RejectedEdits)
	}
	if f.Verification == nil || !f.Verification.Passed() {
		t.Errorf("Verification = %+v, want the last run, which passed", f.Verification)
	}
	if f.Turns != 4 || f.Requests != 2 || f.Total != 280 || f.Prompt != 250 || f.Completion != 30 {
		t.Errorf("usage = turns %d requests %d total %d prompt %d completion %d", f.Turns, f.Requests, f.Total, f.Prompt, f.Completion)
	}
}

func TestDeniedCallsAreListedOnceAndBounded(t *testing.T) {
	var events []journal.Event
	for i := 0; i < handoff.MaxDenied+3; i++ {
		id := fmt.Sprintf("r%d", i)
		events = append(events,
			ev(1, journal.PermissionRequested{RequestID: id, Tool: "run_shell", Detail: journal.InlineText(fmt.Sprintf("rm -rf /tmp/x%d", i))}),
			ev(1, journal.PermissionDecided{RequestID: id, Decision: "deny"}))
	}
	// The same refusal again, and an allowed call, add nothing.
	events = append(events,
		ev(2, journal.PermissionRequested{RequestID: "again", Tool: "run_shell", Detail: journal.InlineText("rm -rf /tmp/x0")}),
		ev(2, journal.PermissionDecided{RequestID: "again", Decision: "deny"}),
		ev(2, journal.PermissionRequested{RequestID: "ok", Tool: "run_shell", Detail: journal.InlineText("go test")}),
		ev(2, journal.PermissionDecided{RequestID: "ok", Decision: "allow"}))

	f := handoff.FactsFrom(events)
	if len(f.Denied) != handoff.MaxDenied || f.DeniedMore != 3 {
		t.Fatalf("denied = %d listed and %d more, want %d and 3", len(f.Denied), f.DeniedMore, handoff.MaxDenied)
	}
	if f.Denied[0].Detail != "rm -rf /tmp/x0" {
		t.Errorf("first denied = %+v, want first-seen order", f.Denied[0])
	}
}

func TestNoEventsGivesAnEmptyButUsableFacts(t *testing.T) {
	f := handoff.FactsFrom(nil)
	doc := handoff.Render(handoff.Narrative{}, f)
	for _, want := range []string{"Files written: none", "Last verification: none was run", "0 turns"} {
		if !strings.Contains(doc, want) {
			t.Errorf("empty facts rendered without %q:\n%s", want, doc)
		}
	}
}

func TestParseNarrativeIsForgivingAboutForm(t *testing.T) {
	reply := `Sure, here is the handoff.

### **Goal:**
Make the retry backoff exponential.

## done
- wrote backoff.go

## Next Step
Run the failing test and read the first error.

## Remaining
1. wire it into client.go
## Verify
` + "`go test ./...`"
	n := handoff.ParseNarrative(reply)

	if n.Goal != "Make the retry backoff exponential." {
		t.Errorf("Goal = %q", n.Goal)
	}
	if n.Done != "- wrote backoff.go" || n.NextStep != "Run the failing test and read the first error." {
		t.Errorf("Done = %q, NextStep = %q", n.Done, n.NextStep)
	}
	if n.Remaining != "1. wire it into client.go" || n.Verify != "`go test ./...`" {
		t.Errorf("Remaining = %q, Verify = %q", n.Remaining, n.Verify)
	}
	if n.Decisions != "" || n.OpenProblems != "" {
		t.Error("sections the model left out must stay empty, not be invented")
	}
}

func TestRenderNamesWhatWasNotSaidAndKeepsEveryHeading(t *testing.T) {
	doc := handoff.Render(handoff.Narrative{Goal: "do the thing"}, handoff.Facts{})
	order := []string{"## Goal", "## Done", "## Remaining", "## Decisions", "## Open problems", "## Next step", "## Verify", "## Recorded by kopicode"}
	at := 0
	for _, h := range order {
		i := strings.Index(doc[at:], h)
		if i < 0 {
			t.Fatalf("heading %q missing or out of order in:\n%s", h, doc)
		}
		at += i
	}
	if strings.Count(doc, handoff.NotGiven) != 6 {
		t.Errorf("want %q for each of the six sections left out, got:\n%s", handoff.NotGiven, doc)
	}
	if !strings.Contains(doc, "not carried over") {
		t.Error("the document must say verification is not carried over")
	}
}

func TestAPassingVerificationIsNeverStatedAsCurrent(t *testing.T) {
	f := handoff.Facts{Verification: &handoff.Verification{Command: []string{"make", "test"}, Source: "configured", ExitCode: 0}}
	doc := handoff.Render(handoff.Narrative{}, f)
	if !strings.Contains(doc, "passed, at that point; run it again") {
		t.Errorf("a passing run must be dated and followed by an instruction to rerun:\n%s", doc)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	n := handoff.ParseNarrative("## Goal\nx\n## Done\ny\n")
	f := handoff.FactsFrom([]journal.Event{ev(1, journal.EditApplied{Path: "z.go"}), ev(1, journal.EditApplied{Path: "a.go"})})
	a, b := handoff.Render(n, f), handoff.Render(n, f)
	if a != b {
		t.Error("two renders of the same input differ")
	}
	if !strings.Contains(a, "a.go, z.go") {
		t.Errorf("files must be sorted:\n%s", a)
	}
}

func TestPromptAndParserShareTheSameSections(t *testing.T) {
	p := handoff.Prompt("ship it")
	// Every heading the prompt asks for must come back out of the parser.
	reply := ""
	for _, h := range []string{"Goal", "Done", "Remaining", "Decisions", "Open problems", "Next step", "Verify"} {
		if !strings.Contains(p, "## "+h+"\n") {
			t.Errorf("the prompt does not ask for %q", h)
		}
		reply += "## " + h + "\ntext for " + h + "\n"
	}
	n := handoff.ParseNarrative(reply)
	doc := handoff.Render(n, handoff.Facts{})
	if strings.Contains(doc, handoff.NotGiven) {
		t.Errorf("a reply with every section lost one in parsing:\n%s", doc)
	}
	if !strings.Contains(p, "ship it") {
		t.Error("the user's goal is not in the prompt")
	}
}
