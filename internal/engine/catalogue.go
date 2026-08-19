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
// **These structs are the single source of the model-facing argument names —
// and, since KAN-844, their descriptions too.** The JSON tag is what the model
// must write, the field type is what parse.validate holds a call to, the
// `kopicode:"required"` tag is what makes an absent argument a repairable
// failure rather than a zero value the tool then fails on, and the
// `kopicode_desc` tag is the one clause a rendered wire catalogue says about
// the argument. [Catalogue] derives every parse.Schema from them by
// reflection, so a schema cannot describe an argument the decoder does not read
// and the decoder cannot read one the repair message never mentions, and a
// wire catalogue cannot describe an argument differently from what the repair
// loop already validates it against. A second, hand-written table would have
// exactly that drift, and its symptom used to be a repair message telling a
// model to send a key nothing decodes; for a description, the symptom would be
// a wire catalogue and a repair message disagreeing about what an argument
// means.
//
// `kopicode_desc` is a distinct tag key rather than another `kopicode:"..."`
// token: that tag is a comma-separated set (`required` today), and a
// description is free prose that may itself contain a comma, so packing it in
// would need an escaping scheme for no benefit a second key does not already
// give for free.
//
// Each tool's own description — one clause, not a struct field, because it
// describes the whole call rather than one argument — is the second argument
// to [entryOf] (see dispatch.go), threaded through to [schemaOf] alongside the
// zero value of the tool's argument struct. Keeping it there rather than in a
// package-level map keyed by tool name is the same call ToolSet's own
// duplication note makes: a lookup table addressable by name is a second place
// a tool's identity lives, and the ordinary way for that to drift is a rename
// on one side.
//
// What this is *not*: the tool catalogue as rendered on the wire. That is
// provider.RenderTools, in internal/provider, which turns a []parse.Schema —
// built from exactly these structs — into the wire's tool-definition objects
// (KAN-844). Nothing about the wire's JSON Schema shape lives here; this file
// only says what the tools are, and RenderTools says how to spell that for a
// provider. The system prompt that describes these tools in prose is KAN-843.
// All three inherit the names and descriptions below; none of them is settled
// here.
type (
	readFileArgs struct {
		Path   string `json:"path" kopicode:"required" kopicode_desc:"the file's path, relative to the repository root"`
		Offset int    `json:"offset" kopicode_desc:"1-based line number to start returning from; 0 or omitted means the beginning of the file"`
		Limit  int    `json:"limit" kopicode_desc:"maximum number of lines to return; 0 or omitted uses the tool's per-call maximum"`
	}

	listDirArgs struct {
		Path      string `json:"path" kopicode_desc:"the directory's path, relative to the repository root; empty means the repository root"`
		Recursive bool   `json:"recursive" kopicode_desc:"when true, also list nested directories, skipping .git and .kopicode"`
		Pattern   string `json:"pattern" kopicode_desc:"a glob matched against an entry's file name, or its whole relative path when the pattern contains a slash"`
	}

	grepArgs struct {
		Pattern    string `json:"pattern" kopicode:"required" kopicode_desc:"the RE2 regular expression to search for; no backreferences or lookahead"`
		Path       string `json:"path" kopicode_desc:"the file or directory to search, relative to the repository root; empty means the whole repository"`
		Include    string `json:"include" kopicode_desc:"a glob restricting which file names are searched"`
		IgnoreCase bool   `json:"ignore_case" kopicode_desc:"when true, match case-insensitively"`
	}

	writeFileArgs struct {
		Path    string `json:"path" kopicode:"required" kopicode_desc:"the file's path, relative to the repository root"`
		Content string `json:"content" kopicode:"required" kopicode_desc:"the file's complete new contents, replacing anything already there"`
	}

	editFileArgs struct {
		Path        string `json:"path" kopicode:"required" kopicode_desc:"the file's path, relative to the repository root"`
		AnchorStart string `json:"anchor_start" kopicode:"required" kopicode_desc:"the anchor of the region's first line, printed by a prior read_file call"`
		AnchorEnd   string `json:"anchor_end" kopicode:"required" kopicode_desc:"the anchor of the region's last line, printed by a prior read_file call; the same anchor twice replaces one line"`
		// NewText is required and may be empty: an empty replacement deletes
		// the region outright, which is the tool's documented spelling of a
		// deletion. Required so that omitting it is a repairable mistake rather
		// than an accidental delete.
		NewText string `json:"new_text" kopicode:"required" kopicode_desc:"the replacement text for the anchored region; empty deletes it"`
	}

	editFileFuzzyArgs struct {
		Path   string `json:"path" kopicode:"required" kopicode_desc:"the file's path, relative to the repository root"`
		Before string `json:"before" kopicode:"required" kopicode_desc:"text believed to be in the file, matched by similarity with whitespace normalised"`
		After  string `json:"after" kopicode:"required" kopicode_desc:"the replacement text for whatever before matched"`
	}

	runShellArgs struct {
		Command string `json:"command" kopicode:"required" kopicode_desc:"the command line to run, interpreted by the platform shell"`
		Dir     string `json:"dir" kopicode_desc:"the working directory, relative to the repository root; empty means the repository root"`
		// TimeoutSeconds overrides the tool set's default. Seconds and not a
		// duration string, because a model that has to spell "2m30s" correctly
		// is a repair round trip nobody needed.
		TimeoutSeconds int `json:"timeout_seconds" kopicode_desc:"how many seconds to allow the command to run; 0 or omitted uses the tool's default of 120"`
	}

	// askArgs is docs/adr/0009-ask-tool-contract.md decision 1's shape: two
	// strings, no choices/suggested-answers argument. That is deliberate and
	// not a placeholder — paramType below has no case for reflect.Slice and
	// panics at init on one, so a structured argument is real, separable work
	// the ADR leaves for later rather than a gap in this one. A model that
	// wants to offer options writes them into Question itself.
	askArgs struct {
		Question string `json:"question" kopicode:"required" kopicode_desc:"the single question to put to the human, phrased so it can be answered in one reply"`
		Context  string `json:"context" kopicode_desc:"what you have already tried or found and why you are stuck; shown to the human alongside the question so they do not have to read the whole session to answer it"`
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
// desc is the tool's own one-clause description — the whole call's, not one
// argument's, so it cannot live on a struct field the way an argument's
// description does. Panics on a struct it cannot describe, which is a
// package-initialisation panic and therefore a compile-then-immediately-fail
// rather than a runtime surprise: an argument type nobody mapped would
// otherwise reach the model as parse.TypeAny, which never produces a type
// check failure, and a missing description would otherwise reach the wire
// catalogue as a name and a type with nothing telling the model what either
// means — the exact handicap KAN-844 exists to close, so leaving it possible
// to forget one is leaving the gap open by omission.
func schemaOf(name, desc string, args any) parse.Schema {
	t := reflect.TypeOf(args)
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("engine: schema for %s: %s is not a struct", name, t))
	}
	if desc == "" {
		panic(fmt.Sprintf("engine: schema for %s: no tool description was given; "+
			"the wire catalogue would present a name and nothing else", name))
	}

	schema := parse.Schema{Name: name, Description: desc}
	for i := range t.NumField() {
		f := t.Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if key == "" || key == "-" {
			panic(fmt.Sprintf("engine: schema for %s: field %s has no json name, "+
				"so the model would be told to send a key nothing decodes", name, f.Name))
		}
		argDesc := f.Tag.Get("kopicode_desc")
		if argDesc == "" {
			panic(fmt.Sprintf("engine: schema for %s: field %s (%q) has no kopicode_desc tag, "+
				"so the wire catalogue would tell the model the argument's name and type and "+
				"nothing about what it means", name, f.Name, key))
		}
		schema.Params = append(schema.Params, parse.Param{
			Name:        key,
			Type:        paramType(name, f),
			Required:    slices.Contains(strings.Split(f.Tag.Get("kopicode"), ","), "required"),
			Description: argDesc,
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
