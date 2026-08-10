package parse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
)

// Key aliases accepted inside one call object. Each alias is a name a model has
// a real reason to reach for; none of them is ambiguous on its own, and two of
// them disagreeing is a hard failure rather than a coin toss.
var (
	nameKeys = []string{"tool", "name", "tool_name"}
	argKeys  = []string{"arguments", "args", "parameters"}
	idKeys   = []string{"id", "tool_call_id"}

	// nestKeys hold a nested call object: the OpenAI envelope, written into
	// text by a model that learned the shape from documentation.
	nestKeys = []string{"function", "tool_call"}
)

// maxValueDepth bounds how far decodeStream will follow nested arrays before
// giving up. Model output is untrusted, and an unbounded recursion over
// attacker-shaped JSON is a stack overflow with extra steps.
const maxValueDepth = 4

// decodeStream decodes the JSON payload of a text-route block into calls.
//
// The block may hold one object, an array of objects, or a whitespace-separated
// stream of either — a model emitting two calls back to back without wrapping
// them in an array has said something unambiguous, so it is read rather than
// rejected. Anything that is not valid JSON is [KindInvalidJSON]: repairing
// single quotes and trailing commas is KAN-779's job, not this package's.
func decodeStream(route Route, block, raw string) ([]ToolCall, *Error) {
	if strings.TrimSpace(block) == "" {
		return nil, &Error{
			Kind:   KindEmptyBlock,
			Route:  route,
			Detail: "the block held no content",
		}
	}

	dec := json.NewDecoder(strings.NewReader(block))
	var out []ToolCall
	for {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, &Error{
				Kind:    KindInvalidJSON,
				Route:   route,
				Detail:  "the block is not valid JSON",
				Snippet: snippet(block),
				err:     err,
			}
		}
		calls, perr := callsFromValue(route, v, raw, 0)
		if perr != nil {
			return nil, perr
		}
		out = append(out, calls...)
	}

	if len(out) == 0 {
		return nil, &Error{
			Kind:   KindEmptyBlock,
			Route:  route,
			Detail: "the block held no tool call",
		}
	}
	return out, nil
}

// callsFromValue turns one decoded JSON value into calls, following arrays.
func callsFromValue(route Route, v json.RawMessage, raw string, depth int) ([]ToolCall, *Error) {
	if depth > maxValueDepth {
		return nil, &Error{
			Kind:    KindInvalidJSON,
			Route:   route,
			Detail:  "the call is nested too deeply to be a tool call",
			Snippet: snippet(string(v)),
		}
	}

	trimmed := bytes.TrimSpace(v)
	switch {
	case len(trimmed) == 0:
		return nil, nil

	case trimmed[0] == '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, &Error{
				Kind:    KindInvalidJSON,
				Route:   route,
				Detail:  "the call array did not decode",
				Snippet: snippet(string(trimmed)),
				err:     err,
			}
		}
		var out []ToolCall
		for _, item := range items {
			calls, perr := callsFromValue(route, item, raw, depth+1)
			if perr != nil {
				return nil, perr
			}
			out = append(out, calls...)
		}
		return out, nil

	case trimmed[0] == '{':
		call, perr := callFromObject(route, trimmed, raw)
		if perr != nil {
			return nil, perr
		}
		return []ToolCall{call}, nil

	default:
		return nil, &Error{
			Kind:    KindInvalidJSON,
			Route:   route,
			Detail:  "expected a JSON object describing a tool call",
			Snippet: snippet(string(trimmed)),
		}
	}
}

// callFromObject normalises one call object into a [ToolCall].
func callFromObject(route Route, obj json.RawMessage, raw string) (ToolCall, *Error) {
	fields, perr := collectFields(route, obj, 0)
	if perr != nil {
		return ToolCall{}, perr
	}

	name, perr := soleName(route, fields, obj)
	if perr != nil {
		return ToolCall{}, perr
	}

	args, encoding, perr := soleArguments(route, fields, obj)
	if perr != nil {
		return ToolCall{}, perr
	}

	if raw == "" {
		raw = string(obj)
	}
	return ToolCall{
		ID:          fields.id,
		Name:        name,
		Arguments:   args,
		ArgEncoding: encoding,
		Raw:         raw,
	}, nil
}

// callFields are the candidate values found for one call, across the accepted
// key aliases and one level of OpenAI-style nesting.
type callFields struct {
	names []string
	args  []json.RawMessage
	id    string
}

