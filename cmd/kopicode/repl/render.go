package repl

import (
	"fmt"
	"strconv"
	"strings"
)

// Kind is what an [Event] is about.
//
// The vocabulary is the journal's, deliberately: with three exceptions at the
// bottom, every kind here corresponds to exactly one payload type in
// internal/journal, and the adapter that drives the engine builds an Event by
// copying fields off the payload it just saw. That is what makes "everything
// the REPL prints is derived from journal events" a checkable claim rather
// than an intention (ADR-0002 decision 2).
//
//	Kind                    journal payload
//	KindSessionStarted      SessionStarted
//	KindUser                UserMessage
//	KindAssistant           AssistantMessage
//	KindThinking            ThinkingBlock
//	KindToolCall            ToolCallParsed  (and ToolCallRequested)
//	KindToolRepair          ToolCallRepaired
//	KindToolFailed          ToolCallFailed
//	KindToolResult          ToolResult
//	KindEditApplied         EditApplied
//	KindEditRejected        EditRejected
//	KindSyntaxGate          SyntaxGateRun
//	KindPermissionDecided   PermissionDecided
//	KindSnapshot            TurnSnapshot
//	KindVerification        VerificationRun
//	KindSessionEnded        SessionEnded
//
// Three kinds have no journal payload behind them and are marked as such
// where they are declared: [KindNotice] and [KindError] are the surface
// talking about itself, and [KindTurnStopped] renders an [engine.Stop], which
// is a summary of events the loop already wrote rather than an event of its
// own.
//
// PermissionRequested has no kind because it is not rendered as a line: it is
// rendered as a question, by [Loop.Ask].
type Kind uint8

const (
	// KindUnspecified is the zero value. Rendering one is a bug in the
	// caller, and it is printed as such rather than silently dropped.
	KindUnspecified Kind = iota

	KindSessionStarted
	KindUser
	KindAssistant
	KindThinking
	KindToolCall
	KindToolRepair
	KindToolFailed
	KindToolResult
	KindEditApplied
	KindEditRejected
	KindSyntaxGate
	KindPermissionDecided
	KindSnapshot
	KindVerification
	KindSessionEnded

	// KindNotice is the surface talking about itself. No journal payload.
	KindNotice
	// KindError is the surface reporting its own failure. No journal payload.
	KindError
	// KindTurnCancelled is a turn the user interrupted. No journal payload —
	// and that absence is KAN-857, which this package cannot close from
	// here: see the note on [Loop.runTurn] in repl.go and the card.
	KindTurnCancelled
	// KindTurnStopped renders an engine.Stop other than completed or
	// cancelled. No journal payload; it summarises the record.
	KindTurnStopped
)

// Event is one thing to print, with the fields a journal payload supplies.
//
// It is a flat struct rather than a union of per-kind types because it is
// copied *from* a union of per-kind types and immediately rendered to a line:
// a second tagged union here would be a translation table with nothing in it,
// and the field names below are the payload field names so the copy is
// checkable by eye.
//
// Fields not relevant to a kind are left zero.
type Event struct {
	Kind Kind

	// Text is the message body: the assistant's prose, the thinking block,
	// the notice, the error, the detail of a failure.
	Text string
	// Tool is the tool name as the model called it.
	Tool string
	// Path is the file an edit or a syntax check was about.
	Path string
	// Detail is the short form of what happened: a command line, an
	// argument summary, a session id.
	Detail string
	// Reason is why: a rejection reason, a stop reason, a policy reason.
	Reason string
	// Mode is an edit's "anchored" or "fuzzy". A fuzzy edit is shown as
	// such because the session it is in is classified unattributed, and a
	// surface that hid the mode would hide the reason (SLICE-1 §4).
	Mode string
	// Ref is a snapshot's shadow ref.
	Ref string
	// Command is a verification command's argv.
	Command []string
	// Decision and Source are a permission outcome: "allow"/"deny"/
	// "allow_session", and "user"/"policy".
	Decision string
	Source   string
	// Checker is the syntax gate's command, empty when none was available.
	Checker string
	// Ran reports whether the syntax gate actually ran. A gate that did not
	// run must not read as a pass (ADR-0006 §4), so it is rendered
	// differently rather than omitted.
	Ran bool
	// ExitCode is a process exit code, valid when HasExitCode is set.
	ExitCode    int
	HasExitCode bool
	// Size is a payload size in bytes, for output that is summarised rather
	// than printed. Nothing here truncates anything: the full value is in
	// the journal, whole, and this is a pointer to it.
	Size int64
}

// Stream prints model text as it arrives, verbatim.
func (l *Loop) Stream(text string) {
	if text == "" {
		return
	}
	l.streamed.WriteString(text)
	l.out.text(text)
}

// Progress paints a status on the current line only. "" clears it.
func (l *Loop) Progress(status string) { l.out.setProgress(status) }

