package engine

import (
	"fmt"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/provider"
)

// This file is KAN-939's half of the loop: rebuilding [Assembler]'s message
// history from a resumed session's prior events, so the first new turn's
// request carries the whole prior conversation — CLAUDE.md's "naive full
// history, nothing truncates" applied across a process boundary rather than
// only across turns within one process.
//
// # What replays exactly
//
// The mapping walks the journal forward, in seq order, and drives exactly the
// [Assembler] methods [Engine.Run] itself would have called:
//
//   - [journal.UserMessage] -> [Assembler.AppendUser].
//   - [journal.ProviderResponse], together with the [journal.AssistantMessage]
//     that follows it when the reply carried prose, and the
//     [journal.ToolCallParsed] events (route "native") that follow when the
//     reply carried native tool calls -> one [Assembler.AppendAssistant], with
//     Content and ToolCalls reconstructed exactly as the wire wrote them.
//   - Each [journal.ToolResult] that follows a dispatched call ->
//     [Assembler.AppendToolResult] when the call it answers parsed on the
//     native route (echoing the call's own id, as the original run did), or
//     [Assembler.AppendUser] when it answers a text-route call (fenced JSON or
//     XML), matching [Engine.observe]'s own choice of route for the reason
//     given on [Assembler.AppendToolResult]: a text-route call carries no
//     provider-issued id to echo.
//   - The feedback text of a [journal.ToolCallRepaired], a
//     [journal.ToolCallFailed], or a blocking [journal.VerificationRun]
//     (Skip == "" and ExitCode > 0) -> [Assembler.AppendUser], or
//     [Assembler.AppendToolResult] per unanswered call id when the assistant
//     message that produced it left native calls unanswered — mirroring
//     [Engine.observe] exactly.
//   - [journal.ThinkingBlock] is dropped, matching [Assembler.AppendAssistant]'s
//     own contract: reasoning is never fed back.
//   - Every other payload type — [journal.SessionStarted], [journal.ProviderRequest],
//     [journal.ToolCallRequested], [journal.EditApplied], [journal.EditRejected],
//     [journal.SyntaxGateRun], [journal.PermissionRequested],
//     [journal.PermissionDecided], [journal.TurnSnapshot], [journal.ProviderRetried],
//     [journal.TurnCancelled], [journal.SessionEnded], and anything this build
//     does not recognise — carries nothing the assembler needs and is skipped.
//
// # The one documented gap
//
// A [journal.ToolCallRepaired] or [journal.ToolCallFailed] event is journaled
// only when the repair loop could not turn a reply into dispatchable calls,
// which means no [journal.ToolCallParsed] was ever written for that reply.
// Ordinarily that is exactly the case where the original reply's native
// ToolCalls were empty too — the model wrote its call as prose on a text
// route, or asked for nothing dispatchable at all — and this replay's
// reconstruction (no pending native calls for that reply) matches the
// original run exactly, because [Engine.observe] then also finds nothing
// unanswered and falls back to [Assembler.AppendUser].
//
// It is possible, though nothing in this codebase's corpus has been observed
// to trigger it, for a reply to carry well-formed *native* tool calls whose
// arguments nonetheless fail [parse]'s own validation (an unknown tool name,
// a schema mismatch) — parse.Extract's repair loop requires the *whole* reply
// to be dispatchable before dispatching any of it, so that reply's real
// native call ids are never written anywhere in the journal; only the
// reply's raw text is (journal.ToolCallRequested.Raw). In that one case, this
// replay cannot recover the original ids and answers the repair feedback as a
// user turn instead of as N tool-result messages echoing them, which is a
// different message shape than the interrupted process would have sent next.
// Closing this gap needs a schema change — recording the native call array
// itself alongside the repair/failure event — and is left to a follow-up
// rather than decided here; see this card's PR for the argument.

