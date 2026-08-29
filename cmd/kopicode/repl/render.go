package repl

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/leejianrong/kopicode/internal/engine"
)

// This file is the whole of what the REPL prints, and it prints one thing: the
// engine's event stream.
//
// [engine.Event] is derived from the journal at the moment each event is
// recorded — the append happens first and only what the record accepted is
// announced — so a line here is a rendering of the session record rather than a
// second account of it (ADR-0002 decision 2). There is no other source: the
// surface adds only what it knows about *itself*, which is the four notices
// halfway down this file, and each says so where it is declared.

// Render prints one event from the engine's stream.
//
// It is [Surface.Render], and it is also what the wiring gives
// engine.Options.Events. Those are the same object by design: a turn that
// prints and a session announcing its own record are one surface, and two would
// interleave.
func (l *Loop) Render(e engine.Event) {
	switch e.Kind {
	case engine.EventDelta:
		l.delta(e)

	case engine.EventAssistantMessage:
		l.assistant(e.Text)

	case engine.EventUserMessage:
		// On a terminal the user has already read what they typed, and the
		// terminal echoed it. Piped, nothing echoed it and the transcript
		// would be a monologue, so the record's copy is printed.
		if !l.out.interactive {
			l.out.line("> " + e.Text)
		}

	case engine.EventThinking:
		for _, ln := range splitLines(e.Text) {
			l.out.line(l.out.dim("  " + ln))
		}

	case engine.EventSessionStarted:
		l.tag("session", strings.TrimSpace(e.Detail+" on "+e.Text))

	case engine.EventProviderRequest:
		// Not a line. One per turn plus one per repair would bury the events
		// that matter, and what a user wants from it is the waiting — which is
		// the progress indicator's job, on the current line, never scrollback.
		l.Progress("waiting for " + e.Detail)

	case engine.EventProviderRetried:
		// Unlike a provider request or response, this is not routine — it says
		// the provider answered with a 429, a 5xx, or nothing at all, and the
		// client is about to try again on its own. A user watching a session
		// wants to know the wait is the provider being flaky and not the model
		// thinking, so this is a line rather than progress text.
		l.tag("retry", fmt.Sprintf("attempt %d, %s: %s", e.ExitCode, e.Detail, e.Reason))

	case engine.EventProviderResponse:
		l.Progress("")

	case engine.EventToolCallRequested:
		// Also not a line: it is the raw text of a call that did not parse, and
		// the repair or the failure that follows says what was wrong. The raw
		// text stays in the record for whoever is measuring the parser.

	case engine.EventToolCallParsed:
		l.tag("tool", strings.TrimSpace(e.Tool+" "+e.Detail))

	case engine.EventToolCallRepaired:
		l.tag("repair", fmt.Sprintf("attempt %d: %s", e.ExitCode, e.Reason))

	case engine.EventToolCallFailed:
		l.tag("tool", fmt.Sprintf("%s failed: %s", orUnknown(e.Tool), e.Reason))

	case engine.EventToolResult:
		l.tag("tool", e.Tool+": "+resultSummary(e))

	case engine.EventEditApplied:
		l.tag("edit", fmt.Sprintf("%s applied (%s)", e.Path, e.Mode))

	case engine.EventEditRejected:
		l.tag("edit", fmt.Sprintf("%s rejected: %s", e.Path, e.Reason))
		l.indent(e.Text)

	case engine.EventSyntaxGate:
		l.tag("syntax", syntaxSummary(e))

	case engine.EventPermissionRequested:
		// The question is [Loop.Ask], and by the time this lands it has already
		// been asked: the gate journals the request and then blocks on the
		// answer. Rendering it here would put it on screen twice.

	case engine.EventPermissionDecided:
		l.tag("perm", fmt.Sprintf("%s (%s)", e.Decision, e.Source))

	case engine.EventTurnSnapshot:
		l.tag("snap", e.Ref)

	case engine.EventVerification:
		l.tag("verify", fmt.Sprintf("%s exit %d", strings.Join(e.Command, " "), e.ExitCode))

	case engine.EventTurnCancelled:
		l.tag("cancelled", cancelSummary(e))

	case engine.EventSessionEnded:
		l.tag("session", fmt.Sprintf("ended: %s (exit %d)", e.Reason, e.ExitCode))

	case engine.EventSessionForked:
		// Detail is the source session id, ExitCode the turn branched from
		// (see engine.eventOf's own mapping) and Size how many of its events
		// were copied in. KAN-941 is what wires a --fork flag up to this; the
		// rendering exists now so a kind the record can already hold never
		// falls through to the "no rendering for" case below.
		l.tag("fork", fmt.Sprintf("from %s turn %d (%d events copied)", e.Detail, e.ExitCode, e.Size))

	case engine.EventAskRequested:
		// The question is [Loop.AnswerAsk], and by the time this lands the
		// dispatch closure has already appended it and is about to call the
		// Answerer: docs/adr/0009-ask-tool-contract.md decision 3 writes
		// AskRequested *before* the Answerer runs, deliberately, so a turn
		// cancelled mid-question still leaves it on the record. Rendering the
		// question here would print it before AnswerAsk's own renderAsk gets
		// to, the same "already handled, would show twice" reasoning
		// EventPermissionRequested's own case gives.

	case engine.EventProjectInstructionsLoaded:
		// A genuinely new session's bootstrap found the repository's own
		// AGENTS.md and fed it into the conversation before the first real
		// user turn (KAN-1025) — see journal.ProjectInstructionsLoaded's own
		// doc comment. Rendered once, the same register EventSessionStarted's
		// own line uses, so a user watching the session sees what it was
		// shown without having to open the record.
		l.tag("instructions", fmt.Sprintf("%s (%s)", e.Path, humanBytes(e.Size)))

	case engine.EventAskAnswered:
		if e.Reason == "refused" {
			l.tag("ask", fmt.Sprintf("unanswered (%s): %s", e.Source, e.Text))
		} else {
			l.tag("ask", fmt.Sprintf("answered (%s)", e.Source))
		}

	case engine.EventUnknown:
		// The journal's compatibility promise, carried to the screen. A build
		// that cannot name the type can still say that something happened and
		// what it was called, which beats silently skipping a line of the
		// record.
		l.tag("event", e.Reason+": written by a newer build, kept whole in the record")

	case engine.EventUnspecified:
		fallthrough
	default:
		// Loud rather than dropped. An event the journal holds and the user was
		// never told about is the one failure this surface exists to prevent.
		l.Fail(fmt.Sprintf("repl: no rendering for %s", e.Kind))
	}
}

