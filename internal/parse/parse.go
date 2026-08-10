// Package parse extracts tool calls from a model's reply.
//
// Weak-model tool calling is the riskiest mechanism in the product: the whole
// thesis is that harness quality moves a cheap model's measured coding ability,
// and a model that cannot get a call through the front door never reaches the
// parts of the harness that are supposed to help it. So extraction tries three
// routes in order and the first success wins (docs/SLICE-1.md §3):
//
//  1. [RouteNative]     — OpenAI-style tool_calls carried on the response.
//  2. [RouteFencedJSON] — a ```tool fenced JSON block in the reply text.
//  3. [RouteXMLTag]     — a <tool_call>…</tool_call> block in the reply text.
//
// The route taken comes back as part of the result, because *which* route a
// model reaches for is a finding: a model that never uses its native channel is
// telling us where the harness has to meet it. The engine journals the route;
// this package does not know the journal exists, and must not — the interface
// is declared where it is consumed.
//
// # Failing loudly
//
// The project's core metric depends on telling a harness failure from a model
// failure, so a half-successful parse is worse than no parse at all. Two rules
// follow, and they are the reason to prefer this package over a regexp:
//
//   - Once a route's marker is found, extraction commits to that route. A
//     ```tool fence that is never closed, or whose JSON does not parse, is a
//     typed failure — never a reason to quietly try the next route and report
//     success on something else the model happened to write.
//   - Anything the message says two ways at once is [KindAmbiguousCall], not a
//     guess. Picking the more plausible of two disagreeing tool names is
//     exactly the silent misapplication the design exists to prevent.
//
// Every failure is a *[Error] carrying a [Kind], which is what the repair loop
// (KAN-779) turns into a specific message back to the model. Nothing here
// repairs, and nothing here panics: the input is untrusted model output by
// definition.
//
// # Leniency, and its limit
//
// The extractor does normalise a few things, because they are unambiguous
// rather than because they are convenient: key aliases within one call object
// (tool/name, arguments/args/parameters), an OpenAI-shaped {"function": {…}}
// nesting written into text, absent or null arguments meaning "none", and
// arguments arriving as a JSON *string* that itself decodes to an object —
// which is what the OpenAI wire format specifies natively and what weak models
// copy into text. Each of those has exactly one reading. Where two readings
// exist, extraction fails.
package parse

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Message is the assistant reply extraction reads.
//
// It is provider-agnostic on purpose: this package is the consumer of the
// provider's output, so the shape lives here and the provider maps onto it,
// rather than this package importing a wire format it does not own.
type Message struct {
	// Content is the assistant's text, which the fenced and XML routes scan.
	Content string

	// ToolCalls is the native tool_calls array, already destructured from the
	// response envelope. Empty when the model answered in text.
	ToolCalls []NativeCall
}

// NativeCall is one entry of an OpenAI-style tool_calls array.
type NativeCall struct {
	// ID is the provider-assigned call id, echoed back on the tool result.
	ID string

	// Name is the function name.
	Name string

	// Arguments is the raw arguments value as it arrived. The OpenAI wire
	// format specifies a JSON *string* holding JSON, several providers send a
	// bare object instead, and the difference is a finding rather than a
	// detail — so it stays raw until extraction classifies it.
	Arguments json.RawMessage
}

// ArgEncoding records how a call's arguments arrived on the wire.
//
// Both encodings are accepted and both normalise to the same object, but which
// one a model produced is exactly the kind of per-model harness fact this
// project exists to measure, so it is not thrown away.
type ArgEncoding uint8

const (
	// ArgsObject means arguments arrived as a JSON object, or were absent.
	ArgsObject ArgEncoding = iota

	// ArgsJSONString means arguments arrived double-encoded: a JSON string
	// whose contents are themselves a JSON object.
	ArgsJSONString
)

var argEncodingText = map[ArgEncoding]string{
	ArgsObject:     "object",
	ArgsJSONString: "json_string",
}

