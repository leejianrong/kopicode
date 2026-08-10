package parse

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrNoToolCall reports that the message contained no tool call on any route.
//
// This is the benign outcome — a plain prose reply has no tool call in it and
// that is not an error the model needs told about. Every other failure means a
// route's marker *was* found and the content behind it was broken, which is a
// finding and a repair opportunity (KAN-779).
//
// Compare with errors.Is; the concrete failure is a *[Error], reachable with
// errors.As.
var ErrNoToolCall = errors.New("parse: no tool call found")

// Kind classifies an extraction failure.
//
// The classification is the extractor's whole contribution to repair: the
// repair loop turns a Kind into the specific message that goes back to the
// model, and the bench classifier needs to tell a harness failure from a model
// failure. So a failure is never a bare string — an unclassified parse error is
// a number nobody can attribute.
type Kind uint8

const (
	// KindUnspecified is the zero value and names a programming error here, not
	// a model failure.
	KindUnspecified Kind = iota

	// KindNoCall means no route's marker appeared anywhere in the message.
	KindNoCall

	// KindUnlabelledFence means a fenced block holds something tool-call
	// shaped, but the fence carried no ```tool label. The model got the
	// content right and the envelope wrong, which is worth saying back
	// precisely.
	KindUnlabelledFence

	// KindUnfencedCall means tool-call-shaped JSON was written loose in prose,
	// with no fence and no tag around it. Same distinction as
	// KindUnlabelledFence: content right, envelope missing.
	KindUnfencedCall

	// KindUnclosedFence means a ```tool fence was opened and never closed. The
	// content behind it is not parsed: half a tool call applied is worse than
	// none.
	KindUnclosedFence

	// KindUnclosedTag means a <tool_call> tag was opened and never closed.
	KindUnclosedTag

	// KindEmptyBlock means a route's marker was found with nothing behind it.
	KindEmptyBlock

	// KindInvalidJSON means the block was found but did not parse as JSON —
	// single quotes, a trailing comma, an unescaped newline inside a string.
	KindInvalidJSON

	// KindMissingName means the call object carried no usable tool name.
	KindMissingName

	// KindInvalidArguments means arguments were present but were neither a JSON
	// object nor a JSON string encoding one.
	KindInvalidArguments

	// KindAmbiguousCall means the call object said two different things — two
	// name keys disagreeing, two argument keys disagreeing. Guessing which one
	// the model meant is exactly the plausible half-success this package
	// refuses to produce.
	KindAmbiguousCall

	// KindUnknownTool means the call parsed cleanly and named a tool this
	// harness does not offer. The four classifications below are the semantic
	// half of docs/SLICE-1.md §3: they need a [Tools] lookup to reach, and
	// they are the ones a specific message recovers most often, because the
	// model was one token away from a working call.
	KindUnknownTool

	// KindMissingArgument means a required argument of a known tool was absent.
	KindMissingArgument

	// KindWrongArgumentType means an argument was present with a JSON type the
	// tool does not accept — a number where a string belongs, an object where
	// an array belongs.
	KindWrongArgumentType

	// KindUnknownEnumValue means an argument's value sits outside the closed
	// set the tool accepts for it.
	KindUnknownEnumValue
)

var kindText = map[Kind]string{
	KindUnspecified:       "unspecified",
	KindNoCall:            "no_call",
	KindUnlabelledFence:   "unlabelled_fence",
	KindUnfencedCall:      "unfenced_call",
	KindUnclosedFence:     "unclosed_fence",
	KindUnclosedTag:       "unclosed_tag",
	KindEmptyBlock:        "empty_block",
	KindInvalidJSON:       "invalid_json",
	KindMissingName:       "missing_name",
	KindInvalidArguments:  "invalid_arguments",
	KindAmbiguousCall:     "ambiguous_call",
	KindUnknownTool:       "unknown_tool",
	KindMissingArgument:   "missing_argument",
	KindWrongArgumentType: "wrong_argument_type",
	KindUnknownEnumValue:  "unknown_enum_value",
}

// String returns the wire form of the kind.
func (k Kind) String() string {
	if s, ok := kindText[k]; ok {
		return s
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// MarshalText encodes the kind as its wire form, for the journal.
func (k Kind) MarshalText() ([]byte, error) {
	s, ok := kindText[k]
	if !ok {
		return nil, fmt.Errorf("parse: cannot marshal unknown kind %d", uint8(k))
	}
	return []byte(s), nil
}

// UnmarshalText decodes the wire form produced by [Kind.MarshalText].
func (k *Kind) UnmarshalText(b []byte) error {
	for kind, s := range kindText {
		if s == string(b) {
			*k = kind
			return nil
		}
	}
	return fmt.Errorf("parse: unknown kind %q", b)
}

// Error is an extraction failure, classified.
//
// Route names the route that was attempting the extraction when it failed, or
// [RouteUnknown] when no route's marker was ever found.
type Error struct {
	Kind   Kind
	Route  Route
	Detail string

	// Tool names the tool the failure is about, when the failure knows it. The
	// semantic classifications always do; most extraction failures do not,
	// because a block that did not parse never named anything. It is what the
	// journal's ToolCallFailed.Tool carries, and what lets the repair message
	// quote the shape of one tool rather than the whole catalogue.
	Tool string

	// Argument names the offending argument on the argument classifications,
	// and is empty elsewhere.
	Argument string

	// Snippet is a bounded excerpt of the offending text, for the message only.
	// The full text is never lost: the provider response it came from is
	// journaled whole, and the never-truncate rule lives there.
	Snippet string

	err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("parse: ")
	if e.Route.Valid() {
		b.WriteString(e.Route.String())
		b.WriteString(" route: ")
	}
	b.WriteString(e.Kind.String())
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.err != nil && !errors.Is(e.err, ErrNoToolCall) {
		b.WriteString(": ")
		b.WriteString(e.err.Error())
	}
	if e.Snippet != "" {
		b.WriteString(": ")
		fmt.Fprintf(&b, "%q", e.Snippet)
	}
	return b.String()
}

// Unwrap exposes the cause: [ErrNoToolCall] for the benign outcomes, the
// underlying encoding/json error for a malformed block.
func (e *Error) Unwrap() error { return e.err }

// snippetLimit bounds an excerpt in an error message. It is a display bound,
// not a truncation of anything anyone relies on.
const snippetLimit = 200

func snippet(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= snippetLimit {
		return s
	}
	n := 0
	for i := range s {
		if n == snippetLimit {
			return s[:i] + "…"
		}
		n++
	}
	return s
}
