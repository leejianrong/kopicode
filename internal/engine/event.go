package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
)

// EventKind says what an [Event] is about.
//
// With one exception the vocabulary is the journal's: each kind names exactly
// one payload type in internal/journal, and an [Event] carrying it is that
// payload's fields copied across. The exception is [EventDelta] and it is
// marked as such where it is declared.
//
// It is a distinct type rather than journal.Type re-exported, for the reason
// [Event] is a distinct type: see that comment.
type EventKind uint8

const (
	// EventUnspecified is the zero value. No event carries it, and a surface
	// that receives one has been handed a value nobody built.
	EventUnspecified EventKind = iota

	EventSessionStarted
	EventUserMessage
	EventAssistantMessage
	EventThinking
	EventProviderRequest
	EventProviderRetried
	EventProviderResponse
	EventToolCallRequested
	EventToolCallParsed
	EventToolCallRepaired
	EventToolCallFailed
	EventToolResult
	EventEditApplied
	EventEditRejected
	EventSyntaxGate
	EventPermissionRequested
	EventPermissionDecided
	EventTurnSnapshot
	EventVerification
	EventTurnCancelled
	EventSessionEnded
	EventSessionForked
	EventAskRequested
	EventAskAnswered

	// EventUnknown is an event this build has no typed payload for, preserved
	// by the journal's compatibility promise and passed through here rather
	// than dropped. Text holds the payload bytes.
	EventUnknown

	// EventDelta is the one kind with no journal payload behind it: a fragment
	// of the model's reply, as it arrives.
	//
	// It is on this stream rather than on a callback of its own because a
	// surface needs the deltas and the record *in order*, and two seams cannot
	// promise that. What keeps it from being a second record is [Event.Seq],
	// which is zero here and non-zero on everything else: a delta is explicitly
	// not a thing that happened as far as the journal is concerned, it is a
	// thing that is happening. When the reply lands, EventAssistantMessage
	// carries what was recorded, and a surface prints the difference — see the
	// note on [Event.Seq].
	EventDelta
)

var eventKindText = map[EventKind]string{
	EventUnspecified:         "unspecified",
	EventSessionStarted:      "session_started",
	EventUserMessage:         "user_message",
	EventAssistantMessage:    "assistant_message",
	EventThinking:            "thinking",
	EventProviderRequest:     "provider_request",
	EventProviderRetried:     "provider_retried",
	EventProviderResponse:    "provider_response",
	EventToolCallRequested:   "tool_call_requested",
	EventToolCallParsed:      "tool_call_parsed",
	EventToolCallRepaired:    "tool_call_repaired",
	EventToolCallFailed:      "tool_call_failed",
	EventToolResult:          "tool_result",
	EventEditApplied:         "edit_applied",
	EventEditRejected:        "edit_rejected",
	EventSyntaxGate:          "syntax_gate",
	EventPermissionRequested: "permission_requested",
	EventPermissionDecided:   "permission_decided",
	EventTurnSnapshot:        "turn_snapshot",
	EventVerification:        "verification",
	EventTurnCancelled:       "turn_cancelled",
	EventSessionEnded:        "session_ended",
	EventSessionForked:       "session_forked",
	EventAskRequested:        "ask_requested",
	EventAskAnswered:         "ask_answered",
	EventUnknown:             "unknown",
	EventDelta:               "delta",
}

// String returns the kind's name.
func (k EventKind) String() string {
	if s, ok := eventKindText[k]; ok {
		return s
	}
	return fmt.Sprintf("event_kind(%d)", uint8(k))
}

