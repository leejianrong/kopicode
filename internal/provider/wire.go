package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// This file decodes as much of OpenRouter's chat-completions format as the loop
// needs to act on a reply. It is deliberately partial and deliberately lenient
// on the way in: an OpenRouter response carries fields this build does not read
// (logprobs, refusal, reasoning_details, cost accounting), and a decoder that
// rejected them would fail the day the provider adds one. [Reply].Raw keeps the
// full bytes, so nothing is lost by not modelling a field.
//
// # Why this is not fixture's wire.go
//
// internal/provider/fixture has a similar file. It is unexported, it exists to
// check a fixture against itself, and it models `arguments` as a Go string
// because that is what its assemble-and-compare check needs. This one is the
// path the loop actually runs on, and it differs where that matters:
//
//   - Arguments is json.RawMessage, so the bytes that arrived reach the
//     extractor unaltered and parse.ArgEncoding can record whether the model
//     sent the wire-specified JSON string or a bare object. Decoding into a
//     string and re-encoding would erase that difference, and it is a finding:
//     it lands in the journal on ToolCallParsed as arg_encoding.
//   - Streamed argument fragments are accumulated in first-seen index order,
//     never by array position, because a provider is free to interleave them.
//
// The two files are held to each other by the fixtures themselves: every
// shipped fixture carries both a stream and an assembled body, and fixture's
// Validate refuses one whose stream does not fold back into the other. So a
// stream this file replays is, by construction, the body that file checked.
// KAN-776 should reach for this one rather than adding a third copy.

// completion is a chat-completion object, assembled or chunked.
type completion struct {
	ID       string   `json:"id"`
	Object   string   `json:"object"`
	Created  int64    `json:"created"`
	Model    string   `json:"model"`
	Provider string   `json:"provider,omitempty"`
	Choices  []choice `json:"choices"`
	Usage    *usage   `json:"usage,omitempty"`
}

// choice is one completion. kopicode sends n=1 and reads the first.
type choice struct {
	Index              int      `json:"index"`
	Message            *message `json:"message,omitempty"`
	Delta              *delta   `json:"delta,omitempty"`
	FinishReason       *string  `json:"finish_reason"`
	NativeFinishReason *string  `json:"native_finish_reason,omitempty"`
}

// message is an assembled assistant reply. Content is typed `string | null` on
// the wire and is null — not "" — on a reply that is only tool calls.
type message struct {
	Role      string     `json:"role"`
	Content   *string    `json:"content"`
	Reasoning *string    `json:"reasoning,omitempty"`
	ToolCalls []wireCall `json:"tool_calls,omitempty"`
}

// delta is one streamed increment of a message.
type delta struct {
	Role      string      `json:"role,omitempty"`
	Content   *string     `json:"content,omitempty"`
	Reasoning *string     `json:"reasoning,omitempty"`
	ToolCalls []callDelta `json:"tool_calls,omitempty"`
}

// wireCall is one entry of an assembled tool_calls array.
type wireCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

// wireFunction is a tool call's name and arguments.
type wireFunction struct {
	Name string `json:"name"`
	// Arguments is the raw value, kept as it arrived. The wire format specifies
	// a JSON string holding JSON; some providers send a bare object.
	Arguments json.RawMessage `json:"arguments"`
}

// callDelta is one streamed increment of a tool call. Index is what identifies
// which call a fragment belongs to; array position is not, and treating it as
// such is the bug OpenRouter's own snippet contains.
type callDelta struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function *functionDelta `json:"function,omitempty"`
}

