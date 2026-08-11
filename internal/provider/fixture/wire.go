package fixture

import (
	"encoding/json"
	"fmt"
	"strings"
)

// This file models as much of OpenRouter's chat-completions wire format as it
// takes to check a fixture against itself. It is not the provider client:
// KAN-776 owns request construction, retries and the mapping onto
// parse.Message, and KAN-773 owns replay. Nothing here is imported by either
// yet, and if one of them wants these shapes it should move them rather than
// copy them.
//
// # Where each field was verified
//
// Every field below says whether it was confirmed against OpenRouter's
// published documentation or not. That distinction is the whole point of the
// honesty constraint on KAN-772: the fixtures are synthesised, so a field
// nobody checked is a field the loop will meet for the first time in
// production.
//
// Two doc sources were used and they do not agree with each other, which is
// itself worth knowing:
//
//   - The hand-written TypeScript types on the API reference overview
//     (https://openrouter.ai/docs/api/reference/overview). OpenRouter-flavoured:
//     it carries native_finish_reason and gen- prefixed ids.
//   - A generated OpenAPI schema on the chat-completion endpoint page. Visibly
//     OpenAI-derived boilerplate — chatcmpl- ids, no native_finish_reason, a
//     three-value finish_reason enum.
//
// Where they conflict the note says so and the fixtures follow the first,
// because it is the one written about this API rather than inherited from
// another.
//
// The structs are deliberately partial. Unknown fields are *not* rejected on
// the way in — an OpenRouter response carries more than this (logprobs,
// refusal, reasoning_details, cached-token and cost accounting) and a decoder
// that errored on them would fail the moment the provider adds one. The
// fixture's Body keeps the full bytes; these types read the parts a check
// needs.

// completion is the assembled non-streaming chat-completion object.
type completion struct {
	// ID is the generation id.
	//
	// PARTLY VERIFIED. Three OpenRouter-authored pages show a "gen-" prefix and
	// the fixtures follow it; the generated OpenAPI schema shows OpenAI's
	// "chatcmpl-" instead, and no page states the format normatively. Nothing
	// should branch on the prefix. The suffix in a hand-authored fixture is a
	// fixed literal, not a generated value.
	ID string `json:"id"`

	// Object is "chat.completion" for an assembled response and
	// "chat.completion.chunk" for a streamed one. VERIFIED.
	Object string `json:"object"`

	// Created is the unix timestamp the provider stamped. VERIFIED as a field.
	// In a hand-authored fixture the value is a frozen literal — see the
	// package doc on determinism.
	Created int64 `json:"created"`

	// Model is the model id that served the request. VERIFIED.
	Model string `json:"model"`

	// Provider is the upstream provider that actually answered, and is what
	// journal.ProviderResponse.ServedBy records so a result served outside the
	// declared pin can be discarded rather than adjusted (ADR-0005 §2).
	//
	// NOT VERIFIED AS DOCUMENTED, and this is the field to be most careful
	// with. It appears verbatim in OpenRouter's own streaming and mid-stream
	// error examples, so it is really on the wire — but it is absent from the
	// Response type, absent from the OpenAPI schema and absent from the
	// non-streaming example, and the two examples that do show it disagree on
	// casing ("OpenAI" versus "openai"). So: read it, record it, and never
	// compare it case-sensitively or require it to be present. [Validate]
	// allows a fixture to omit it and the shipped fixtures state the casing
	// they assume.
	Provider string `json:"provider,omitempty"`

	// Choices is the completion list. kopicode sends n=1 and expects exactly
	// one; [Validate] enforces that on a fixture rather than leaving the loop
	// to discover a second choice at run time.
	Choices []choice `json:"choices"`

	// Usage is the token accounting. On a non-streaming response it is always
	// present. On a stream it arrives only when the request asked for it — see
	// [assembleStream].
	Usage *usage `json:"usage,omitempty"`
}

