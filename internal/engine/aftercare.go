package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/leejianrong/kopicode/internal/journal"
)

// An edit replaces the lines between two anchors. A model that wants to rewrite
// a function but names only its `def` line writes a replacement holding the whole
// new function, and the old body, outside the region, stays below it. The file
// then parses and its tests may pass, and nothing else looks at what the edit left
// behind. ADR-0040 is that look, over the diff the edit itself produced.

// deadLine is a `return` or `raise` and the line after it that can never run, as
// the post-edit file numbers them, with the text of each for the note.
type deadLine struct {
	line, next         int
	lineText, nextText string
}

var (
	hunkRE   = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
	exitRE   = regexp.MustCompile(`^(\s*)(?:return|raise)\b`)
	branchRE = regexp.MustCompile(`^\s*(?:else|elif|except|finally|case)\b`)
	// closerRE and commentRE are lines at a return's own indentation that are not
	// code: the end of a multi-line return, and a comment.
	closerRE  = regexp.MustCompile(`^\s*[)\]}]`)
	commentRE = regexp.MustCompile(`^\s*#`)
)

// maxDeadReported is how many pairs one edit's note names.
const maxDeadReported = 3

// deadAfterExit returns the lines the edit's diff leaves after a `return` or
// `raise` at the same indentation, where either of the two was added by the edit,
// so a file's own old dead code is never reported. Only Python is read: indentation
// is its block structure, and a brace language needs a parser, not this rule. The
// rule sees the lines a unified diff carries, its changed lines and their context,
// which is all an edit's own damage can be in.
func deadAfterExit(path, diff string) []deadLine {
	if !strings.HasSuffix(path, ".py") {
		return nil
	}
	var out []deadLine
	type post struct {
		text  string
		added bool
		no    int
	}
	var lines []post
	flush := func() {
		for i := 0; i < len(lines); i++ {
			m := exitRE.FindStringSubmatch(lines[i].text)
			if m == nil {
				continue
			}
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j].text) == "" {
				j++
			}
			if j >= len(lines) {
				continue
			}
			nx := lines[j].text
			if !strings.HasPrefix(nx, m[1]) || len(nx)-len(strings.TrimLeft(nx, " \t")) != len(m[1]) {
				continue // deeper, or a dedent: a different block
			}
			if branchRE.MatchString(nx) || closerRE.MatchString(nx) || commentRE.MatchString(nx) {
				continue
			}
			if !lines[i].added && !lines[j].added {
				continue
			}
			out = append(out, deadLine{
				line: lines[i].no, next: lines[j].no,
				lineText: strings.TrimSpace(lines[i].text), nextText: strings.TrimSpace(nx),
			})
		}
		lines = lines[:0]
	}
	no := 0
	inHunk := false
	for _, l := range strings.Split(diff, "\n") {
		if m := hunkRE.FindStringSubmatch(l); m != nil {
			flush()
			no, _ = strconv.Atoi(m[1])
			inHunk = true
			continue
		}
		if !inHunk || l == "" {
			continue
		}
		switch l[0] {
		case ' ':
			lines = append(lines, post{l[1:], false, no})
			no++
		case '+':
			lines = append(lines, post{l[1:], true, no})
			no++
		}
	}
	flush()
	return out
}

// deadJournal is the part of deadAfterExit the journal keeps.
func deadJournal(d []deadLine) []journal.UnreachableLine {
	if len(d) == 0 {
		return nil
	}
	out := make([]journal.UnreachableLine, len(d))
	for i, x := range d {
		out[i] = journal.UnreachableLine{Line: x.line, Next: x.next}
	}
	return out
}

// aftercareNote is the sentence appended to an applied edit's result when the
// edit-aftercare setting is on (ADR-0040 decision 2).
func aftercareNote(d []deadLine) string {
	if len(d) == 0 {
		return ""
	}
	var b strings.Builder
	for i, x := range d {
		if i == maxDeadReported {
			break
		}
		fmt.Fprintf(&b, "\nkopicode: after this edit, line %d (`%s`) is followed at the same indentation by line %d (`%s`), which can never run. "+
			"If the old text was meant to be replaced, your range ended short: re-read the file and replace the rest.",
			x.line, clipLine(x.lineText), x.next, clipLine(x.nextText))
	}
	return b.String()
}

func clipLine(s string) string {
	const max = 60
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
