package engine

import (
	"errors"
	"fmt"
	"slices"

	"github.com/leejianrong/kopicode/internal/provider"
)

// Assembler builds the message list for the next provider request.
//
// It is SLICE-1 affordance E2 — system prompt, history, tool results — and it is
// **naive full history**: every message the session has produced goes into every
// request, oldest first, and nothing is dropped, summarised, windowed or
// clipped. That is a decision rather than a gap (docs/SLICE-1.md §Risks): a
// compaction strategy chosen without session data is a guess, and the journal is
// exactly the data needed to choose one later. The corpus's 20-turn bound is what
// makes it survivable, and cost grows quadratically with turns, so read [Size]
// and journal what the provider reports rather than waiting to be surprised.
//
// Two consequences worth stating, because both are easy to "fix" wrongly:
//
//   - **Nothing here truncates.** CLAUDE.md's rule about tool output is not only
//     about the journal; a context assembler that drops a large tool result to
//     save tokens is the same boundary crossed from the inside, and it removes
//     the diagnostic output that justified a fix from the one place the model
//     could still act on it. TestAssemblerNeverTruncates holds this.
//   - **Nothing here journals.** The engine loop (KAN-789) owns the journal.
//     This type returns data — the messages, and the exact sizes of them — that
//     the loop can journal without inventing anything.
//
// The system prompt is an input. This card does not write one, and there is no
// system prompt anywhere in the repo yet: NewAssembler takes whatever the harness
// config resolves (ADR-0007 puts it inside the harness config hash, so it is a
// per-arm value, not a constant this package may pick).
//
// # Determinism
//
// A replayed session must produce a byte-identical journal, so the assembled
// request must be a pure function of what was appended. There is no map anywhere
// in this type: history is a slice, and the answered/unanswered bookkeeping for
// tool calls is a slice too, so nothing here can be reordered by a runtime whose
// map iteration order is deliberately randomised.
//
// # Concurrency
//
// An Assembler belongs to one loop and is not safe for concurrent use. The loop
// is single-threaded over its own turn; the concurrency in this system is inside
// the provider stream and the tool subprocesses, neither of which touches this.
type Assembler struct {
	system string
	msgs   []provider.Message
	// pending is the native tool calls the latest assistant message made, in
	// wire order, with whether each has been answered. A slice and not a map:
	// this feeds an output path.
	pending []pendingCall
}

type pendingCall struct {
	id       string
	answered bool
}

// Errors from [Assembler.AppendToolResult]. A tool result that cannot be placed
// is refused rather than placed somewhere plausible: an OpenAI-compatible
// provider rejects a tool message whose tool_call_id it never issued, so the
// alternative to this error is a 400 several layers away from the mistake.
var (
	// ErrUnknownToolCall is a result naming a call id the latest assistant
	// message did not make.
	ErrUnknownToolCall = errors.New("engine: no such pending tool call")
	// ErrToolCallAnswered is a second result for a call already answered.
	ErrToolCallAnswered = errors.New("engine: tool call already answered")
)

// NewAssembler returns an assembler that opens every request with systemPrompt.
//
// An empty prompt emits no system message at all, rather than an empty one: a
// blank system turn is a wire artefact that a model may or may not tolerate, and
// "no system prompt configured" is a thing a test wants to be able to say.
func NewAssembler(systemPrompt string) *Assembler {
	return &Assembler{system: systemPrompt}
}

// SystemPrompt returns the prompt this assembler was built with.
func (a *Assembler) SystemPrompt() string { return a.system }

// AppendUser adds a user turn.
//
// This is the human's message, and it is also how the loop feeds back anything
// the wire has no better role for: a repair message after a malformed tool call,
// or a tool result for a call that arrived on a text route (see
// [Assembler.AppendToolResult]).
func (a *Assembler) AppendUser(text string) {
	a.msgs = append(a.msgs, provider.Message{Role: provider.RoleUser, Content: text})
}

// AppendAssistant adds the model's reply.
//
// Two fields of the reply are used and the rest are deliberately not.
//
// Content and ToolCalls go into the message. **ToolCalls must be the native
// calls the provider itself issued** — [provider.Reply.ToolCalls] — and this
// method takes the whole reply rather than a call slice so that a caller cannot
// hand it something else. A call the extractor found in a fenced block or an XML
// tag has no provider-issued id ([parse.ToolCall.ID] is empty there, and the
// engine assigns one for its own records); re-encoding it as a native call would
// invent wire history that never happened, and the text it came from is already
// in Content. So text-route calls travel back in the reply's text, exactly as
// the model wrote them, and their results come back as user turns.
//
// Reasoning is dropped. The journal keeps it as a separate ThinkingBlock
// precisely because it "is not fed back the same way", and feeding a model its
// own reasoning tokens back as assistant content is a different harness — one
// that would need measuring rather than assuming.
func (a *Assembler) AppendAssistant(reply provider.Reply) {
	msg := provider.Message{
		Role:      provider.RoleAssistant,
		Content:   reply.Content,
		ToolCalls: slices.Clone(reply.ToolCalls),
	}
	a.msgs = append(a.msgs, msg)

	a.pending = a.pending[:0]
	for _, tc := range reply.ToolCalls {
		a.pending = append(a.pending, pendingCall{id: tc.ID})
	}
}