// choice is one completion.
type choice struct {
	// Index is the choice's position.
	//
	// CONFLICTING: absent from the hand-written NonStreamingChoice type,
	// present in the OpenAPI schema and in both documented chunk examples. Real
	// responses carry it, so the fixtures do.
	Index int `json:"index"`

	// Message is the assembled assistant reply. Absent on a streamed chunk,
	// which carries Delta instead.
	Message *message `json:"message,omitempty"`

	// Delta is the incremental reply on a streamed chunk.
	Delta *delta `json:"delta,omitempty"`

	// FinishReason is OpenRouter's *normalised* stop reason, null until the
	// last chunk. VERIFIED, and the five values are exactly [finishReasons].
	FinishReason *string `json:"finish_reason"`

	// NativeFinishReason is the upstream provider's own raw stop reason, passed
	// through alongside the normalised one. VERIFIED as a real field of the
	// choice. Recorded because the two disagreeing is the kind of
	// provider-specific fact a pinned experiment wants to see; nothing in
	// kopicode should branch on it, because its vocabulary is the upstream's,
	// not OpenRouter's.
	NativeFinishReason *string `json:"native_finish_reason,omitempty"`
}

// finishReasons is OpenRouter's normalised finish_reason vocabulary.
//
// VERIFIED against the API reference overview. The generated OpenAPI schema
// lists only the first three, which is the incomplete source: "error" is
// confirmed by OpenRouter's own mid-stream error example.
//
// The empty string is here because a streamed chunk carries finish_reason null
// until the last one, and an assembled fixture reply that never reached a stop
// reason is a fixture, not a session.
var finishReasons = map[string]bool{
	"tool_calls":     true,
	"stop":           true,
	"length":         true,
	"content_filter": true,
	"error":          true,
}

// terminalFinishReasons are the reasons that end a turn cleanly rather than
// through a failure path. A two-turn session fixture has to end on one of
// these, or it is not a session that stopped — it is one that was cut off.
var terminalFinishReasons = map[string]bool{
	"stop": true,
}

// message is an assembled assistant reply.
type message struct {
	Role string `json:"role"`

	// Content is the assistant's text, typed `string | null`. VERIFIED that it
	// is null — not "" — on a reply that is only tool calls. It is a pointer so
	// the two are distinguishable on the way in, and [Validate] treats them
	// alike on the way out.
	Content *string `json:"content"`

	// ToolCalls is the native tool_calls array. Absent when the model replied
	// in text. VERIFIED.
	ToolCalls []toolCall `json:"tool_calls,omitempty"`

	// Reasoning is the reasoning-token text some models return. VERIFIED in
	// prose (the reasoning-tokens page) though absent from the message type;
	// there is also a richer choices[].message.reasoning_details[]. Modelled
	// only so a fixture carrying one is not silently ignored by the checks. The
	// journal keeps it as a ThinkingBlock, and no shipped fixture uses it yet.
	Reasoning *string `json:"reasoning,omitempty"`
}

// delta is one streamed increment of a message.
type delta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	Reasoning *string         `json:"reasoning,omitempty"`
	ToolCalls []toolCallDelta `json:"tool_calls,omitempty"`
}

// toolCall is one entry of an assembled tool_calls array.
//
// VERIFIED by example: {"id": …, "type": "function", "function": {"name": …,
// "arguments": …}}. The referenced FunctionCall type is named in the docs but
// never defined there, so the example is the authority.
type toolCall struct {
	// Index is the call's position.
	//
	// NOT VERIFIED for the assembled array — it appears in no OpenRouter type
	// and no OpenRouter example, in either direction. OpenAI omits it
	// non-streaming. It is optional here and never compared, and no shipped
	// fixture writes one.
	Index *int `json:"index,omitempty"`

	// ID is the provider-assigned call id, echoed back on the tool result.
	//
	// The docs' example is "call_abc123", but there is no statement that
	// OpenRouter normalises ids and in practice they are passed through from
	// the upstream, so their shape varies by provider. Treat the prefix as an
	// example and never validate it.
	ID string `json:"id"`

	// Type is "function". VERIFIED.
	Type string `json:"type"`

	Function function `json:"function"`
}