// functionDelta carries a name once and arguments in fragments.
type functionDelta struct {
	Name string `json:"name,omitempty"`
	// Arguments is a fragment of the arguments value. On the documented path it
	// is a JSON string carrying part of the argument text; a provider that
	// emits a whole call in one delta may send the object instead, so it is
	// kept raw and classified in accumulator.add.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// usage is the provider's token accounting, in the provider's own spelling.
// OpenRouter always includes it — stream_options.include_usage is documented as
// having no effect — so a stream that carried none is a stream that ended early.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// text returns a message's content with null and "" treated alike.
func (m *message) text() string {
	if m == nil || m.Content == nil {
		return ""
	}
	return *m.Content
}

func (m *message) reasoning() string {
	if m == nil || m.Reasoning == nil {
		return ""
	}
	return *m.Reasoning
}

// finishReason returns the choice's normalised stop reason, or "" while it is
// still null.
func (c choice) finishReason() string {
	if c.FinishReason == nil {
		return ""
	}
	return *c.FinishReason
}

// accumulator folds an assembled body, or a sequence of streamed chunks, into
// one reply.
//
// It is the single place a reply is built, so the streaming and non-streaming
// paths cannot come to disagree about what a reply is.
type accumulator struct {
	id           string
	model        string
	servedBy     string
	finishReason string
	content      bytes.Buffer
	reasoning    bytes.Buffer

	// calls indexes accumulating tool calls by the wire's index field; order
	// keeps them in first-seen order. Output is built from order and never from
	// ranging over the map — Go randomises that, and a journal whose tool calls
	// swap places between two replays of the same fixture is not byte-identical.
	calls map[int]*callAccumulator
	order []int

	usage *usage
}

// callAccumulator is one tool call being assembled from fragments.
type callAccumulator struct {
	id   string
	kind string
	name string
	// args holds the raw fragments concatenated. fromString records that they
	// arrived as JSON string fragments, in which case args holds the *decoded*
	// text and is re-quoted on the way out.
	args       bytes.Buffer
	fromString bool
	sawArgs    bool
}

// addBody folds a complete, non-streamed body.
func (a *accumulator) addBody(body completion) error {
	if len(body.Choices) != 1 {
		return fmt.Errorf("response carries %d choices, want 1", len(body.Choices))
	}
	c := body.Choices[0]
	if c.Message == nil {
		return fmt.Errorf("response choice carries no message")
	}

	a.id, a.model, a.servedBy = body.ID, body.Model, body.Provider
	a.finishReason = c.finishReason()
	a.content.WriteString(c.Message.text())
	a.reasoning.WriteString(c.Message.reasoning())
	a.usage = body.Usage

	for i, tc := range c.Message.ToolCalls {
		acc := a.call(i)
		acc.id, acc.kind, acc.name = tc.ID, tc.Type, tc.Function.Name
		if len(bytes.TrimSpace(tc.Function.Arguments)) > 0 {
			acc.sawArgs = true
			acc.args.Write(tc.Function.Arguments)
		}
	}
	return nil
}

// addChunk folds one streamed chunk and returns the user-visible increments it
// carried, in the order they must be emitted.
//
// Tool-call fragments are not among them: a partial call is not something the
// loop can act on or the REPL can usefully print, so calls are delivered
// assembled on the reply instead. Folding and emitting are one operation
// because splitting them is how a chunk gets folded twice, or printed without
// being recorded.
func (a *accumulator) addChunk(chunk completion) ([]Delta, error) {
	if chunk.Object != "" && chunk.Object != "chat.completion.chunk" {
		return nil, fmt.Errorf("chunk object is %q, want %q", chunk.Object, "chat.completion.chunk")
	}
	if a.id == "" {
		a.id, a.model, a.servedBy = chunk.ID, chunk.Model, chunk.Provider
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}
	if len(chunk.Choices) == 0 {
		// A usage-only trailing chunk. Whether usage rides here or on the chunk
		// that carries finish_reason is not documented, so both are accepted.
		return nil, nil
	}
	if len(chunk.Choices) != 1 {
		return nil, fmt.Errorf("chunk carries %d choices, want 1", len(chunk.Choices))
	}

	c := chunk.Choices[0]
	if r := c.finishReason(); r != "" {
		a.finishReason = r
	}
	if c.Delta == nil {
		return nil, nil
	}
	for _, tc := range c.Delta.ToolCalls {
		if err := a.addCallDelta(tc); err != nil {
			return nil, err
		}
	}

	var out []Delta
	if d := c.Delta.Reasoning; d != nil && *d != "" {
		a.reasoning.WriteString(*d)
		out = append(out, Delta{Kind: DeltaReasoning, Text: *d})
	}
	if d := c.Delta.Content; d != nil && *d != "" {
		a.content.WriteString(*d)
		out = append(out, Delta{Kind: DeltaContent, Text: *d})
	}
	return out, nil
}

// addCallDelta merges one tool-call fragment by its index.
func (a *accumulator) addCallDelta(tc callDelta) error {
	acc := a.call(tc.Index)
	if tc.ID != "" {
		acc.id = tc.ID
	}
	if tc.Type != "" {
		acc.kind = tc.Type
	}
	if tc.Function == nil {
		return nil
	}
	acc.name += tc.Function.Name

	frag := bytes.TrimSpace(tc.Function.Arguments)
	if len(frag) == 0 {
		return nil
	}
	if frag[0] == '"' {
		// The documented shape: a JSON string carrying part of the argument
		// text. Decode it so fragments concatenate as text rather than as
		// quoted pieces, and re-quote once at the end.
		var part string
		if err := json.Unmarshal(frag, &part); err != nil {
			return fmt.Errorf("tool call %d: decoding arguments fragment: %w", tc.Index, err)
		}
		if acc.sawArgs && !acc.fromString {
			return fmt.Errorf("tool call %d: arguments arrived as both a JSON string and a bare value", tc.Index)
		}
		acc.fromString, acc.sawArgs = true, true
		acc.args.WriteString(part)
		return nil
	}

	// A provider that emitted the whole arguments object in one delta.
	if acc.sawArgs && acc.fromString {
		return fmt.Errorf("tool call %d: arguments arrived as both a JSON string and a bare value", tc.Index)
	}
	acc.sawArgs = true
	acc.args.Write(frag)
	return nil
}

// call returns the accumulator for one tool-call index, creating it in
// first-seen order.
func (a *accumulator) call(index int) *callAccumulator {
	if a.calls == nil {
		a.calls = map[int]*callAccumulator{}
	}
	if acc, ok := a.calls[index]; ok {
		return acc
	}
	acc := &callAccumulator{}
	a.calls[index] = acc
	a.order = append(a.order, index)
	return acc
}

// reply builds the assembled reply. raw is the response body verbatim, which is
// what the journal records.
func (a *accumulator) reply(raw json.RawMessage) Reply {
	r := Reply{
		ID:           a.id,
		ModelID:      a.model,
		ServedBy:     a.servedBy,
		FinishReason: a.finishReason,
		Content:      a.content.String(),
		Reasoning:    a.reasoning.String(),
		Raw:          raw,
	}
	if a.usage != nil {
		r.Usage = Usage{
			Prompt:     a.usage.PromptTokens,
			Completion: a.usage.CompletionTokens,
			Total:      a.usage.TotalTokens,
		}
	}
	for _, idx := range a.order {
		c := a.calls[idx]
		r.ToolCalls = append(r.ToolCalls, ToolCall{
			ID:        c.id,
			Name:      c.name,
			Arguments: c.arguments(),
		})
	}
	return r
}

// arguments returns the call's arguments as raw JSON, re-quoting the
// string-fragment case exactly once.
func (c *callAccumulator) arguments() json.RawMessage {
	if !c.sawArgs {
		return nil
	}
	if !c.fromString {
		return append(json.RawMessage(nil), c.args.Bytes()...)
	}
	return jsonString(c.args.String())
}

// jsonString encodes s as a JSON string without HTML escaping.
//
// encoding/json's default escaping would rewrite a `<` inside an argument as
// a unicode escape, which decodes back to the same value but is not the same
// bytes — and these bytes reach the journal through parse.ToolCall.Arguments.
// It is the same trap journal.Marshal exists to avoid on the way out.
func jsonString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// Unreachable: encoding a Go string as JSON cannot fail. Falling back
		// to the escaping encoder keeps the value correct if it somehow does.
		out, _ := json.Marshal(s)
		return out
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