// String returns the wire form of the encoding.
func (a ArgEncoding) String() string {
	if s, ok := argEncodingText[a]; ok {
		return s
	}
	return fmt.Sprintf("arg_encoding(%d)", uint8(a))
}

// MarshalText encodes the argument encoding as its wire form, for the journal.
func (a ArgEncoding) MarshalText() ([]byte, error) {
	s, ok := argEncodingText[a]
	if !ok {
		return nil, &Error{Kind: KindUnspecified, Detail: "cannot marshal unknown argument encoding"}
	}
	return []byte(s), nil
}

// ToolCall is one extracted call, normalised.
type ToolCall struct {
	// ID is the provider-assigned call id. Empty on the text routes, which
	// have no id to carry — the engine assigns one there.
	ID string

	// Name is the tool name, non-empty for any call extraction produced.
	Name string

	// Arguments is always a compact JSON object, never a string and never
	// null. Compact so that a replayed session produces a byte-identical
	// journal.
	Arguments json.RawMessage

	// ArgEncoding records how Arguments arrived before normalisation.
	ArgEncoding ArgEncoding

	// Raw is the exact text this call was extracted from, for the journal's
	// ToolCallRequested event. Keeping it means a parse argued about later can
	// be re-examined against what the model actually wrote.
	Raw string
}

// Extraction is a successful extraction: one or more calls, and the route that
// produced them.
//
// Its fields are unexported and there is no exported constructor, so the only
// Extraction that exists outside this package is one [Extract] returned — and
// [Extract] cannot produce one without a valid route. An extraction that does
// not say which route it came from is not representable, which is the point:
// the route is evidence, and evidence that can be omitted gets omitted.
type Extraction struct {
	route Route
	calls []ToolCall
}

// Route returns the route that produced the calls. It is always valid on an
// Extraction returned by [Extract]; the zero Extraction reports
// [RouteUnknown].
func (e Extraction) Route() Route { return e.route }

// Calls returns the extracted calls, always at least one. The slice is a copy:
// an extraction is a record of what the model asked for, and a caller editing
// it in place would be rewriting the evidence.
func (e Extraction) Calls() []ToolCall { return slices.Clone(e.calls) }

// newExtraction is the only way an Extraction comes into being.
func newExtraction(route Route, calls []ToolCall) (Extraction, *Error) {
	if !route.Valid() {
		return Extraction{}, &Error{
			Kind:   KindUnspecified,
			Detail: "internal: extraction produced without a route",
		}
	}
	if len(calls) == 0 {
		return Extraction{}, &Error{
			Kind:   KindUnspecified,
			Route:  route,
			Detail: "internal: extraction produced without calls",
		}
	}
	return Extraction{route: route, calls: calls}, nil
}

// Extract pulls tool calls out of a model reply, trying the routes in order and
// stopping at the first that produces one.
//
// A route whose marker is absent is skipped silently. A route whose marker is
// present but whose content is broken fails the whole extraction with a typed
// *[Error] rather than falling through — see the package doc on failing loudly.
//
// When no route's marker appears at all, the error wraps [ErrNoToolCall], which
// is the only failure that means "the model just replied in prose".
func Extract(msg Message) (Extraction, error) {
	for _, route := range routeOrder {
		calls, err := extractRoute(route, msg)
		if err != nil {
			return Extraction{}, err
		}
		if len(calls) == 0 {
			continue
		}
		ext, err := newExtraction(route, calls)
		if err != nil {
			return Extraction{}, err
		}
		return ext, nil
	}
	return Extraction{}, noCallError(msg)
}

// extractRoute runs one route. It returns (nil, nil) when the route's marker is
// not present, which is how Extract knows to try the next one.
func extractRoute(route Route, msg Message) ([]ToolCall, *Error) {
	switch route {
	case RouteNative:
		return extractNative(msg.ToolCalls)
	case RouteFencedJSON:
		return extractFenced(msg.Content)
	case RouteXMLTag:
		return extractXMLTag(msg.Content)
	case RouteUnknown:
		fallthrough
	default:
		return nil, &Error{Kind: KindUnspecified, Detail: "internal: no such route"}
	}
}