// Event is one thing a surface may render.
//
// # Why this type exists at all
//
// A front end may import internal/engine, internal/bench and internal/build and
// nothing else (ADR-0003 decision 3, enforced by internal/arch over test files
// too). So it cannot name journal.Event, cannot type-switch on
// journal.Payload, and cannot be handed the record directly. This is the shape
// that crosses the boundary: declared here, built here, and made of nothing but
// strings, ints and bools.
//
// # Why it is not a second transcript
//
// It is **derived**, and derived at the moment of writing. Every Event but
// [EventDelta] is produced by a decorator around the [journal.Journal] the loop
// appends to: the record is written first, and only what was actually recorded
// is announced. There is no path by which a surface can be told about something
// the journal does not hold, which is the property ADR-0002 decision 2 is
// about, and it is the reason this is a tee rather than a hook in the loop.
//
// # Why the fields are flat
//
// They are copied off a tagged union and rendered to lines. A second tagged
// union here would be a translation table with nothing in it, and the field
// names below are the journal payload field names, so the copy is checkable by
// eye. Fields not relevant to a kind are zero.
type Event struct {
	// Kind says which journal payload this came from.
	Kind EventKind

	// Seq is the journal sequence number of the event this was derived from,
	// and it is **zero exactly for [EventDelta]**.
	//
	// That is the whole of the "is this in the record" question, and it is why
	// a surface can stream optimistically without inventing anything: text
	// arriving with Seq 0 is a prediction, and the EventAssistantMessage that
	// follows — with a real Seq — is what the session actually holds. A surface
	// prints the difference rather than choosing between them.
	Seq uint64
	// Turn is the turn the event belongs to, 0 for events outside one.
	Turn int

	// Text is the message body: assistant prose, a thinking block, a rejection
	// detail, an unknown payload's bytes.
	Text string
	// Tool is the tool name as the model called it.
	Tool string
	// Path is the file an edit or a syntax check was about.
	Path string
	// Detail is the short form: a command line, an argument summary, a session
	// id, a consent target.
	Detail string
	// Reason is why: a rejection reason, a stop reason, a policy rule, a tool
	// result's error kind, a cancellation's phase.
	Reason string
	// Mode is an edit's "anchored" or "fuzzy". Journaled on every edit because
	// one fuzzy edit classifies the whole session unattributed (SLICE-1 §4), so
	// a surface that hid it would hide the reason.
	Mode string
	// Ref is a snapshot's shadow ref.
	Ref string
	// Command is a verification command's argv.
	Command []string
	// Decision is "allow", "deny" or "allow_session"; Source is "user" or
	// "policy". A bench run's auto-approval must not read like a human saying
	// yes, which is why the pair travels together.
	Decision string
	Source   string
	// Checker is the syntax gate's command, "" when none was available.
	Checker string
	// Ran reports whether the syntax gate ran. A gate that did not run must not
	// read as a pass (ADR-0006 §4), so it is carried rather than inferred from
	// an exit code of zero.
	Ran bool
	// ExitCode is a process exit code, meaningful when HasExitCode is set.
	ExitCode    int
	HasExitCode bool
	// Size is the full length in bytes of a value the record holds whole. It is
	// the *record's* size, not the length of Text: content over the journal's
	// spill threshold lives in a blob, and a surface summarising by size is
	// pointing at the record rather than reprinting it.
	Size int64
}

// Observer receives events as they are recorded. It is called synchronously on
// the goroutine running the turn, in record order; a slow one slows the turn,
// which is the honest trade and the same one [Config.Stream] makes.
type Observer func(Event)

// observed wraps a journal so that every event it accepts is announced.
//
// The order is deliberate and is the whole design: append first, announce
// second, announce nothing when the append failed. A decorator that announced
// first would be a surface printing things the record does not hold, which is
// the parallel transcript ADR-0002 forbids, arriving through the one door that
// looks like plumbing.
type observed struct {
	journal.Journal
	observer Observer
}

// tee returns j with obs attached, or j unchanged when there is nothing to
// announce to.
func tee(j journal.Journal, obs Observer) journal.Journal {
	if obs == nil {
		return j
	}
	return &observed{Journal: j, observer: obs}
}

func (o *observed) Append(ctx context.Context, turn int, payload journal.Payload) (journal.Event, error) {
	ev, err := o.Journal.Append(ctx, turn, payload)
	if err != nil {
		return ev, err
	}
	o.observer(eventOf(ev))
	return ev, nil
}

// deltaEvent is the one Event with no record behind it. Seq is zero and says so.
func deltaEvent(turn int, d provider.Delta) Event {
	kind := EventDelta
	reason := d.Kind.String()
	if d.Kind == provider.DeltaReasoning {
		// Reasoning is kept apart from the answer in the journal and is
		// rendered differently, so the distinction has to survive the crossing.
		reason = "reasoning"
	}
	return Event{Kind: kind, Turn: turn, Text: d.Text, Reason: reason}
}

