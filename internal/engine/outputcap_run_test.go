package engine_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// KOP-179. A grep that returns 200 long lines (its own match limit): the model sees the start
// and the end and a note, and the journal still holds every byte.
func TestAHugeGrepResultIsCappedForTheModelAndWholeInTheJournal(t *testing.T) {
	var file strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&file, "match %05d %s\n", i, strings.Repeat("x", 90))
	}
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-1", "grep", `{"pattern":"match","path":"big.txt"}`)}, usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "done.", usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
	}
	prov := script(t, replies, oneAttemptPerTurn(len(replies)))
	rec := &recordingProvider{inner: prov}
	h := newHarness(t, prov, map[string]string{"big.txt": file.String()},
		withProvider(rec), withMaxTurns(4),
		func(c *engine.Config) { c.Selection.Config.OutputCap = 2000 })
	if _, err := h.eng.Run(t.Context(), "find them"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	seen := told(rec, 1)
	if !strings.Contains(seen, "omitted from the middle") {
		t.Fatalf("the model was shown the whole result; saw %d bytes", len(seen))
	}
	if !strings.Contains(seen, "match 00200") {
		t.Error("the end of the output was cut; a log's verdict is there")
	}
	if strings.Contains(seen, "match 00100") {
		t.Error("the middle of the output reached the model")
	}

	res := sole[journal.ToolResult](t, h.events())
	whole := res.Output.Inline
	if !strings.Contains(whole, "match 00100") || !strings.Contains(whole, "match 00200") || strings.Contains(whole, "omitted from the middle") {
		t.Error("the journal's result is not the whole, unmarked output")
	}
}

func TestNothingIsCappedWhenTheCapIsOff(t *testing.T) {
	var file strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&file, "match %05d %s\n", i, strings.Repeat("x", 90))
	}
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-1", "grep", `{"pattern":"match","path":"big.txt"}`)}, usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
		{text: "done.", usage: wireUsage{Prompt: 10, Completion: 5, Total: 15}},
	}
	prov := script(t, replies, oneAttemptPerTurn(len(replies)))
	rec := &recordingProvider{inner: prov}
	h := newHarness(t, prov, map[string]string{"big.txt": file.String()}, withProvider(rec), withMaxTurns(4))
	if _, err := h.eng.Run(t.Context(), "find them"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if seen := told(rec, 1); strings.Contains(seen, "omitted from the middle") {
		t.Error("a result was capped with OutputCap 0")
	}
}
