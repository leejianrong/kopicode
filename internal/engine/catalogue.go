package engine

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/leejianrong/kopicode/internal/parse"
)

// The argument objects the model fills in, one per tool.
//
// **These structs are the single source of the model-facing argument names.**
// The JSON tag is what the model must write, the field type is what
// parse.validate holds a call to, and the `kopicode:"required"` tag is what
// makes an absent argument a repairable failure rather than a zero value the
// tool then fails on. [Catalogue] derives every parse.Schema from them by
// reflection, so a schema cannot describe an argument the decoder does not read
// and the decoder cannot read one the repair message never mentions. A second,
// hand-written table would have exactly that drift, and its symptom is a repair
// message telling a model to send a key nothing decodes.
//
// What this is *not*: the tool catalogue as rendered on the wire.
// provider.Request carries no tool definitions and this card adds none —
// parse.Schema has no room for the per-tool and per-argument descriptions the
// wire format wants, which is why choosing that shape is KAN-844 rather than a
// transcription. The system prompt that describes these tools in prose is
// KAN-843. Both inherit the names below; neither is settled here.
type (
	readFileArgs struct {
		Path   string `json:"path" kopicode:"required"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}

	listDirArgs struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
		Pattern   string `json:"pattern"`
	}

	grepArgs struct {
		Pattern    string `json:"pattern" kopicode:"required"`
		Path       string `json:"path"`
		Include    string `json:"include"`
		IgnoreCase bool   `json:"ignore_case"`
	}

	writeFileArgs struct {
		Path    string `json:"path" kopicode:"required"`
		Content string `json:"content" kopicode:"required"`
	}

	editFileArgs struct {
		Path        string `json:"path" kopicode:"required"`
		AnchorStart string `json:"anchor_start" kopicode:"required"`
		AnchorEnd   string `json:"anchor_end" kopicode:"required"`
		// NewText is required and may be empty: an empty replacement deletes
		// the region outright, which is the tool's documented spelling of a
		// deletion. Required so that omitting it is a repairable mistake rather
		// than an accidental delete.
		NewText string `json:"new_text" kopicode:"required"`
	}

	editFileFuzzyArgs struct {
		Path   string `json:"path" kopicode:"required"`
		Before string `json:"before" kopicode:"required"`
		After  string `json:"after" kopicode:"required"`
	}

	runShellArgs struct {
		Command string `json:"command" kopicode:"required"`
		Dir     string `json:"dir"`
		// TimeoutSeconds overrides the tool set's default. Seconds and not a
		// duration string, because a model that has to spell "2m30s" correctly
		// is a repair round trip nobody needed.
		TimeoutSeconds int `json:"timeout_seconds"`
	}
)

// Catalogue returns the schemas for the named tools, which is what the repair
// loop consults to tell a model the shape of the one call it got wrong.
//
// names is the harness configuration's ToolSet — the tools presented to the
// model, and therefore the tools that exist as far as this session is concerned.
// It is taken as an argument rather than read off a package-level set because
// ADR-0007 decision 6 puts the tool set inside the harness config hash: a
// catalogue that always offered everything would make an arm that presents fewer
// tools hash differently and behave identically, which is the hash describing
// something that is not happening.
//
// A name no tool answers is an error rather than a silent omission. The
// alternative is a harness configuration that promises a tool the binary does
// not have, discovered when a model calls it and gets a harness failure.
func Catalogue(names []string) (parse.Tools, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("engine: a tool set with no tools leaves the model nothing to call")
	}
	c := catalogue{schemas: make(map[string]parse.Schema, len(names))}
	for _, name := range names {
		entry, ok := toolByName[name]
		if !ok {
			return nil, fmt.Errorf("engine: the harness configuration presents %q and no tool answers it; "+
				"this binary dispatches %s", name, strings.Join(ToolNames(), ", "))
		}
		if _, dup := c.schemas[name]; dup {
			return nil, fmt.Errorf("engine: the harness configuration presents %q twice", name)
		}
		c.names = append(c.names, name)
		c.schemas[name] = entry.schema
	}
	slices.Sort(c.names)
	return c, nil
}

// ToolNames is every tool this binary can dispatch, sorted.
//
// It is what a harness configuration's ToolSet is drawn from and what KAN-843's
// system prompt and KAN-844's wire catalogue will both need. Sorted and copied,
// so nothing downstream depends on the dispatch table's own order.
func ToolNames() []string {
	out := make([]string, 0, len(toolEntries))
	for _, entry := range toolEntries {
		out = append(out, entry.name)
	}
	slices.Sort(out)
	return out
}

// catalogue is a parse.Tools over a fixed set of schemas.
//
// names is a slice, sorted once, and Names returns a copy of it. A map range
// would put Go's randomised iteration order into an unknown-tool repair
// message, and a repair message that reads differently on every run is one a
// bench arm cannot be scored against.
type catalogue struct {
	names   []string
	schemas map[string]parse.Schema
}

// Schema returns the named tool's schema. Lookup is exact — a near miss is a
// finding the repair message reports, not something to resolve silently.
func (c catalogue) Schema(name string) (parse.Schema, bool) {
	s, ok := c.schemas[name]
	return s, ok
}

// Names returns every tool name offered, sorted, as a fresh slice.
func (c catalogue) Names() []string { return slices.Clone(c.names) }

// schemaOf derives a tool's schema from its argument struct.
//
// Panics on a struct it cannot describe, which is a package-initialisation
// panic and therefore a compile-then-immediately-fail rather than a runtime
// surprise: an argument type nobody mapped would otherwise reach the model as
// parse.TypeAny, which never produces a type failure, and the repair loop would
// silently stop checking that argument.
func schemaOf(name string, args any) parse.Schema {
	t := reflect.TypeOf(args)
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("engine: schema for %s: %s is not a struct", name, t))
	}

	schema := parse.Schema{Name: name}
	for i := range t.NumField() {
		f := t.Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if key == "" || key == "-" {
			panic(fmt.Sprintf("engine: schema for %s: field %s has no json name, "+
				"so the model would be told to send a key nothing decodes", name, f.Name))
		}
		schema.Params = append(schema.Params, parse.Param{
			Name:     key,
			Type:     paramType(name, f),
			Required: slices.Contains(strings.Split(f.Tag.Get("kopicode"), ","), "required"),
		})
	}
	return schema
}

// paramType maps a Go field type onto the JSON type the repair loop checks
// against.
func paramType(tool string, f reflect.StructField) parse.ParamType {
	switch f.Type.Kind() {
	case reflect.String:
		return parse.TypeString
	case reflect.Bool:
		return parse.TypeBoolean
	case reflect.Int:
		return parse.TypeInteger
	case reflect.Float64:
		return parse.TypeNumber
	default:
		panic(fmt.Sprintf("engine: schema for %s: field %s has unmapped type %s; "+
			"an unmapped type would be declared parse.TypeAny, which can never fail a "+
			"type check, so the repair loop would quietly stop validating it",
			tool, f.Name, f.Type))
	}
}