// Render prints one event.
func (l *Loop) Render(e Event) {
	switch e.Kind {
	case KindAssistant:
		l.renderAssistant(e.Text)

	case KindUser:
		// On a terminal the user has already read what they typed, and the
		// terminal echoed it. Piped, nothing echoed it and the transcript
		// would be a monologue, so the record's copy is printed.
		if !l.out.interactive {
			l.out.line("> " + e.Text)
		}

	case KindThinking:
		for _, ln := range splitLines(e.Text) {
			l.out.line(l.out.dim("  " + ln))
		}

	case KindSessionStarted:
		l.tag("session", "started "+e.Detail)

	case KindToolCall:
		l.tag("tool", e.Tool+" "+e.Detail)

	case KindToolRepair:
		l.tag("repair", fmt.Sprintf("%s: %s", orUnknown(e.Tool), e.Reason))

	case KindToolFailed:
		l.tag("tool", fmt.Sprintf("%s failed: %s", orUnknown(e.Tool), e.Reason))

	case KindToolResult:
		l.tag("tool", e.Tool+": "+resultSummary(e))

	case KindEditApplied:
		l.tag("edit", fmt.Sprintf("%s applied (%s)", e.Path, e.Mode))

	case KindEditRejected:
		l.tag("edit", fmt.Sprintf("%s rejected: %s", e.Path, e.Reason))
		l.indent(e.Text)

	case KindSyntaxGate:
		l.tag("syntax", syntaxSummary(e))

	case KindPermissionDecided:
		l.tag("perm", fmt.Sprintf("%s (%s)", e.Decision, e.Source))

	case KindSnapshot:
		l.tag("snap", e.Ref)

	case KindVerification:
		l.tag("verify", fmt.Sprintf("%s exit %d", strings.Join(e.Command, " "), e.ExitCode))

	case KindSessionEnded:
		l.tag("session", "ended: "+e.Reason)

	case KindTurnCancelled:
		l.tag("cancelled", "turn interrupted; the session is still open")

	case KindTurnStopped:
		l.tag("stopped", e.Text)

	case KindNotice:
		l.tag("note", e.Text)

	case KindError:
		l.out.line(l.out.bold("[error] ") + e.Text)

	case KindUnspecified:
		fallthrough
	default:
		// Loud rather than dropped. An event nobody rendered is a fact the
		// journal holds and the user was not told, which is the one thing
		// this surface exists not to do.
		l.out.line(fmt.Sprintf("[error] repl: no rendering for event kind %d", uint8(e.Kind)))
	}
}

// renderAssistant prints the message the journal recorded, minus whatever
// [Loop.Stream] already put on the screen.
//
// See the package comment. The streamed bytes are an optimistic prefix of the
// record; when they are a prefix, only the remainder is printed, and when they
// are not, the record is printed in full behind a marker because the record is
// what the session actually was.
func (l *Loop) renderAssistant(text string) {
	streamed := l.streamed.String()
	l.streamed.Reset()

	switch {
	case streamed == "":
		l.out.text(text)
	case strings.HasPrefix(text, streamed):
		l.out.text(text[len(streamed):])
	default:
		l.out.flushLine()
		l.tag("note", "the streamed text differed from the recorded message; the record follows")
		l.out.text(text)
	}
	l.out.flushLine()
}

// tag writes one tagged line: "[tool] read_file: ok".
//
// The tag is a fixed, greppable, ASCII prefix. Not a unicode glyph and not a
// colour alone — a piped session has neither, and a line whose meaning
// survives only in the terminal is a line the transcript lost.
func (l *Loop) tag(tag, rest string) {
	l.out.line(l.out.dim("["+tag+"] ") + rest)
}

// indent prints a detail block under the line it belongs to.
func (l *Loop) indent(text string) {
	for _, ln := range splitLines(text) {
		l.out.line("       " + ln)
	}
}

// resultSummary renders a tool result: what happened, and how much output the
// journal is holding.
//
// It summarises the *size* and never the content. Nothing is truncated here
// because nothing is printed here: the full output is in the record, whole,
// and a surface that pasted 64 KiB of test output into the scrollback would be
// unusable rather than more honest.
func resultSummary(e Event) string {
	var parts []string
	switch e.Reason {
	case "":
		parts = append(parts, "ok")
	case "task":
		parts = append(parts, "reported a problem")
	case "internal":
		parts = append(parts, "harness error")
	case "cancelled":
		parts = append(parts, "cancelled")
	default:
		parts = append(parts, e.Reason)
	}
	if e.HasExitCode {
		parts = append(parts, "exit "+strconv.Itoa(e.ExitCode))
	}
	if e.Size > 0 {
		parts = append(parts, humanBytes(e.Size)+" of output")
	}
	return strings.Join(parts, ", ")
}

// syntaxSummary renders the post-edit language check, keeping "did not run"
// distinct from "passed".
func syntaxSummary(e Event) string {
	if !e.Ran {
		return e.Path + ": no checker available, not run"
	}
	return fmt.Sprintf("%s: %s exit %d", e.Path, e.Checker, e.ExitCode)
}

// humanBytes renders a size the way a person reads one. Binary units, because
// the threshold it is usually near is the journal's 64 KiB spill.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

// orUnknown names a tool that never identified itself. ToolCallFailed.Tool is
// "" when the call named no recognisable one, and printing an empty string
// where a name goes reads as a rendering bug.
func orUnknown(tool string) string {
	if tool == "" {
		return "(unnamed tool)"
	}
	return tool
}

// splitLines splits text into lines without a trailing empty one, so a block
// ending in a newline does not render a blank line under itself.
func splitLines(text string) []string {
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
