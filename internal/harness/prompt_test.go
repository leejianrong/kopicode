package harness_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// TestSystemPromptIsInTheConfigHash is KAN-843's done-when, stated as the one
// thing that can be checked rather than argued: changing the prompt changes the
// harness config hash journaled on SessionStarted.
//
// TestHashCoversEveryConfigField already varies every field by reflection, and
// SystemPrompt is a string so it is genuinely covered rather than merely
// appearing to be. This test is here anyway, because that one asserts a
// property of the *struct* and this one asserts the property of the *prompt* —
// if someone were to hash the prompt's length, or a normalised form of it, or
// hash nothing and leave a field that happens to move for another reason, the
// reflection test would still pass and this one would not.
func TestSystemPromptIsInTheConfigHash(t *testing.T) {
	base := defaultConfig(t)

	// Positive control: the hash must be stable for an unchanged prompt, or
	// every "the hash moved" assertion below passes for the wrong reason.
	if base.Hash() != defaultConfig(t).Hash() {
		t.Fatalf("the same configuration hashed twice gave %s and %s; nothing below means anything",
			base.Hash(), defaultConfig(t).Hash())
	}

	if base.SystemPrompt == "" {
		t.Fatal("the shipped harness configuration carries no system prompt")
	}

	cases := map[string]string{
		"one character appended":  base.SystemPrompt + " ",
		"one character removed":   base.SystemPrompt[:len(base.SystemPrompt)-1],
		"leading newline added":   "\n" + base.SystemPrompt,
		"no prompt at all":        "",
		"whitespace-only rewrite": strings.ReplaceAll(base.SystemPrompt, "\n", " "),
	}
	for name, prompt := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := base
			mutated.SystemPrompt = prompt
			if mutated.Hash() == base.Hash() {
				t.Errorf("%s left the harness config hash at %s\n"+
					"the system prompt is in the hash preimage by digest "+
					"(docs/adr/0007-model-selection-and-harness-config-shape.md decision 6): "+
					"two arms running different prompts would otherwise pool as one",
					name, base.Hash())
			}
		})
	}
}

// TestSystemPromptDocumentsEveryTool holds the prompt's tool sections to the
// configuration's tool set, in order and in both directions.
//
// The prompt is currently the *only* description of the tools the model gets:
// provider.Request carries no tool definitions, so there is no rendered
// catalogue on the wire (KAN-844 owns that shape). A tool registered and not
// described is a tool the model cannot call; a tool described and not
// registered is an instruction to call something that is not there, and the
// model spends a turn discovering it.
//
// The expectation is the configuration's ToolSet, which
// TestToolSetMatchesInternalTools already holds to internal/tools' own
// constants — so this closes the chain from the source to the prose without a
// hand-written list anywhere in it.
func TestSystemPromptDocumentsEveryTool(t *testing.T) {
	cfg := defaultConfig(t)
	sections := promptSections(t, cfg.SystemPrompt)

	// Positive control: a section scan that found nothing would accept a prompt
	// documenting no tools at all.
	if len(sections) == 0 {
		t.Fatalf("positive control failed: no `### <tool>` section was found in the system prompt, "+
			"so this test would accept any prose at all\nprompt begins:\n%s",
			firstLines(cfg.SystemPrompt, 5))
	}

	var documented []string
	for _, s := range sections {
		documented = append(documented, s.name)
	}

	if !slices.Equal(documented, cfg.ToolSet) {
		t.Errorf("the system prompt documents %v; harness configuration %q presents %v\n"+
			"the prompt is the only tool description the model receives (provider.Request carries "+
			"no tool definitions yet, KAN-844), and ToolSet's order is the order the model is shown "+
			"them, so the two must agree exactly",
			documented, cfg.Name, cfg.ToolSet)
	}
}

