// Package provider is the model provider's wire format and the types the loop
// exchanges with it.
//
// It holds no client. The real OpenRouter client is KAN-776 and the replay
// provider is [github.com/leejianrong/kopicode/internal/provider/mock]; both
// speak the shapes declared here, and both satisfy the one-method interface the
// engine declares over them. That interface lives in internal/engine and not
// here, because it is declared where it is consumed — the same rule that puts
// parse.Tools in internal/parse rather than in internal/tools.
//
// # What the loop gets back
//
// A reply arrives as a [Stream]: deltas as they land, then an assembled
// [Reply]. Streaming is not an optimisation here, it is the contract — the REPL
// prints tokens as they arrive and Ctrl-C has to cancel mid-reply
// (docs/SLICE-1.md §Test Plan), so a provider that only returned a finished
// answer would leave that whole class of behaviour undriven by the primary test
// seam.
//
// # Determinism
//
// A replayed session must produce a byte-identical journal, given an injected
// clock, RNG and session id. Two properties of this package are what let a
// replay hold up its end, and both are load-bearing rather than incidental:
//
//   - Nothing here starts a goroutine. A [Stream] is pulled synchronously by
//     its consumer, so the order of deltas is a function of the bytes and not
//     of the scheduler.
//   - Nothing here iterates a map to produce output. Tool calls assembled from
//     a stream come back in first-seen index order, kept in a slice alongside
//     the map that indexes them.
//
// # No credentials
//
// [Request] carries a model, a pin, sampling parameters and messages. It has no
// header map, no URL and no credential-shaped field, so a client cannot journal
// one by handing this type to the engine. The API key belongs to the client's
// transport and travels no further (docs/SLICE-1.md §Build Plan step 3).
package provider

import (
	"encoding/json"
	"fmt"

	"github.com/leejianrong/kopicode/internal/parse"
)

// Pin is the provider routing demanded on a request.
//
// Every benchmark request sets provider.order to a single slug, disallows
// fallbacks and fixes the quantization; the resolved pin is journaled on
// ProviderRequest and a result served outside it is discarded rather than
// adjusted (docs/adr/0005-benchmark-and-ab-methodology.md §2).
//
// It mirrors journal.ProviderPin and fixture.Pin field for field and is its own
// declaration on purpose: this package must not import the journal (the engine
// journals; packages return data), and the fixture package is data that must
// not import a client. The three are held to each other by a test rather than
// by an import — see pin_test.go.
type Pin struct {
	// Order is provider.order — a single slug for a benchmark run.
	Order []string `json:"order"`
	// AllowFallbacks is false on every benchmark request.
	AllowFallbacks bool `json:"allow_fallbacks"`
	// Quantizations is the fixed quantization set requested.
	Quantizations []string `json:"quantizations"`
}

// Equal reports whether two pins demand the same routing.
//
// Order and Quantizations are compared as sequences rather than as sets: a pin
// is what was sent on the wire, and two requests that sent their slugs in a
// different order sent different requests.
func (p Pin) Equal(other Pin) bool {
	if p.AllowFallbacks != other.AllowFallbacks {
		return false
	}
	return equalStrings(p.Order, other.Order) && equalStrings(p.Quantizations, other.Quantizations)
}

