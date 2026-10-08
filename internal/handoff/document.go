package handoff

import (
	"fmt"
	"strings"
)

// Narrative is the model's half of a handoff: the sections only it can write.
// Every field is plain text. A section the model left out is "" here and is
// rendered as [NotGiven], so a document is never missing a heading.
type Narrative struct {
	Goal         string
	Done         string
	Remaining    string
	Decisions    string
	OpenProblems string
	NextStep     string
	Verify       string
}

// NotGiven stands in for a section the model did not write. Saying so is the
// point: a blank would read as "nothing to say" when it means "not said".
const NotGiven = "(the previous session did not say)"

// section pairs a heading with where its text lives, in document order. The
// prompt, the parser and the renderer all walk this one list, so the three
// cannot drift apart.
type section struct {
	heading string
	// hint is the line of the prompt that tells the model what belongs there.
	hint string
	text func(*Narrative) *string
}

var sections = []section{
	{"Goal", "What the task is, in the user's terms. One or two sentences.", func(n *Narrative) *string { return &n.Goal }},
	{"Done", "What has been completed and is in the working tree now. Short bullets.", func(n *Narrative) *string { return &n.Done }},
	{"Remaining", "What is still to do, in order.", func(n *Narrative) *string { return &n.Remaining }},
	{"Decisions", "Choices made and why, so the next session does not undo or relitigate them.", func(n *Narrative) *string { return &n.Decisions }},
	{"Open problems", "Anything failing or not understood yet: the error, where it shows, what was tried.", func(n *Narrative) *string { return &n.OpenProblems }},
	{"Next step", "The single next thing to do, concrete enough to start without reading anything else first.", func(n *Narrative) *string { return &n.NextStep }},
	{"Verify", "The exact command that shows the work is correct.", func(n *Narrative) *string { return &n.Verify }},
}

// Prompt is the instruction for the one model call that writes the narrative. It
// is built from the same section list the parser and the renderer use.
func Prompt(goal string) string {
	var b strings.Builder
	b.WriteString("Write a handoff for the next session, which will start with an empty context and nothing but your handoff.\n\n")
	b.WriteString("Use exactly these headings, in this order, each as a line starting with ##:\n\n")
	for _, s := range sections {
		fmt.Fprintf(&b, "## %s\n%s\n\n", s.heading, s.hint)
	}
	b.WriteString("Rules: name files by path and never paste their contents. Do not describe tool calls or restate the conversation. ")
	b.WriteString("Do not say that tests pass unless you ran them and saw them pass; the next session will run them again anyway. ")
	b.WriteString("Be specific and short.\n")
	if goal = strings.TrimSpace(goal); goal != "" {
		fmt.Fprintf(&b, "\nThe next session's goal, as the user put it: %s\n", goal)
	}
	return b.String()
}

// ParseNarrative reads the model's reply. It is deliberately forgiving about
// form (heading depth, case, a trailing colon, bold markers) and strict about
// nothing: a model that wrote the sections in another order, left one out or
// added a preamble still produces a usable handoff, with text before the first
// heading ignored and each missing section left empty for [Render] to name.
func ParseNarrative(reply string) Narrative {
	var n Narrative
	var cur *string
	var buf []string

	flush := func() {
		if cur != nil {
			*cur = strings.TrimSpace(strings.Join(buf, "\n"))
		}
		buf = buf[:0]
	}
	for _, line := range strings.Split(strings.ReplaceAll(reply, "\r\n", "\n"), "\n") {
		if h, ok := headingOf(line); ok {
			flush()
			cur = nil
			for _, s := range sections {
				if strings.EqualFold(h, s.heading) {
					cur = s.text(&n)
					break
				}
			}
			continue
		}
		buf = append(buf, line)
	}
	flush()
	return n
}

// headingOf reports whether line is a markdown heading and returns its text with
// the markers, bold and a trailing colon removed.
func headingOf(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "#") {
		return "", false
	}
	t = strings.TrimSpace(strings.TrimLeft(t, "#"))
	t = strings.Trim(t, "*_ ")
	t = strings.TrimSpace(strings.TrimSuffix(t, ":"))
	return t, t != ""
}

// Render is the whole handoff: the narrative's sections in fixed order, then the
// facts kopicode recorded. The same inputs give the same bytes.
func Render(n Narrative, f Facts) string {
	var b strings.Builder
	b.WriteString("# Handoff from a previous session\n\n")
	b.WriteString("The previous session's context is gone; this is all you have of it. ")
	b.WriteString("Its verification result is not carried over: run the verify command yourself before you say anything passes.\n")

	for _, s := range sections {
		text := strings.TrimSpace(*s.text(&n))
		if text == "" {
			text = NotGiven
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", s.heading, text)
	}

	b.WriteString("\n## Recorded by kopicode\n\nFrom the journal, not written by the model.\n\n")
	b.WriteString(renderFacts(f))
	return b.String()
}

func renderFacts(f Facts) string {
	var b strings.Builder
	list := func(label string, paths []string) {
		if len(paths) == 0 {
			fmt.Fprintf(&b, "- %s: none\n", label)
			return
		}
		fmt.Fprintf(&b, "- %s: %s\n", label, strings.Join(paths, ", "))
	}
	list("Files written", f.FilesWritten)
	list("Files deleted", f.FilesDeleted)
	if f.RejectedEdits > 0 {
		fmt.Fprintf(&b, "- Edits refused for stale anchors: %d\n", f.RejectedEdits)
	}

	switch v := f.Verification; {
	case v == nil:
		b.WriteString("- Last verification: none was run\n")
	case v.Source == "none":
		b.WriteString("- Last verification: no command was configured or found\n")
	case v.Skip != "":
		fmt.Fprintf(&b, "- Last verification: `%s` did not conclude (%s)\n", strings.Join(v.Command, " "), v.Skip)
	case v.Passed():
		fmt.Fprintf(&b, "- Last verification: `%s` passed, at that point; run it again\n", strings.Join(v.Command, " "))
	default:
		fmt.Fprintf(&b, "- Last verification: `%s` exited %d, at that point; run it again\n", strings.Join(v.Command, " "), v.ExitCode)
	}

	for _, d := range f.Denied {
		fmt.Fprintf(&b, "- Refused by the permission gate: %s %s\n", d.Tool, d.Detail)
	}
	if f.DeniedMore > 0 {
		fmt.Fprintf(&b, "- …and %d more refused calls; the journal has them\n", f.DeniedMore)
	}
	fmt.Fprintf(&b, "- Spent: %d turns, %d requests, %d tokens (prompt %d, completion %d)\n",
		f.Turns, f.Requests, f.Total, f.Prompt, f.Completion)
	return b.String()
}