// function is a tool call's name and arguments.
type function struct {
	Name string `json:"name"`

	// Arguments is a JSON *string* holding JSON. VERIFIED by the documented
	// example, escaped quotes and all. internal/parse accepts both this and a
	// bare object because providers differ, and records which arrived
	// (parse.ArgEncoding) — so a fixture using the spec-correct string shape
	// exercises the path the spec says to expect, and the bare-object variant
	// belongs in a *recorded* fixture from a provider that really sends one,
	// not in a hand-written guess about which providers do.
	Arguments string `json:"arguments"`
}

// toolCallDelta is one streamed increment of a tool call.
//
// NOT VERIFIED, and this is the largest gap in the whole file. OpenRouter types
// the streaming delta's tool_calls as the same array as the non-streaming one —
// required id, required type, no index — which cannot be what real streaming
// traffic looks like, because a partial arguments fragment has no id to carry.
// OpenRouter publishes no streaming tool-call example and no fragment-
// accumulation guidance, and its one snippet on the subject appends deltas
// rather than merging them by index, which is a bug.
//
// So the shape below is the OpenAI streaming contract, assumed compatible: the
// first fragment carries index, id, type and the function name, and later
// fragments carry index and an arguments fragment only. The shipped fixtures
// say the same thing in their own notes. If a recording from the pinned
// provider disagrees, the recording wins — and note that upstreams vary here,
// with some emitting a whole tool call in a single delta.
type toolCallDelta struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function *functionDelta `json:"function,omitempty"`
}

// functionDelta carries a name once and arguments in fragments.
type functionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// usage is the provider's token accounting, in the provider's own spelling.
//
// VERIFIED: prompt_tokens, completion_tokens and total_tokens are required, and
// there are optional prompt_tokens_details, completion_tokens_details, cost and
// cost_details siblings this build does not read. Also VERIFIED, and worth
// knowing before the client is written: usage is always included on
// OpenRouter — stream_options.include_usage is documented as deprecated and
// having no effect — so there is no request flag to remember.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// SSE framing constants. All VERIFIED.
//
// The framing is plain text/event-stream: `data: ` lines carrying one JSON
// object each, a blank line separating events, and a final `data: [DONE]`.
//
// OpenRouter additionally sends SSE *comment* lines — a line beginning with a
// colon — as keep-alives while a request waits on an upstream provider, and the
// docs say in as many words to skip lines starting with ":" before calling a
// JSON parser. A client that does not is one JSON error away from dropping a
// stream, which is exactly the framing detail a hand-written fixture would
// omit and a live run would then discover. So a fixture carries one.
const (
	sseDataPrefix    = "data: "
	sseCommentPrefix = ":"
	sseDone          = "[DONE]"
)

// KeepAliveComment is the comment line OpenRouter sends while it waits on an
// upstream: a colon, a space, and the text in capitals. VERIFIED verbatim from
// the streaming documentation.
//
// It is exported because it is part of what this card delivers — the framing a
// client has to skip — and a client written against these fixtures should
// recognise it by name rather than by rediscovering the string. Note that
// skipping *any* line starting with ":" is the correct rule; this constant is
// the one instance of it that OpenRouter is known to send.
//
// What is NOT verified is how often it arrives, or whether the blank line SSE
// requires after an event follows it. Every hand-authored fixture writes one
// keep-alive followed by a blank line, which is what the specification implies;
// a recording may show otherwise, and a recording is not required to carry one
// at all — a provider that answers immediately never sends it.
const KeepAliveComment = ": OPENROUTER PROCESSING"