// delta prints a fragment of the reply as it arrives.
//
// The text is remembered as well as printed, because it is a *prediction* —
// see [Loop.assistant].
func (l *Loop) delta(e engine.Event) {
	if e.Text == "" {
		return
	}
	if e.Reason == "reasoning" {
		// Reasoning is not the answer, and the journal keeps the two apart. It
		// is printed dim and deliberately not remembered, so it cannot be
		// mistaken for a prefix of the message that is coming.
		l.out.text(l.out.dim(e.Text))
		return
	}
	l.streamed.WriteString(e.Text)
	l.out.text(e.Text)
}

// assistant prints the message the journal recorded, minus whatever already
// reached the screen as deltas.
//
// The streamed bytes are an optimistic prefix of the record: when they are a
// prefix only the remainder is printed, and when they are not, the record is
// printed in full behind a marker, because the record is what the session
// actually was. Either way what the user has read is exactly what the journal
// holds — which is what lets this surface stream at all without keeping a
// second account of the reply.
func (l *Loop) assistant(text string) {
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

// cancelSummary renders an interrupted turn: what the user did, and what it
// caught in flight.
//
// The phase comes from the record — journal.TurnCancelled.Phase — rather than
// from anything this package observed. Until KAN-857 there was a Loop.Cancelled
// method here saying "turn interrupted" from what the *surface* saw, because a
// cancelled turn left a ProviderRequest with no ProviderResponse and there was
// nothing in the journal to derive a line from. There is now, so this is a
// rendering of the record like every other line in this file.
func cancelSummary(e engine.Event) string {
	what := ""
	switch e.Reason {
	case "provider_stream":
		what = " while the reply was arriving"
	case "tool_call":
		what = " while a tool was running"
	case "verification":
		what = " while verification was running"
	case "between_steps":
	default:
		// A phase written by a newer build. Named rather than dropped, for the
		// same reason EventUnknown is rendered at all.
		what = " during " + e.Reason
	}
	return "turn interrupted" + what + "; the session is still open"
}

// The three methods below are the only things this surface prints that are not
// derived from the record. Each is about the surface itself rather than about
// the session.

// Notice prints something the surface wants to say.
func (l *Loop) Notice(text string) { l.tag("note", text) }

// Fail prints a failure of the surface, or one the session could not record.
func (l *Loop) Fail(text string) { l.out.line(l.out.bold("[error] ") + text) }

// Stopped reports a turn that ended for a reason other than a clean stop.
//
// It renders an [engine.Stop], which summarises events the loop already wrote
// rather than being one of its own; the SessionEnded that eventually carries
// the same reason is the record's version.
func (l *Loop) Stopped(stop engine.Stop, detail string) {
	if detail == "" {
		detail = stop.Reason()
	}
	l.tag("stopped", detail)
}

// Progress paints a status on the current line only. "" clears it.
func (l *Loop) Progress(status string) { l.out.setProgress(status) }

// tag writes one tagged line: "[tool] read_file: ok".
//
// The tag is a fixed, greppable, ASCII prefix. Not a unicode glyph and not a
// colour alone — a piped session has neither, and a line whose meaning survives
// only in the terminal is a line the transcript lost.
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
// It summarises the size and never the content. Nothing is truncated here
// because nothing is printed here: the full output is in the record, whole, and
// a surface that pasted 64 KiB of test output into the scrollback would be
// unusable rather than more honest.
func resultSummary(e engine.Event) string {
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
// distinct from "passed" (ADR-0006 §4).
func syntaxSummary(e engine.Event) string {
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