// TestSystemPromptDocumentsEveryToolArgument holds the argument names in the
// prompt to the argument structs in internal/engine's catalogue.
//
// This is the guard that matters most on this card, and the source it reads is
// the one thing about it that changed since the card was first written. The
// model-facing argument names now exist in exactly one place — the `json` tags
// on the argument structs in internal/engine/catalogue.go, which
// engine.schemaOf turns into the parse.Schema the repair loop validates a call
// against and quotes back in a repair message. The names in internal/tools'
// request structs are Go-side and the model never sees them; on run_shell the
// two genuinely disagree, because a ShellRequest.Timeout is a time.Duration and
// what the model sends is `timeout_seconds`.
//
// So an argument the prompt does not name is one the model will not send, and
// an argument the prompt names that the catalogue does not carry is one the
// decoder will reject. There is still no tool catalogue on the wire —
// provider.Request carries no tool definitions, and KAN-844 owns that shape —
// so the prose is the only *description* the model gets. The names it uses are
// no longer unverifiable, which is what this test now proves.
//
// The expectation is read out of engine's source rather than imported: engine
// imports harness (internal/engine/selection.go), so the dependency cannot run
// the other way. internal/harness/registry_test.go takes the same route into
// internal/tools for the same reason.
func TestSystemPromptDocumentsEveryToolArgument(t *testing.T) {
	cfg := defaultConfig(t)
	sections := promptSections(t, cfg.SystemPrompt)
	args := catalogueArgs(t)

	byName := make(map[string]promptSection, len(sections))
	for _, s := range sections {
		byName[s.name] = s
	}

	// Both directions, because a tool whose argument struct nobody wrote is as
	// invisible to this test as one nobody documented.
	catalogued := slices.Sorted(maps.Keys(args))
	offered := slices.Sorted(slices.Values(cfg.ToolSet))
	if !slices.Equal(catalogued, offered) {
		t.Fatalf("internal/engine's catalogue carries argument structs for %v; harness "+
			"configuration %q presents %v\nthe struct name is the tool name in lowerCamel plus "+
			"`Args`, and a mismatch means either a tool the engine cannot decode a call for or "+
			"one whose arguments this test is silently not checking",
			catalogued, cfg.Name, offered)
	}

	checked := 0
	for _, tool := range cfg.ToolSet {
		section, ok := byName[tool]
		if !ok {
			t.Errorf("the system prompt has no `### %s` section", tool)
			continue
		}
		names := args[tool]
		if len(names) == 0 {
			t.Errorf("positive control failed: the argument struct for %q parsed with no json "+
				"tags, so the tool is unchecked", tool)
			continue
		}

		for _, arg := range names {
			checked++
			// As a quoted JSON key, not as a bare word: `path` and `pattern`
			// occur in ordinary prose, and a substring match on those would
			// pass for a section that never shows the argument at all.
			if !strings.Contains(section.body, `"`+arg+`"`) {
				t.Errorf("the system prompt's %s section never shows %q as a JSON key\n"+
					"the json tags in internal/engine/catalogue.go are the only place the "+
					"model-facing argument names exist, and the prompt is the only place the "+
					"model is told them, so an argument missing here is one it cannot send",
					tool, arg)
			}
		}
	}

	// Positive control: a walk that checked nothing would pass silently.
	if checked < len(cfg.ToolSet) {
		t.Fatalf("positive control failed: only %d arguments were checked across %d tools",
			checked, len(cfg.ToolSet))
	}
}

// TestSystemPromptAnswersEveryRejectReason requires the prompt to name every
// value edit_file and edit_file_fuzzy can refuse with.
//
// ADR-0006's whole design is that a refusal is the product: the edit fails
// closed and the model retries against reality. That only pays if the model
// knows what each reason means, and the four anchored reasons want four
// *different* recoveries — re-anchoring from the anchors the rejection printed,
// picking a unique anchor, swapping two arguments, or fixing the shape. A model
// that answers all of them by re-reading the file burns a turn each time, which
// is SLICE-1 risk 2.
//
// The reasons are read out of internal/tools' source, so a fifth one added
// without prompt guidance fails here rather than showing up as a retry loop in
// a bench run.
func TestSystemPromptAnswersEveryRejectReason(t *testing.T) {
	cfg := defaultConfig(t)
	reasons := toolsRejectReasons(t)

	// Positive control: an extraction that found nothing would accept a prompt
	// that explains no rejection at all.
	if len(reasons) == 0 {
		t.Fatal("positive control failed: no RejectReason constants were found in internal/tools, " +
			"so this test would accept a prompt that documents none of them")
	}

	for _, reason := range reasons {
		if !strings.Contains(cfg.SystemPrompt, reason) {
			t.Errorf("the system prompt never names the rejection reason %q\n"+
				"internal/tools can refuse an edit with it, and each reason implies a different "+
				"recovery (docs/adr/0006-hash-anchored-edits-and-failure-attribution.md); a reason "+
				"the prompt does not explain is a turn spent guessing", reason)
		}
	}
}

