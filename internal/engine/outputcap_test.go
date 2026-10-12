package engine

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/leejianrong/kopicode/internal/tools"
)

func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %05d\n", i)
	}
	return b.String()
}

func TestCapOutputLeavesSmallAndUncappedOutputAlone(t *testing.T) {
	out := numberedLines(10)
	for _, c := range []struct {
		name, tool string
		limit      int
	}{
		{"under the cap", tools.ToolRunShell, 1000},
		{"cap off", tools.ToolRunShell, 0},
		{"a tool that bounds itself", tools.ToolReadFile, 10},
		{"an edit", tools.ToolEditFile, 10},
	} {
		if got := capOutput(c.tool, out, c.limit); got != out {
			t.Errorf("%s: output changed: %q", c.name, got)
		}
	}
}

func TestCapOutputKeepsTheStartAndMostlyTheEndAndSaysWhatWasLeftOut(t *testing.T) {
	out := numberedLines(2000) // 22000 bytes
	got := capOutput(tools.ToolRunShell, out, 4000)

	if !strings.HasPrefix(got, "line 00001\n") {
		t.Errorf("the start was not kept: %.40q", got)
	}
	if !strings.HasSuffix(got, "line 02000\n") {
		t.Errorf("the end was not kept: %q", got[len(got)-40:])
	}
	if len(got) > 4000+300 {
		t.Errorf("capped output is %d bytes, want about the 4000 cap plus the note", len(got))
	}
	if !strings.Contains(got, "lines (") || !strings.Contains(got, "omitted from the middle") {
		t.Errorf("no note saying what was left out: %q", got)
	}
	if !strings.Contains(got, "Re-run with a narrower command") {
		t.Errorf("run_shell's note does not say how to get another part: %q", got)
	}
	tail, head := got[strings.Index(got, "]\n")+2:], got[:strings.Index(got, "[")]
	if len(tail) <= len(head) {
		t.Errorf("the end got %d bytes and the start %d; the end is where a log puts its verdict", len(tail), len(head))
	}
	if !strings.Contains(capOutput(tools.ToolGrep, out, 4000), "Narrow the search") {
		t.Error("grep's note does not tell the model to narrow the search")
	}
}

// The count in the note must be true: kept head + omitted + kept tail is the whole.
func TestCapOutputAccountsForEveryLine(t *testing.T) {
	out := numberedLines(500)
	got := capOutput(tools.ToolRunShell, out, 1000)
	note := got[strings.Index(got, "["):]
	note = note[:strings.Index(note, "]")+1]
	var lines, bytes int
	if _, err := fmt.Sscanf(note, "[%d lines (%d bytes) omitted", &lines, &bytes); err != nil {
		t.Fatalf("unreadable note %q: %v", note, err)
	}
	kept := strings.Count(got, "line 0")
	if kept+lines != 500 {
		t.Errorf("kept %d lines and the note says %d omitted, want 500 in all", kept, lines)
	}
	if bytes+len(got)-len(note)-1 != len(out) {
		t.Errorf("kept bytes + omitted bytes = %d, want %d", bytes+len(got)-len(note)-1, len(out))
	}
}

func TestCapOutputNeverSplitsARune(t *testing.T) {
	out := strings.Repeat("日本語のテスト、", 2000) // one very long line of 3-byte runes
	got := capOutput(tools.ToolRunShell, out, 1001)
	if !utf8.ValidString(got) {
		t.Error("the capped output is not valid UTF-8")
	}
	if !strings.Contains(got, "omitted from the middle") {
		t.Error("a single long line was not capped")
	}
}

func TestCapOutputIsAPureFunction(t *testing.T) {
	out := numberedLines(900)
	first := capOutput(tools.ToolRunShell, out, 2000)
	if second := capOutput(tools.ToolRunShell, out, 2000); first != second {
		t.Error("two calls on the same input differ; a replay would build a different view")
	}
}
