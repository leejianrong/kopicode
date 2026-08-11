package parse

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Tools is the tool lookup the repair loop needs in order to say what a call
// got wrong.
//
// It is declared here rather than in internal/tools because this package is the
// consumer: the repair loop needs a name resolved and a catalogue listed, and
// nothing else. Declaring it at the consumer keeps the dependency arrow pointing
// one way and keeps the seam honest — a five-method interface here would be the
// tool registry wearing a parser's coat.
//
// A nil Tools is legitimate and means the semantic classifications
// ([KindUnknownTool], [KindMissingArgument], [KindWrongArgumentType],
// [KindUnknownEnumValue]) are not reachable. Extraction repair still works.
type Tools interface {
	// Schema returns the schema for the named tool, and whether the harness
	// offers one under exactly that name. Lookup is exact: a near miss is a
	// finding the repair message reports, not something to resolve silently.
	Schema(name string) (Schema, bool)

	// Names returns every tool name the harness offers. It is what the
	// unknown-tool message lists — names only, never the schemas behind them,
	// because re-sending the whole catalogue is the failure mode this card
	// exists to avoid.
	Names() []string
}

// Schema describes one tool in exactly the detail a repair message needs: what
// its arguments are called, what type each takes, which are required, and which
// are drawn from a closed set.
//
// It is deliberately not a general schema language. Everything JSON Schema adds
// beyond this — nested objects, oneOf, pattern constraints — would end up
// rendered back into the model's context, and "not the whole schema again" is
// the requirement (docs/SLICE-1.md §3).
type Schema struct {
	// Name is the tool's name as the model must spell it.
	Name string

	// Params are the tool's arguments, in the order a repair message should
	// show them. Order is the tool's choice, not sorted here: the tool knows
	// which argument a reader looks for first.
	Params []Param
}

// Param is one tool argument.
type Param struct {
	// Name is the key inside the arguments object.
	Name string

	// Type is the JSON type the argument accepts. The zero value, [TypeAny],
	// declares nothing and is therefore never a type failure.
	Type ParamType

	// Required marks an argument the tool cannot run without.
	Required bool

	// Enum, when non-empty, is the closed set of string values accepted.
	Enum []string
}

// ParamType is the JSON type an argument accepts.
type ParamType uint8

const (
	// TypeAny is the zero value and declares no constraint, so it can never
	// produce a [KindWrongArgumentType]. A tool that forgot to declare a type
	// must not have its calls rejected on the harness's guess.
	TypeAny ParamType = iota

	TypeString
	TypeNumber
	TypeInteger
	TypeBoolean
	TypeObject
	TypeArray
)

var paramTypeText = map[ParamType]string{
	TypeAny:     "any",
	TypeString:  "string",
	TypeNumber:  "number",
	TypeInteger: "integer",
	TypeBoolean: "boolean",
	TypeObject:  "object",
	TypeArray:   "array",
}

// String returns the name of the type as the repair message spells it to the
// model.
func (t ParamType) String() string {
	if s, ok := paramTypeText[t]; ok {
		return s
	}
	return fmt.Sprintf("param_type(%d)", uint8(t))
}

// accepts reports whether v is a value of this type.
//
// A JSON null is handled by the caller as an absent argument, so it never
// reaches here; everything else is judged on its first byte, which is all JSON
// needs to distinguish its types.
func (t ParamType) accepts(v json.RawMessage) bool {
	switch t {
	case TypeAny:
		return true
	case TypeString:
		return jsonKind(v) == "string"
	case TypeNumber:
		return jsonKind(v) == "number"
	case TypeInteger:
		if jsonKind(v) != "number" {
			return false
		}
		_, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
		return err == nil
	case TypeBoolean:
		return jsonKind(v) == "boolean"
	case TypeObject:
		return jsonKind(v) == "object"
	case TypeArray:
		return jsonKind(v) == "array"
	default:
		// An unknown type constant is a programming error in the tool that
		// declared it, and rejecting the model's call for it would attribute a
		// harness bug to the model. Accept, and let the tool fail honestly.
		return true
	}
}

// jsonKind names the JSON type of an already-valid JSON value. It reports ""
// for anything empty, which callers treat as absent.
func jsonKind(v json.RawMessage) string {
	t := strings.TrimSpace(string(v))
	if t == "" {
		return ""
	}
	switch t[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// shape renders the one correct call shape for this tool.
//
// One line, the tool's own arguments and nothing else. This is the whole
// difference between a repair message that recovers a weak model and one that
// spends 900 tokens re-stating a catalogue the model already has.
func (s Schema) shape() string {
	var b strings.Builder
	b.WriteString(`{"name": `)
	b.WriteString(strconv.Quote(s.Name))
	b.WriteString(`, "arguments": {`)
	for i, p := range s.Params {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(strconv.Quote(p.Name))
		b.WriteString(": ")
		b.WriteString(p.placeholder())
	}
	b.WriteString("}}")
	return b.String()
}

// placeholder renders one argument slot: its type, whether it is required, and
// its accepted values when the set is closed.
func (p Param) placeholder() string {
	var parts []string
	if len(p.Enum) > 0 {
		parts = append(parts, "one of "+strings.Join(quoteAll(p.Enum), "|"))
	} else {
		parts = append(parts, p.Type.String())
	}
	if p.Required {
		parts = append(parts, "required")
	} else {
		parts = append(parts, "optional")
	}
	return "<" + strings.Join(parts, ", ") + ">"
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return out
}
