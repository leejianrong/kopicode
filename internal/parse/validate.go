package parse

import (
	"encoding/json"
	"strconv"
	"strings"
)

// validate checks one extracted call against what the harness actually offers.
//
// Extraction proves the model produced a well-formed call; it says nothing
// about whether that call can run. The four classifications here are the other
// half of docs/SLICE-1.md §3, and they are the ones worth the most: a model
// that wrote `read` for `read_file`, or left out `path`, is one specific
// sentence away from a working turn.
//
// A nil Tools skips every check — the harness has not said what it offers, so
// it has no grounds to reject anything.
func validate(tools Tools, route Route, call ToolCall) *Error {
	if tools == nil {
		return nil
	}

	schema, ok := tools.Schema(call.Name)
	if !ok {
		return &Error{
			Kind:   KindUnknownTool,
			Route:  route,
			Tool:   call.Name,
			Detail: "this harness offers no tool named " + quote(call.Name),
		}
	}

	args, err := argumentFields(route, schema.Name, call.Arguments)
	if err != nil {
		return err
	}

	if perr := checkRequired(route, schema, args); perr != nil {
		return perr
	}
	return checkValues(route, schema, args)
}

// argumentFields decodes the call's arguments, dropping JSON nulls.
//
// Extraction guarantees Arguments is an object, so a decode failure here is a
// harness bug rather than a model one — it is reported as [KindInvalidArguments]
// anyway, because a repair attempt costs one round trip and a panic costs the
// session.
func argumentFields(route Route, tool string, raw json.RawMessage) (map[string]json.RawMessage, *Error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, &Error{
			Kind:    KindInvalidArguments,
			Route:   route,
			Tool:    tool,
			Detail:  "the arguments object did not decode",
			Snippet: snippet(string(raw)),
			err:     err,
		}
	}

	// A null is the model saying "nothing here", which is the same thing as
	// omitting the key — and reading it as a present value would report a type
	// failure where the real one is a missing argument.
	for k, v := range fields {
		if jsonKind(v) == "null" {
			delete(fields, k)
		}
	}
	return fields, nil
}

// checkRequired reports every required argument the call left out, in one
// error. Naming them one per round trip would spend the repair budget on
// arithmetic.
func checkRequired(route Route, schema Schema, args map[string]json.RawMessage) *Error {
	var missing []string
	for _, p := range schema.Params {
		if !p.Required {
			continue
		}
		if _, ok := args[p.Name]; !ok {
			missing = append(missing, p.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	noun := "argument"
	if len(missing) > 1 {
		noun = "arguments"
	}
	return &Error{
		Kind:     KindMissingArgument,
		Route:    route,
		Tool:     schema.Name,
		Argument: missing[0],
		Detail: "the call to " + quote(schema.Name) + " left out the required " +
			noun + " " + strings.Join(quoteAll(missing), ", "),
	}
}

// checkValues reports the first argument whose value the tool cannot accept.
//
// First rather than all: a type error and an enum error on two arguments have
// different fixes, and one specific sentence per round trip is the whole
// premise. Schema order makes "first" deterministic.
func checkValues(route Route, schema Schema, args map[string]json.RawMessage) *Error {
	for _, p := range schema.Params {
		v, ok := args[p.Name]
		if !ok {
			continue
		}

		if !p.Type.accepts(v) {
			return &Error{
				Kind:     KindWrongArgumentType,
				Route:    route,
				Tool:     schema.Name,
				Argument: p.Name,
				Detail: "the argument " + quote(p.Name) + " of " + quote(schema.Name) +
					" must be a JSON " + p.Type.String() + ", but " + describe(p.Type, v) + " was sent",
				Snippet: snippet(string(v)),
			}
		}

		if perr := checkEnum(route, schema, p, v); perr != nil {
			return perr
		}
	}
	return nil
}

// checkEnum holds an argument to its closed set. A value outside the set is
// never coerced to the nearest member: picking the more plausible of two
// spellings is the same silent misapplication extraction refuses.
func checkEnum(route Route, schema Schema, p Param, v json.RawMessage) *Error {
	if len(p.Enum) == 0 {
		return nil
	}

	var got string
	if err := json.Unmarshal(v, &got); err != nil {
		// The decoder's own wording ("cannot unmarshal number into Go value of
		// type string") is about Go, not about the model's mistake, so it is
		// not wrapped into a message the model has to act on.
		return &Error{
			Kind:     KindWrongArgumentType,
			Route:    route,
			Tool:     schema.Name,
			Argument: p.Name,
			Detail: "the argument " + quote(p.Name) + " of " + quote(schema.Name) +
				" must be a JSON string drawn from a fixed set, but a " + jsonKind(v) + " was sent",
			Snippet: snippet(string(v)),
		}
	}

	for _, allowed := range p.Enum {
		if got == allowed {
			return nil
		}
	}
	return &Error{
		Kind:     KindUnknownEnumValue,
		Route:    route,
		Tool:     schema.Name,
		Argument: p.Name,
		Detail: "the argument " + quote(p.Name) + " of " + quote(schema.Name) +
			" does not accept the value " + quote(got),
	}
}

// describe names the value's type as the model needs to hear it. JSON has one
// number type, so "a number was sent" where an integer was wanted reads as a
// contradiction; the fractional part is the actual mistake and is what gets
// said.
func describe(want ParamType, v json.RawMessage) string {
	got := jsonKind(v)
	if want == TypeInteger && got == "number" {
		return "a number with a fractional part"
	}
	if got == "" {
		return "nothing"
	}
	return "a " + got
}

// quote renders a name for a message. It goes through strconv so that a tool
// name the model invented — which may hold a quote, a newline or a control
// byte — cannot break the line it is quoted into.
func quote(s string) string { return strconv.Quote(s) }