// collectFields gathers candidates from an object and from a nested call
// envelope inside it, without deciding between them. Deciding is soleName and
// soleArguments' job, and disagreement is a failure there.
func collectFields(route Route, obj json.RawMessage, depth int) (callFields, *Error) {
	var out callFields
	if depth > 1 {
		return out, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(obj, &fields); err != nil {
		return out, &Error{
			Kind:    KindInvalidJSON,
			Route:   route,
			Detail:  "the call object did not decode",
			Snippet: snippet(string(obj)),
			err:     err,
		}
	}

	for _, key := range nameKeys {
		v, ok := fields[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return out, &Error{
				Kind:    KindMissingName,
				Route:   route,
				Detail:  "the tool name must be a JSON string, key " + key,
				Snippet: snippet(string(v)),
				err:     err,
			}
		}
		if s = strings.TrimSpace(s); s != "" {
			out.names = append(out.names, s)
		}
	}

	for _, key := range argKeys {
		if v, ok := fields[key]; ok {
			out.args = append(out.args, v)
		}
	}

	for _, key := range idKeys {
		v, ok := fields[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && out.id == "" {
			out.id = s
		}
	}

	for _, key := range nestKeys {
		v, ok := fields[key]
		if !ok {
			continue
		}
		if inner := bytes.TrimSpace(v); len(inner) == 0 || inner[0] != '{' {
			continue
		}
		nested, perr := collectFields(route, v, depth+1)
		if perr != nil {
			return out, perr
		}
		out.names = append(out.names, nested.names...)
		out.args = append(out.args, nested.args...)
		if out.id == "" {
			out.id = nested.id
		}
	}

	return out, nil
}

// soleName returns the one tool name the object names, or fails.
func soleName(route Route, fields callFields, obj json.RawMessage) (string, *Error) {
	distinct := distinctStrings(fields.names)
	switch len(distinct) {
	case 1:
		return distinct[0], nil
	case 0:
		return "", &Error{
			Kind:    KindMissingName,
			Route:   route,
			Detail:  "the call named no tool; expected one of " + strings.Join(nameKeys, ", "),
			Snippet: snippet(string(obj)),
		}
	default:
		return "", &Error{
			Kind:    KindAmbiguousCall,
			Route:   route,
			Detail:  "the call named two different tools: " + strings.Join(distinct, ", "),
			Snippet: snippet(string(obj)),
		}
	}
}

// soleArguments returns the one argument object the call carries, normalised to
// a compact JSON object.
//
// Absent or null arguments mean an empty object: a tool that takes none is
// ordinary. A JSON string that itself decodes to an object is accepted — that
// is what the OpenAI wire format specifies, and a model copying that shape into
// text has said exactly one thing — but the encoding is recorded rather than
// erased.
func soleArguments(route Route, fields callFields, obj json.RawMessage) (json.RawMessage, ArgEncoding, *Error) {
	distinct := distinctRaw(fields.args)
	if len(distinct) > 1 {
		return nil, ArgsObject, &Error{
			Kind:    KindAmbiguousCall,
			Route:   route,
			Detail:  "the call gave two different argument objects; expected one of " + strings.Join(argKeys, ", "),
			Snippet: snippet(string(obj)),
		}
	}
	if len(distinct) == 0 {
		return json.RawMessage("{}"), ArgsObject, nil
	}

	value := bytes.TrimSpace(distinct[0])
	switch {
	case len(value) == 0, bytes.Equal(value, []byte("null")):
		return json.RawMessage("{}"), ArgsObject, nil

	case value[0] == '{':
		compacted, err := compactObject(value)
		if err != nil {
			return nil, ArgsObject, &Error{
				Kind:    KindInvalidArguments,
				Route:   route,
				Detail:  "the arguments object did not decode",
				Snippet: snippet(string(value)),
				err:     err,
			}
		}
		return compacted, ArgsObject, nil

	case value[0] == '"':
		var inner string
		if err := json.Unmarshal(value, &inner); err != nil {
			return nil, ArgsObject, &Error{
				Kind:    KindInvalidArguments,
				Route:   route,
				Detail:  "the arguments string did not decode",
				Snippet: snippet(string(value)),
				err:     err,
			}
		}
		if trimmed := strings.TrimSpace(inner); trimmed == "" {
			return json.RawMessage("{}"), ArgsJSONString, nil
		}
		compacted, err := compactObject([]byte(inner))
		if err != nil {
			return nil, ArgsObject, &Error{
				Kind:    KindInvalidArguments,
				Route:   route,
				Detail:  "the arguments arrived as a string that is not a JSON object",
				Snippet: snippet(inner),
				err:     err,
			}
		}
		return compacted, ArgsJSONString, nil

	default:
		return nil, ArgsObject, &Error{
			Kind:    KindInvalidArguments,
			Route:   route,
			Detail:  "arguments must be a JSON object, or a JSON string encoding one",
			Snippet: snippet(string(value)),
		}
	}
}

// compactObject validates that b is a JSON object and returns it compacted, so
// that a replayed session journals byte-identical arguments.
func compactObject(b []byte) (json.RawMessage, error) {
	if t := bytes.TrimSpace(b); len(t) == 0 || t[0] != '{' {
		return nil, errors.New("not a JSON object")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return nil, err
	}
	return json.RawMessage(buf.Bytes()), nil
}

func distinctStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// distinctRaw dedupes candidate argument values by their compact form, so that
// the same object written twice under two aliases is agreement, not conflict.
func distinctRaw(in []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	var seen []string
	for _, v := range in {
		key := string(bytes.TrimSpace(v))
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err == nil {
			key = buf.String()
		}
		if slices.Contains(seen, key) {
			continue
		}
		seen = append(seen, key)
		out = append(out, v)
	}
	return out
}
