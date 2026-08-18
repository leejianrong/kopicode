package provider

import (
	"bytes"
	"encoding/json"

	"github.com/leejianrong/kopicode/internal/parse"
)

// This file is the wire shape for KAN-844: rendering the harness's tool
// catalogue into the chat-completions request's `tools` array.
//
// # Why this shape
//
// It is OpenAI's documented function-calling contract — `{"type": "function",
// "function": {"name", "description", "parameters"}}`, with `parameters` a
// JSON Schema object — because that is the contract internal/provider/fixture
// already commits to for the *streamed tool-call delta* shape (see wire.go's
// own note: "OpenRouter documents nowhere... the fixtures follow OpenAI's
// contract and say so"), and a *second* wire convention for the request side
// of the same feature would be a harness inventing a dialect no provider in
// front of it necessarily speaks. OpenRouter proxies the request largely
// unchanged to whichever upstream serves the pinned model, so the contract
// that upstream expects is the one to send.
//
// # Why Properties is a slice and not a map
//
// JSON Schema's own `properties` keyword is an object keyed by argument name,
// and that is exactly what [ToolParameters.MarshalJSON] puts on the wire. But
// the Go value behind it, [ToolProperty], is an ordered slice: this type
// reaches internal/harness's Config through [Config.ToolCatalogue], and
// TestConfigHoldsNoMap enforces — by walking the whole type graph, not just
// Config's direct fields — that no field anywhere under a harness
// configuration is a map, because Go randomises map iteration order and a
// preimage built from one hashes differently in every process. So the object
// the wire actually wants is assembled by hand from the ordered slice, in
// [ToolParameters.MarshalJSON], rather than expressed as a Go map that
// encoding/json would (deterministically, since it sorts map keys) render the
// same bytes from — the byte-determinism was never the problem; the field
// existing at all as a map was.
//
// # Determinism
//
// [RenderTools] does nothing but walk a []parse.Schema, in order, and each
// Schema's own Params, in order (parse.Schema's own doc comment: "Order is the
// tool's choice, not sorted here"). No map, no goroutine, so the same input
// schemas render the same bytes on every call in every process — the same
// property this package's stream accumulator holds for tool-call deltas.

// ToolDefinition is one entry of the wire's `tools` array.
type ToolDefinition struct {
	// Type is always "function": the only kind this format defines and the
	// only kind kopicode's tools are.
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is a tool definition's name, description and argument schema.
type ToolFunction struct {
	Name string `json:"name"`
	// Description is one clause saying what the tool does, straight from
	// parse.Schema.Description. Omitted when empty rather than sent as "",
	// which a provider could read as "documented to do nothing" instead of
	// "undocumented".
	Description string         `json:"description,omitempty"`
	Parameters  ToolParameters `json:"parameters"`
}

// ToolParameters is a tool's arguments object, rendered as the JSON Schema a
// function-calling `parameters` field is documented to hold.
//
// It is always `{"type": "object", ...}`: every kopicode tool takes one
// arguments object, never a bare scalar or array, which is what
// internal/parse's own extraction routes assume too.
type ToolParameters struct {
	// Properties is every argument, in the tool's own order. See the file doc
	// comment for why this is a slice and not the map the JSON it renders as
	// would suggest.
	Properties []ToolProperty
	// Required is the subset of Properties' names the tool cannot run
	// without, in the same order [parse.Schema.Params] declared them.
	Required []string
}

// ToolProperty is one argument's JSON Schema entry: its name (the object key
// [ToolParameters.MarshalJSON] renders it under), its type, its description
// and, when the value is drawn from a closed set, its enum.
type ToolProperty struct {
	Name        string
	Type        string
	Description string
	// Enum, when non-empty, is the closed set of accepted string values.
	Enum []string
}

// MarshalJSON renders the JSON Schema object a `parameters` field is
// documented to hold: `{"type": "object", "properties": {...}, "required":
// [...]}` with `properties` a genuine JSON object, built by hand from the
// ordered slice rather than from a Go map — see the file doc comment.
func (p ToolParameters) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"type":"object","properties":{`)
	for i, prop := range p.Properties {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(prop.Name)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := json.Marshal(propertySchemaOf(prop))
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	if len(p.Required) > 0 {
		req, err := json.Marshal(p.Required)
		if err != nil {
			return nil, err
		}
		buf.WriteString(`,"required":`)
		buf.Write(req)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// propertySchema is one property's own JSON Schema entry — everything about
// [ToolProperty] except the name, which is the object key it is filed under
// rather than a field inside the value.
type propertySchema struct {
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// propertySchema constructs a propertySchema from a ToolProperty, dropping the
// name.
func propertySchemaOf(p ToolProperty) propertySchema {
	return propertySchema{Type: p.Type, Description: p.Description, Enum: p.Enum}
}

// jsonSchemaType maps a parse.ParamType onto the JSON Schema type keyword.
//
// [parse.TypeAny] returns "": the zero value declares no constraint, and a
// rendered schema that said "any" would be inventing a JSON Schema type that
// does not exist. Every tool this binary ships declares a real type for every
// argument (engine.schemaOf panics on a field it cannot map), so this case is
// only reached by a caller building its own schemas, and omitting the keyword
// is the honest rendering of "unconstrained".
func jsonSchemaType(t parse.ParamType) string {
	switch t {
	case parse.TypeAny:
		return ""
	default:
		return t.String()
	}
}

// RenderTools turns a tool catalogue into the wire's `tools` array, in the
// order schemas is given. Nil or empty in produces nil out, so a caller can
// pass an unfiltered catalogue and get "send no tools array at all" for free
// when there is nothing to advertise.
func RenderTools(schemas []parse.Schema) []ToolDefinition {
	if len(schemas) == 0 {
		return nil
	}
	out := make([]ToolDefinition, len(schemas))
	for i, s := range schemas {
		fn := ToolFunction{Name: s.Name, Description: s.Description}
		for _, p := range s.Params {
			prop := ToolProperty{
				Name:        p.Name,
				Type:        jsonSchemaType(p.Type),
				Description: p.Description,
				Enum:        p.Enum,
			}
			fn.Parameters.Properties = append(fn.Parameters.Properties, prop)
			if p.Required {
				fn.Parameters.Required = append(fn.Parameters.Required, p.Name)
			}
		}
		out[i] = ToolDefinition{Type: "function", Function: fn}
	}
	return out
}