// replayHistory rebuilds asm's message history from a resumed session's prior
// events, so the caller's next request carries the whole prior conversation.
//
// events must be in seq order — the order [journal.FileJournal.Read] and
// [journal.ReadSession] both yield — and asm must be freshly built, with no
// history of its own: this function only ever appends. It is a pure function
// of the events handed to it; the caller decides where they came from and
// whether they precede or follow the point a new session's own SessionStarted
// was appended (they must precede it, or the "history" would include the very
// resume this call is preparing for).
func replayHistory(asm *Assembler, events []journal.Event) error {
	var buf *attemptBuffer

	flush := func() error {
		if buf == nil {
			return nil
		}
		b := buf
		buf = nil
		return b.commit(asm)
	}

	for _, ev := range events {
		switch p := ev.Payload.(type) {
		case journal.UserMessage:
			if err := flush(); err != nil {
				return err
			}
			asm.AppendUser(p.Text.Inline)

		case journal.ProviderRequest:
			// The start of a new attempt. Whatever the previous ProviderResponse
			// buffered — a dispatched reply's calls and results, or a
			// repaired/failed reply's feedback — is now complete.
			if err := flush(); err != nil {
				return err
			}

		case journal.ProviderResponse:
			if err := flush(); err != nil {
				return err
			}
			buf = &attemptBuffer{}

		case journal.ThinkingBlock:
			// Dropped: see this file's doc comment. Reasoning is never fed back.

		case journal.AssistantMessage:
			if buf == nil {
				// A well-formed journal never reaches this: AssistantMessage
				// always follows the ProviderResponse from the same call().
				// Defensive rather than fatal, because a payload type this
				// build does not fully understand must not abort a resume.
				continue
			}
			buf.content = p.Text.Inline

		case journal.ToolCallParsed:
			if buf != nil {
				buf.parsed = append(buf.parsed, p)
			}

		case journal.ToolResult:
			if buf != nil {
				buf.results = append(buf.results, p)
			}

		case journal.ToolCallRepaired:
			if buf != nil {
				buf.feedback = append(buf.feedback, p.Error.Inline)
			}

		case journal.ToolCallFailed:
			if buf != nil {
				buf.feedback = append(buf.feedback, p.Detail.Inline)
			}

		case journal.VerificationRun:
			// Blocks() in internal/verify's own terms: a command that ran and
			// exited non-zero. Skip is set only when Outcome is NotRun, and
			// ExitCode is -1 for anything that did not conclude, so "Skip == ''
			// and ExitCode > 0" is exactly Outcome == Failed reconstructed from
			// the journal's own fields without importing internal/verify.
			if buf != nil && p.Skip == "" && p.ExitCode > 0 {
				buf.feedback = append(buf.feedback, p.Output.Inline)
			}

		default:
			// SessionStarted, ToolCallRequested, EditApplied, EditRejected,
			// SyntaxGateRun, PermissionRequested, PermissionDecided,
			// TurnSnapshot, ProviderRetried, TurnCancelled, SessionEnded, and
			// any UnknownPayload this build cannot decode: none of them carry
			// anything the assembler needs. See the file doc comment's list.
		}
	}

	return flush()
}

// attemptBuffer accumulates one provider attempt's events — from a
// ProviderResponse to the next attempt's ProviderRequest, or a new exchange's
// UserMessage, or the end of the journal — so [replayHistory] can commit one
// [Assembler.AppendAssistant] call with the complete native tool-call array
// the wire actually carried, rather than one incomplete call per
// [journal.ToolCallParsed] event it happens to see first.
//
// That completeness is why this buffers at all instead of driving the
// assembler event by event: dispatch journals a multi-call reply's calls
// interleaved with each call's own result
// (ToolCallParsed[0], ToolResult[0], ToolCallParsed[1], ToolResult[1], ...),
// but the original [Assembler.AppendAssistant] call already held the whole
// array before any of that dispatching began. Buffering the whole attempt and
// committing once reproduces that ordering.
type attemptBuffer struct {
	content string
	// parsed is every journal.ToolCallParsed this attempt's reply produced, in
	// dispatch order. Only the ones with Route == parse.RouteNative feed
	// Assembler.AppendAssistant's ToolCalls; the rest (fenced_json, xml) are
	// text-route calls the wire never carried an id for.
	parsed []journal.ToolCallParsed
	// results is every journal.ToolResult this attempt's dispatch produced, in
	// order.
	results []journal.ToolResult
	// feedback is the repair/failure/verification text this attempt's outcome
	// fed back to the model, in order. Ordinarily at most one of these fires
	// per attempt, but the slice makes no such assumption.
	feedback []string
}

// nativeRoute is the wire string [parse.Route.String] renders for
// [parse.RouteNative], compared against [journal.ToolCallParsed.Route]. The
// two packages share this vocabulary as a documented wire contract rather
// than a dependency edge — the same reason [journal.ToolCallParsed.ArgEncoding]
// and [journal.ToolResult.ErrorKind] are plain strings — but this package
// already imports internal/parse for [Catalogue], so referencing
// [parse.RouteNative] directly here is the source of truth rather than a
// third copy of the literal.
var nativeRoute = parse.RouteNative.String()

// commit replays one attempt's buffered events into asm: the assistant reply
// first, with its complete native tool-call array, then each result routed
// the way the original dispatch or observe call routed it.
func (b *attemptBuffer) commit(asm *Assembler) error {
	var calls []provider.ToolCall
	for _, p := range b.parsed {
		if p.Route != nativeRoute {
			continue
		}
		calls = append(calls, provider.ToolCall{ID: p.CallID, Name: p.Tool, Arguments: p.Args})
	}
	asm.AppendAssistant(provider.Reply{Content: b.content, ToolCalls: calls})

	for _, r := range b.results {
		if b.isNative(r.CallID) {
			if err := asm.AppendToolResult(r.CallID, r.Output.Inline); err != nil {
				return fmt.Errorf("engine: resume replay: placing the result of call %q: %w", r.CallID, err)
			}
			continue
		}
		asm.AppendUser(r.Output.Inline)
	}

	for _, text := range b.feedback {
		unanswered := asm.Unanswered()
		if len(unanswered) == 0 {
			asm.AppendUser(text)
			continue
		}
		for _, id := range unanswered {
			if err := asm.AppendToolResult(id, text); err != nil {
				return fmt.Errorf("engine: resume replay: answering call %q with feedback: %w", id, err)
			}
		}
	}
	return nil
}

// isNative reports whether callID names a call this attempt's reply issued on
// the native route, by looking it up among the buffered ToolCallParsed events.
// The set is at most a handful of calls, so a linear scan needs no index.
func (b *attemptBuffer) isNative(callID string) bool {
	for _, p := range b.parsed {
		if p.CallID == callID {
			return p.Route == nativeRoute
		}
	}
	return false
}