// AppendToolResult adds the output of one tool call.
//
// callID selects how the result travels, because the wire gives no choice:
//
//   - A **non-empty** id must name an unanswered call from the latest assistant
//     message. The result becomes a tool-role message echoing that id. An id
//     that names nothing is [ErrUnknownToolCall]; a second result for the same
//     id is [ErrToolCallAnswered]. Both fail closed — a tool-role message the
//     provider cannot match is a request error, not a degraded request.
//   - An **empty** id means the call came from a text route, where there is no
//     id to echo. The result becomes a user turn. This is not a fallback for a
//     lost id: [parse.ToolCall.ID] is empty exactly when the model wrote the
//     call as text, and that is the case the harness is built to serve.
//
// output is passed through byte for byte. It is not labelled, wrapped, headed or
// clipped: how a tool result should be *worded* to a model is prompt design and
// belongs with the system prompt, and clipping it is the specific failure this
// project designs out. Oversized output spills to a blob in the journal; what
// the model sees is whole.
func (a *Assembler) AppendToolResult(callID, output string) error {
	if callID == "" {
		a.AppendUser(output)
		return nil
	}
	i := slices.IndexFunc(a.pending, func(p pendingCall) bool { return p.id == callID })
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrUnknownToolCall, callID)
	}
	if a.pending[i].answered {
		return fmt.Errorf("%w: %q", ErrToolCallAnswered, callID)
	}
	a.pending[i].answered = true
	a.msgs = append(a.msgs, provider.Message{
		Role:       provider.RoleTool,
		Content:    output,
		ToolCallID: callID,
	})
	return nil
}

// Unanswered returns the ids of native tool calls from the latest assistant
// message that have no result yet, in wire order.
//
// It is here because the loop has to hold an invariant this type can see and it
// cannot: an OpenAI-compatible provider refuses a request whose assistant
// message made calls that nothing answers. Assert it is empty before sending.
func (a *Assembler) Unanswered() []string {
	var ids []string
	for _, p := range a.pending {
		if !p.answered {
			ids = append(ids, p.id)
		}
	}
	return ids
}

// Messages returns the assembled conversation, oldest first: the system prompt
// if there is one, then every message appended, unmodified.
//
// The result is a fresh slice with fresh tool-call slices, so a caller holding
// on to a previous request cannot rewrite this one's history.
func (a *Assembler) Messages() []provider.Message {
	out := make([]provider.Message, 0, len(a.msgs)+1)
	if a.system != "" {
		out = append(out, provider.Message{Role: provider.RoleSystem, Content: a.system})
	}
	for _, m := range a.msgs {
		m.ToolCalls = slices.Clone(m.ToolCalls)
		out = append(out, m)
	}
	return out
}

// Size is what the next request costs, measured rather than guessed.
//
// Bytes and Messages are facts about the assembled request. The token count is
// not one — see [Size.EstimatedTokens].
type Size struct {
	// Messages is how many messages the request carries, system prompt
	// included.
	Messages int
	// Bytes is the total length of the content the request carries: every
	// message's text, plus each tool call's id, name and raw arguments, plus
	// each tool result's call id. It is what this package can count exactly.
	//
	// It is not the size of the HTTP body — that is the client's business and
	// depends on a JSON encoding this package does not perform.
	Bytes int
}

// BytesPerTokenEstimate is the divisor behind [Size.EstimatedTokens]: roughly
// four bytes of English-and-code per token, which is the usual rule of thumb for
// a byte-pair vocabulary and is nothing better than that.
const BytesPerTokenEstimate = 4

// EstimatedTokens is Bytes divided by [BytesPerTokenEstimate], and the name is
// the whole of the contract.
//
// **What it is:** a cheap, deterministic order-of-magnitude figure for watching
// full-history context grow across a session, available before a request is
// sent.
//
// **What it is not:** a token count. This repo has no tokenizer, will not link
// one (ADR-0001 keeps the binary dependency-free and every model on the roadmap
// has a different vocabulary), and cannot know a provider's prompt accounting
// before the provider does it. The authoritative number is
// [provider.Usage] on the reply, which the loop journals as
// journal.ProviderResponse.Tokens — that is what a per-turn token record should
// be built from, and it is where a budget decision has to come from. Deciding
// "this will fit" from this method would be exactly the fabricated precision
// that makes a bad decision look measured.
func (s Size) EstimatedTokens() int { return s.Bytes / BytesPerTokenEstimate }

// Size measures what [Assembler.Messages] would return, without building it.
func (a *Assembler) Size() Size {
	var sz Size
	if a.system != "" {
		sz.Messages++
		sz.Bytes += len(a.system)
	}
	for _, m := range a.msgs {
		sz.Messages++
		sz.Bytes += len(m.Content) + len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			sz.Bytes += len(tc.ID) + len(tc.Name) + len(tc.Arguments)
		}
	}
	return sz
}
