package parse

import (
	"encoding/json"
	"strings"
)

// maxBareScans bounds how many candidate offsets the bare-JSON hint will try
// before giving up. The hint is a courtesy to the repair loop, not a parser,
// and it must not turn a long prose reply into a quadratic scan.
const maxBareScans = 32

// noCallError explains why nothing was extracted.
//
// All of these wrap [ErrNoToolCall] — nothing was extracted, so nothing can be
// dispatched — but the [Kind] separates "the model replied in prose", which is
// ordinary, from "the model wrote a tool call and got the envelope wrong",
// which the repair loop can answer with one specific sentence. That distinction
// is the extractor's whole contribution to KAN-779, and it is why a bare
// "unparseable" string is not good enough here.
func noCallError(msg Message) *Error {
	text := msg.Content

	fences := scanFences(text)
	for _, f := range fences {
		if f.isTool() {
			// Handled by the fenced route; reaching here means the fence held
			// no call at all, which that route already reported.
			continue
		}
		if !looksLikeCall(f.content) {
			continue
		}
		return &Error{
			Kind:    KindUnlabelledFence,
			Detail:  "a fenced block holds a tool call but is labelled " + fenceLabelFor(f) + "; expected ```tool",
			Snippet: snippet(f.raw),
			err:     ErrNoToolCall,
		}
	}

	for _, f := range fences {
		if !f.closed {
			return &Error{
				Kind:    KindNoCall,
				Detail:  "no tool call was found, and a ```" + f.label + " fence was left unclosed",
				Snippet: snippet(f.raw),
				err:     ErrNoToolCall,
			}
		}
	}

	if bare, ok := bareCall(text); ok {
		return &Error{
			Kind:    KindUnfencedCall,
			Detail:  "the reply holds a tool call in plain prose, outside any ```tool fence or " + xmlOpenTag + " block",
			Snippet: snippet(bare),
			err:     ErrNoToolCall,
		}
	}

	return &Error{
		Kind:    KindNoCall,
		Detail:  "the reply carried no native tool call and no recognised block",
		Snippet: snippet(text),
		err:     ErrNoToolCall,
	}
}

func fenceLabelFor(f fence) string {
	if f.label == "" {
		return "with no info string"
	}
	return "```" + f.label
}

// bareCall finds tool-call-shaped JSON sitting loose in prose.
func bareCall(text string) (string, bool) {
	scans := 0
	for offset := 0; offset < len(text); {
		i := strings.IndexByte(text[offset:], '{')
		if i < 0 {
			return "", false
		}
		offset += i

		if scans++; scans > maxBareScans {
			return "", false
		}

		dec := json.NewDecoder(strings.NewReader(text[offset:]))
		var v json.RawMessage
		if err := dec.Decode(&v); err == nil && looksLikeCall(string(v)) {
			return string(v), true
		}
		offset++
	}
	return "", false
}

// looksLikeCall reports whether s decodes to an object that names a tool. It
// never produces a call — it only decides whether a failure is worth a specific
// message.
func looksLikeCall(s string) bool {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var obj json.RawMessage
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&obj); err != nil {
		return false
	}
	fields, err := collectFields(RouteUnknown, obj, 0)
	if err != nil {
		return false
	}
	return len(fields.names) > 0
}