// TestSystemPromptStatesTheAnchorVersion couples the prose to ADR-0006 §7's
// experiment-series boundary.
//
// The version string costs the prompt a handful of tokens and tells the model
// nothing it can act on. It is there for what this test does with it: bumping
// anchor.Version changes every anchor the model will ever see, and ADR-0006 §7
// calls that an experiment-series boundary. Failing here forces whoever bumps
// it to open the prompt and decide whether the anchor instructions still
// describe the format — which is the review the boundary is supposed to
// trigger, made mechanical instead of remembered.
func TestSystemPromptStatesTheAnchorVersion(t *testing.T) {
	cfg := defaultConfig(t)
	if !strings.Contains(cfg.SystemPrompt, anchor.Version) {
		t.Errorf("the system prompt does not contain the anchor contract version %q\n"+
			"a bump to anchor.Version changes every anchor the model sees and opens a new "+
			"experiment series (ADR-0006 §7); the prompt states the version so that a bump "+
			"cannot happen without someone reading the anchor instructions again", anchor.Version)
	}
}

// promptSection is one `### <tool>` block of the prompt.
type promptSection struct {
	name string
	body string
}

// promptSections splits the prompt on its third-level headings, which is the
// structure the tool catalogue is written in. A section runs to the next
// heading of any level.
func promptSections(t *testing.T, prompt string) []promptSection {
	t.Helper()

	var out []promptSection
	var body strings.Builder
	// open says whether the text being accumulated belongs to a section at all.
	// Without it, prose outside any `###` heading is written into whichever
	// section happened to be last — which silently gave every tool the *next*
	// section's text, and let a substring match pass on words the tool's own
	// paragraph never contained.
	open := false
	flush := func() {
		if open {
			out[len(out)-1].body = body.String()
			open = false
		}
		body.Reset()
	}
	for _, line := range strings.Split(prompt, "\n") {
		switch {
		case strings.HasPrefix(line, "### "):
			flush()
			out = append(out, promptSection{name: strings.TrimSpace(line[4:])})
			open = true
		case strings.HasPrefix(line, "#"):
			flush()
		default:
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()
	return out
}

// catalogueArgs returns the model-facing argument names of every tool, keyed by
// tool name, read out of internal/engine/catalogue.go.
//
// The keying is the one convention this depends on: a tool's argument struct is
// its name in lowerCamel with `Args` appended, so runShellArgs belongs to
// run_shell. That is checked rather than trusted — the caller compares the whole
// key set against the configuration's ToolSet, so a struct named off-convention
// shows up as a missing tool rather than as an unchecked one.
func catalogueArgs(t *testing.T) map[string][]string {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	path := filepath.Join(root, "internal", "engine", "catalogue.go")

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	out := make(map[string][]string)
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok || !strings.HasSuffix(ts.Name.Name, "Args") {
			return true
		}
		tool := snakeCase(strings.TrimSuffix(ts.Name.Name, "Args"))
		var names []string
		for _, f := range st.Fields.List {
			if f.Tag == nil {
				continue
			}
			tag, err := strconv.Unquote(f.Tag.Value)
			if err != nil {
				t.Fatalf("unquoting the struct tag on %s.%v: %v", ts.Name.Name, f.Names, err)
			}
			name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
			if name == "" || name == "-" {
				// engine.schemaOf panics on this at package init, so it cannot
				// reach a run; failing here says so in the place that noticed.
				t.Errorf("%s.%v has no json name, so no argument name exists for the prompt to "+
					"document", ts.Name.Name, f.Names)
				continue
			}
			names = append(names, name)
		}
		out[tool] = names
		return true
	})

	if len(out) == 0 {
		t.Fatalf("positive control failed: no *Args structs were found in %s, so this test would "+
			"accept a prompt documenting no arguments at all", path)
	}
	return out
}

// toolsRejectReasons returns the string value of every RejectReason constant
// internal/tools declares, in source order.
func toolsRejectReasons(t *testing.T) []string {
	t.Helper()

	var out []string
	for _, file := range parseToolsPackage(t) {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				ident, ok := vs.Type.(*ast.Ident)
				if !ok || ident.Name != "RejectReason" {
					continue
				}
				for _, value := range vs.Values {
					lit, ok := value.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					unquoted, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquoting %s: %v", lit.Value, err)
					}
					out = append(out, unquoted)
				}
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// parseToolsPackage parses internal/tools' non-test source.
//
// Parsing rather than importing keeps internal/harness independent of
// internal/tools: the configuration is a value, not a view onto another
// package's registry, and TestToolSetMatchesInternalTools takes the same route
// for the same reason.
func parseToolsPackage(t *testing.T) []*ast.File {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	dir := filepath.Join(root, "internal", "tools")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	var files []*ast.File
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("positive control failed: no source files were parsed from %s", dir)
	}
	return files
}

// snakeCase converts a Go identifier to its snake_case spelling: editFileFuzzy
// becomes edit_file_fuzzy.
func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// firstLines returns the first n lines of s, for a failure message that shows
// what was actually there.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	return strings.Join(lines[:min(n, len(lines))], "\n")
}