// eventOf maps a recorded journal event onto the surface's vocabulary.
//
// Every payload type in the journal's registry has a case here, and
// TestEveryJournalPayloadCrosses fails when one does not: a payload with no
// case would reach a surface as [EventUnspecified], which is a fact the record
// holds and the user was never told — the specific failure this stream exists
// to prevent.
func eventOf(ev journal.Event) Event {
	base := Event{Seq: ev.Seq, Turn: ev.Turn}

	switch p := ev.Payload.(type) {
	case journal.SessionStarted:
		base.Kind = EventSessionStarted
		base.Detail = ev.SessionID
		base.Text = p.ModelID
		base.Reason = p.HarnessConfigHash

	case journal.UserMessage:
		base.Kind = EventUserMessage
		base.Text, base.Size = text(p.Text)

	case journal.AssistantMessage:
		base.Kind = EventAssistantMessage
		base.Text, base.Size = text(p.Text)

	case journal.ThinkingBlock:
		base.Kind = EventThinking
		base.Text, base.Size = text(p.Text)

	case journal.ProviderRequest:
		base.Kind = EventProviderRequest
		base.Detail = p.ModelID
		base.ExitCode, base.HasExitCode = p.Attempt, true

	case journal.ProviderRetried:
		base.Kind = EventProviderRetried
		base.Reason = p.Cause
		base.Detail = fmt.Sprintf("try %d of %d", p.Try, p.OfTries)
		base.ExitCode, base.HasExitCode = p.Attempt, true
		base.Size = p.DelayMS

	case journal.ProviderResponse:
		base.Kind = EventProviderResponse
		base.Reason = p.FinishReason
		base.Source = p.ServedBy
		base.Size = int64(p.Tokens.Total)

	case journal.ToolCallRequested:
		base.Kind = EventToolCallRequested
		base.Detail = p.CallID
		base.Text, base.Size = text(p.Raw)

	case journal.ToolCallParsed:
		base.Kind = EventToolCallParsed
		base.Tool = p.Tool
		base.Detail = summarise(string(p.Args))
		base.Reason = p.Route
		base.Size = int64(len(p.Args))

	case journal.ToolCallRepaired:
		base.Kind = EventToolCallRepaired
		base.Reason = p.Classification
		base.ExitCode, base.HasExitCode = p.Attempt, true
		base.Text, base.Size = text(p.Error)

	case journal.ToolCallFailed:
		base.Kind = EventToolCallFailed
		base.Tool = p.Tool
		base.Reason = p.Reason
		base.Text, base.Size = text(p.Detail)

	case journal.ToolResult:
		base.Kind = EventToolResult
		base.Tool = p.Tool
		base.Reason = p.ErrorKind
		base.Size = p.Output.Size
		if p.ExitCode != nil {
			base.ExitCode, base.HasExitCode = *p.ExitCode, true
		}

	case journal.EditApplied:
		base.Kind = EventEditApplied
		base.Path = p.Path
		base.Mode = p.Mode
		base.Text, base.Size = text(p.Diff)

	case journal.EditRejected:
		base.Kind = EventEditRejected
		base.Path = p.Path
		base.Mode = p.Mode
		base.Reason = p.Reason
		base.Text, base.Size = text(p.Detail)

	case journal.SyntaxGateRun:
		base.Kind = EventSyntaxGate
		base.Path = p.Path
		base.Checker = p.Checker
		base.Ran = p.Ran
		base.ExitCode, base.HasExitCode = p.ExitCode, true
		base.Text, base.Size = text(p.Output)

	case journal.PermissionRequested:
		base.Kind = EventPermissionRequested
		base.Tool = p.Tool
		base.Reason = p.Reason
		base.Detail, base.Size = text(p.Detail)
		base.Mode = p.Kind

	case journal.PermissionDecided:
		base.Kind = EventPermissionDecided
		base.Decision = p.Decision
		base.Source = p.Source
		base.Reason = p.Reason

	case journal.TurnSnapshot:
		base.Kind = EventTurnSnapshot
		base.Ref = p.Ref
		base.Detail = p.Tree

	case journal.VerificationRun:
		base.Kind = EventVerification
		base.Command = p.Command
		base.Source = p.Source
		base.ExitCode, base.HasExitCode = p.ExitCode, true
		base.Text, base.Size = text(p.Output)

	case journal.TurnCancelled:
		base.Kind = EventTurnCancelled
		base.Reason = p.Phase
		base.Text, base.Size = text(p.Detail)

	case journal.SessionEnded:
		base.Kind = EventSessionEnded
		base.Reason = p.Reason
		base.ExitCode, base.HasExitCode = p.ExitCode, true
		base.Text, base.Size = text(p.Detail)

	case journal.SessionForked:
		base.Kind = EventSessionForked
		base.Detail = p.SourceSessionID
		base.ExitCode, base.HasExitCode = p.SourceTurn, true
		base.Size = int64(p.Copied)

	case journal.AskRequested:
		base.Kind = EventAskRequested
		base.Detail = p.CallID
		base.Text, base.Size = text(p.Question)
		// Context travels summarised, the same way ToolCallParsed.Reason
		// carries a route rather than a "why": Reason is this struct's
		// generic secondary string, not a semantic commitment to "cause",
		// and the full value stays in the journal for anything that needs
		// it whole.
		base.Reason = summarise(p.Context.Inline)

	case journal.AskAnswered:
		base.Kind = EventAskAnswered
		base.Detail = p.CallID
		base.Text, base.Size = text(p.Answer)
		base.Source = p.Source
		if p.Refused {
			base.Reason = "refused"
		}

	case journal.UnknownPayload:
		// The journal's compatibility promise, carried one layer further out. A
		// build that cannot name the type can still say that something
		// happened and what it was called, which is strictly better than a
		// surface silently skipping a line of the record.
		base.Kind = EventUnknown
		base.Reason = string(p.EventType)
		base.Text = string(p.Raw)
		base.Size = int64(len(p.Raw))

	default:
		base.Kind = EventUnspecified
		base.Reason = string(ev.Type())
	}

	return base
}

// text unpacks a journal.Text into the value and the full size the record
// holds.
//
// Inline is empty for a value that spilled to a blob and has not been fetched
// back, which is the append path — so a surface gets "" and a real Size, and
// says "68 KiB of output" rather than pretending there was none. That is the
// honest rendering: nothing is truncated here because nothing is *carried*
// here, and the whole value is in the record.
func text(t journal.Text) (string, int64) { return t.Inline, t.Size }

// summarise flattens a value onto one line for a Detail field.
//
// It is the one place this file shortens anything, and it shortens only a
// *summary* field — never Text, and never anything the record is the sole
// holder of. A surface wanting the whole value reads the journal.
func summarise(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 120
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
