package engine_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
)

// told is everything the model had been shown by the request at index i. The
// journal does not hold it when the loop adds to a result on the wire
// (ADR-0012 decision 1, KAN-1971), so the test reads the requests themselves.
func told(rec *recordingProvider, i int) string {
	var b strings.Builder
	for _, m := range rec.requests[i].Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// stallRun drives a script of native calls followed by a closing reply and
// returns what the model was told on each request.
func stallRun(t *testing.T, threshold int, files map[string]string, calls [][3]string) *recordingProvider {
	t.Helper()
	var replies []scriptedReply
	for i, c := range calls {
		replies = append(replies, scriptedReply{
			calls: []wireCall{nativeCall(fmt.Sprintf("call-%d", i+1), c[0], c[1])},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15},
		})
	}
	replies = append(replies, scriptedReply{text: "done.", usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}})

	prov := script(t, replies, oneAttemptPerTurn(len(replies)))
	rec := &recordingProvider{inner: prov}
	h := newHarness(t, prov, files,
		withProvider(rec), withMaxTurns(len(replies)+1),
		func(c *engine.Config) { c.Selection.Config.StallThreshold = threshold })
	if _, err := h.eng.Run(t.Context(), "look around"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rec
}

const sameCallNote = "kopicode: you have made this exact read_file call"

func TestTheSameCallWithTheSameResultIsCalledOutAtTheThreshold(t *testing.T) {
	read := [3]string{"read_file", `{"path":"a.txt"}`, ""}
	rec := stallRun(t, 3, map[string]string{"a.txt": "hello\n"}, [][3]string{read, read, read, read})

	if len(rec.requests) != 5 {
		t.Fatalf("the model was asked %d times, want 5", len(rec.requests))
	}
	if strings.Contains(told(rec, 2), sameCallNote) {
		t.Error("the note appeared after only two identical calls; two is a model being careful")
	}
	got := told(rec, 3)
	if !strings.Contains(got, sameCallNote+" 3 times") {
		t.Errorf("after the third identical call the model was not told it was stuck; saw:\n%s", got)
	}
	if strings.Count(told(rec, 4), sameCallNote) != 1 {
		t.Error("the note must fire once per run of N, not on every call after it")
	}
}

func TestNothingIsAddedWhenTheDetectorIsOff(t *testing.T) {
	read := [3]string{"read_file", `{"path":"a.txt"}`, ""}
	rec := stallRun(t, 0, map[string]string{"a.txt": "hello\n"}, [][3]string{read, read, read, read})
	for i := 0; i < len(rec.requests); i++ {
		if strings.Contains(told(rec, i), "kopicode: you have made") {
			t.Fatalf("request %d carries a stall note with the threshold at 0", i+1)
		}
	}
}

func TestAChangeToTheTreeAnswersTheQuestion(t *testing.T) {
	read := [3]string{"read_file", `{"path":"a.txt"}`, ""}
	write := [3]string{"write_file", `{"path":"b.txt","content":"x\n"}`, ""}
	// Two identical reads, a write, two more: never three alike without a change
	// between them, so the model has not been going in circles.
	rec := stallRun(t, 3, map[string]string{"a.txt": "hello\n"}, [][3]string{read, read, write, read, read})
	for i := 0; i < len(rec.requests); i++ {
		if strings.Contains(told(rec, i), "kopicode: you have made") {
			t.Fatalf("request %d was told it was stuck although the tree changed in between", i+1)
		}
	}
}

func TestEditsThatKeepFailingOnOneFileAreCalledOut(t *testing.T) {
	// Well-formed anchors that match no line, so the tool rejects each (anchor
	// drift), and different text each time so this is not the same-call rule.
	bad := func(n int) [3]string {
		return [3]string{"edit_file", fmt.Sprintf(`{"path":"a.txt","anchor_start":"deadbeef","anchor_end":"deadbeef","new_text":"attempt %d"}`, n), ""}
	}
	rec := stallRun(t, 3, map[string]string{"a.txt": "hello\n"}, [][3]string{bad(1), bad(2), bad(3), bad(4)})

	if strings.Contains(told(rec, 2), "edits to a.txt in a row have failed") {
		t.Error("the note appeared after only two failed edits")
	}
	if got := told(rec, 3); !strings.Contains(got, "3 edits to a.txt in a row have failed") ||
		!strings.Contains(got, "read_file") {
		t.Errorf("after the third failed edit the model was not told to re-read the file; saw:\n%s", got)
	}
}