// String renders the pin for an error message, in one line.
func (p Pin) String() string {
	return fmt.Sprintf("order=%v allow_fallbacks=%t quantizations=%v", p.Order, p.AllowFallbacks, p.Quantizations)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Sampling is the decoding configuration for one request. Recorded per request
// because a harness arm that changes it is a different experiment
// (journal.Sampling is the wire form of the same thing).
type Sampling struct {
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p"`
	MaxTokens   int     `json:"max_tokens"`
	// Seed is nil when none was sent.
	Seed *int64 `json:"seed,omitempty"`
}

// Role names who wrote a message.
type Role string

// The four roles the chat-completions format defines.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry of the conversation, as the provider sees it.
//
// Context assembly — what goes in, in what order, and whether history is
// compacted — is the engine's (KAN-789's), not this package's. This type is
// only the shape the assembled history travels in.
type Message struct {
	Role Role
	// Content is the message text. Empty on an assistant message that was
	// nothing but tool calls.
	Content string
	// ToolCalls are the calls an assistant message made. Empty otherwise.
	ToolCalls []ToolCall
	// ToolCallID echoes the call a tool result answers. Set on RoleTool only.
	ToolCallID string
}

// ToolCall is one call in the provider's own terms.
//
// Arguments stays as raw wire bytes rather than being decoded here: the OpenAI
// format specifies a JSON *string* holding JSON, several providers send a bare
// object instead, and which one arrived is a finding rather than a detail
// (parse.ArgEncoding records it). Normalising at this layer would erase it.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Request is one call to the model.
//
// It deliberately carries no tool catalogue yet. Rendering the harness's tools
// into the wire's tool-definition objects is KAN-776's, and parse.Schema —
// the only tool description this repo has — has no room for the per-tool and
// per-argument descriptions that wire format wants, so choosing that shape is a
// decision rather than a transcription. The replay provider needs none of it.
type Request struct {
	// ModelID is the provider's model identifier for this arm.
	ModelID string
	// Pin is the routing this request demands.
	Pin Pin
	// Sampling is the decoding configuration.
	Sampling Sampling
	// Messages is the assembled conversation, oldest first.
	Messages []Message

	// Turn is the 1-based loop turn this request belongs to, matching the
	// journal envelope's turn. Attempt is 1 for the first send of a turn and
	// increments per repair or retry, matching journal.ProviderRequest.Attempt.
	//
	// Neither is sent on the wire. They are here because the loop already knows
	// both at the moment it calls — it has to, in order to journal them — and
	// because a replay that is handed the position it is being asked for can
	// *check* that it is serving the reply the loop expects instead of assuming
	// it. A live client ignores them. Zero means "not stated", and the replay
	// then falls back to strict recorded order.
	Turn    int
	Attempt int
}

// Usage is the token accounting the provider reported, in the journal's
// spelling rather than the wire's (the wire says prompt_tokens; the journal
// says prompt).
type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

// Reply is one assembled model reply.
//
// It carries everything journal.ProviderResponse records — the raw body, the
// usage, the finish reason and who served it — plus the destructured content
// the loop acts on. The engine writes the event; this package never touches the
// journal.
type Reply struct {
	// ID is the provider's generation id.
	ID string
	// ModelID is the model that served the request, as the response reported
	// it. It can differ from the requested id, which is a finding.
	ModelID string
	// ServedBy is the upstream provider that answered, from the body's
	// top-level `provider` field. A reply served outside the declared pin is
	// discarded, not adjusted (ADR-0005 §2). Empty when the response omitted
	// it — the field is not universally present, so nothing may require it.
	ServedBy string
	// FinishReason is OpenRouter's normalised stop reason, verbatim.
	FinishReason string
	// Content is the assistant's text.
	Content string
	// Reasoning is reasoning-token text, which the journal keeps as a separate
	// ThinkingBlock rather than folding into the answer.
	Reasoning string
	// ToolCalls are the native tool calls, in wire order.
	ToolCalls []ToolCall
	// Usage is the token accounting.
	Usage Usage
	// Raw is the assembled response body, verbatim, for
	// journal.ProviderResponse.Body. It is the bytes the provider sent and not
	// a re-encoding of the fields above: re-marshalling would be deterministic
	// but would no longer be what arrived, and the journal records what
	// arrived.
	Raw json.RawMessage
}

// Message maps the reply onto the shape the extractor consumes.
//
// This is the real mapping. parse.Message is provider-agnostic by design — "the
// provider maps onto it" is that package's stated contract — so the mapping
// belongs here, on the side that owns the wire format.
//
// Note what it does not do: Arguments is passed through as the bytes that
// arrived. A mapping that unquoted the wire's JSON string and re-encoded it
// would hand the extractor a value that no longer says which encoding the model
// used, and parse.ArgEncoding exists precisely to record that — it reaches the
// session record as journal.ToolCallParsed's arg_encoding, so normalising here
// would not lose a debug detail, it would silently zero a measured field.
func (r Reply) Message() parse.Message {
	msg := parse.Message{Content: r.Content}
	for _, tc := range r.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, parse.NativeCall{
			ID:        tc.ID,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		})
	}
	return msg
}

// APIError is a provider response that was not a success.
//
// It carries the status and the body and nothing else — no headers, because
// that is where the credential travels on the request side and there is no
// version of this type that should be able to hold one.
type APIError struct {
	// Status is the HTTP status code.
	Status int
	// Body is the response body, whole. It is never truncated: the body of a
	// failed provider call is the diagnostic, and clipping it is the specific
	// failure this project designs out.
	Body string
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("provider: http %d", e.Status)
	}
	return fmt.Sprintf("provider: http %d: %s", e.Status, e.Body)
}