// assembleStream folds a fixture's SSE lines back into the completion they
// describe.
//
// This is the check that makes carrying both shapes safe. A fixture whose
// stream and body disagree is worse than one carrying only a body: the
// transport-level replay and the interface-level replay would then serve two
// different sessions under one name, and whichever one a test used would look
// correct.
func assembleStream(lines []string) (completion, error) {
	var (
		out      completion
		content  strings.Builder
		byIndex  = map[int]*toolCall{}
		order    []int
		sawDone  bool
		sawChunk bool
	)

	for i, line := range lines {
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, sseCommentPrefix):
			// A keep-alive comment. Carries no payload by definition.
			continue
		case !strings.HasPrefix(line, sseDataPrefix):
			return completion{}, fmt.Errorf(
				"stream line %d is neither a `data: ` line, an SSE comment nor blank: %q", i+1, line)
		}

		payload := strings.TrimPrefix(line, sseDataPrefix)
		if payload == sseDone {
			sawDone = true
			continue
		}
		if sawDone {
			return completion{}, fmt.Errorf("stream line %d follows the [DONE] sentinel: %q", i+1, line)
		}

		var chunk completion
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return completion{}, fmt.Errorf("stream line %d: decoding chunk: %w", i+1, err)
		}
		sawChunk = true

		if err := foldChunk(&out, chunk, &content, byIndex, &order); err != nil {
			return completion{}, fmt.Errorf("stream line %d: %w", i+1, err)
		}
	}

	if !sawChunk {
		return completion{}, fmt.Errorf("stream carried no chunks")
	}
	if !sawDone {
		return completion{}, fmt.Errorf("stream never sent the %s%s sentinel", sseDataPrefix, sseDone)
	}

	text := content.String()
	msg := &message{Role: "assistant", Content: &text}
	for _, idx := range order {
		msg.ToolCalls = append(msg.ToolCalls, *byIndex[idx])
	}
	if len(out.Choices) == 0 {
		out.Choices = []choice{{}}
	}
	out.Choices[0].Message = msg
	out.Choices[0].Delta = nil
	out.Object = "chat.completion"
	return out, nil
}

// foldChunk merges one streamed chunk into the completion being assembled.
func foldChunk(out *completion, chunk completion, content *strings.Builder, byIndex map[int]*toolCall, order *[]int) error {
	if chunk.Object != "" && chunk.Object != "chat.completion.chunk" {
		return fmt.Errorf("chunk object is %q, want %q", chunk.Object, "chat.completion.chunk")
	}
	if out.ID == "" {
		out.ID, out.Created, out.Model, out.Provider = chunk.ID, chunk.Created, chunk.Model, chunk.Provider
	}
	if chunk.Usage != nil {
		out.Usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		// A usage-only trailing chunk, carrying an empty choices array.
		//
		// Whether OpenRouter puts usage here or on the same chunk that carries
		// finish_reason is NOT VERIFIED: the docs say only "final chunk
		// includes usage stats" and show no raw bytes. Both spellings are
		// accepted here, and the shipped fixtures deliberately use both, so a
		// client written against them cannot quietly depend on one.
		return nil
	}
	if len(chunk.Choices) != 1 {
		return fmt.Errorf("chunk carries %d choices, want 1", len(chunk.Choices))
	}

	c := chunk.Choices[0]
	if len(out.Choices) == 0 {
		out.Choices = []choice{{Index: c.Index}}
	}
	if c.FinishReason != nil {
		out.Choices[0].FinishReason = c.FinishReason
	}
	if c.NativeFinishReason != nil {
		out.Choices[0].NativeFinishReason = c.NativeFinishReason
	}
	if c.Delta == nil {
		return nil
	}
	if c.Delta.Content != nil {
		content.WriteString(*c.Delta.Content)
	}
	for _, tc := range c.Delta.ToolCalls {
		acc, seen := byIndex[tc.Index]
		if !seen {
			acc = &toolCall{}
			byIndex[tc.Index] = acc
			*order = append(*order, tc.Index)
		}
		if tc.ID != "" {
			acc.ID = tc.ID
		}
		if tc.Type != "" {
			acc.Type = tc.Type
		}
		if tc.Function != nil {
			if tc.Function.Name != "" {
				acc.Function.Name += tc.Function.Name
			}
			acc.Function.Arguments += tc.Function.Arguments
		}
	}
	return nil
}

// decodeBody reads a fixture's assembled body.
func decodeBody(raw json.RawMessage) (completion, error) {
	var c completion
	if err := json.Unmarshal(raw, &c); err != nil {
		return completion{}, fmt.Errorf("decoding response body: %w", err)
	}
	return c, nil
}

// text returns a message's content with null and "" treated alike.
func (m *message) text() string {
	if m == nil || m.Content == nil {
		return ""
	}
	return *m.Content
}

// finishReason returns the choice's normalised stop reason, or "" when it is
// still null.
func (c choice) finishReason() string {
	if c.FinishReason == nil {
		return ""
	}
	return *c.FinishReason
}
