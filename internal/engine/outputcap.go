package engine

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/leejianrong/kopicode/internal/tools"
)

// The tool-output cap (KOP-179, ADR-0033). run_shell and grep can return far more
// than a model can use: a test run's whole log, a grep that matches every line.
// Past Config.OutputCap bytes the model is shown the start and, mostly, the end of
// the output with a line saying how much was left out and how to get a different
// part of it. The end gets the larger share because a build or test log puts its
// verdict there.
//
// Only the string handed to the assembler changes. journalToolResult has already
// written the whole output, and the stall detector reads the whole output, so the
// record stays complete (AGENTS.md, "Never truncate tool output") and a replay of
// the journal builds the same capped view, because this is a pure function of the
// output and the cap.
//
// The model cannot read the journal or a blob (the sandbox hides .kopicode, see
// ADR-0031 decision 4), so the note does not point at one. It tells the model to
// narrow the command, which is the recovery it can actually perform.

// cappedTool reports whether tool's output is subject to the cap. read_file
// already bounds itself and takes an offset; the edit tools are small and are the
// model's record of what it changed.
func cappedTool(tool string) bool {
	return tool == tools.ToolRunShell || tool == tools.ToolGrep
}

// capOutput returns what the model sees of output. limit <= 0 is off. Output at or
// under limit bytes is returned untouched. Otherwise it keeps about a quarter of limit
// from the start and the rest from the end, each cut on a line boundary where one
// exists and never inside a UTF-8 sequence, and names what was left out.
func capOutput(tool, output string, limit int) string {
	if limit <= 0 || !cappedTool(tool) || len(output) <= limit {
		return output
	}
	headBudget := limit / 4
	tailBudget := limit - headBudget

	head := cutHead(output, headBudget)
	tail := cutTail(output[len(head):], tailBudget)

	omitted := output[len(head) : len(output)-len(tail)]
	omittedLines := strings.Count(omitted, "\n")
	if omitted != "" && !strings.HasSuffix(omitted, "\n") {
		omittedLines++
	}

	var how string
	if tool == tools.ToolGrep {
		how = "Narrow the search (a more specific pattern or a path) to see another part."
	} else {
		how = "Re-run with a narrower command (pipe to grep, head or tail, or run one test) to see another part."
	}
	note := fmt.Sprintf("[%d lines (%d bytes) omitted from the middle of this output. %s]",
		omittedLines, len(omitted), how)
	return head + note + "\n" + tail
}

// cutHead returns the longest prefix of s of at most n bytes that ends at a line
// break, or, when the first line is longer than n, the longest valid UTF-8 prefix.
func cutHead(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if i := strings.LastIndexByte(s[:n], '\n'); i >= 0 {
		return s[:i+1]
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutTail returns the longest suffix of s of at most n bytes that starts at a
// line start, or, when the last line is longer than n, the longest valid UTF-8
// suffix.
func cutTail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	start := len(s) - n
	if i := strings.IndexByte(s[start:], '\n'); i >= 0 && start+i+1 < len(s) {
		return s[start+i+1:]
	}
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}
