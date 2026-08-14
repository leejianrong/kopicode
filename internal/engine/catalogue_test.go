package engine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
)

// toolNamesFromSource reads the tool-name constants out of internal/tools with
// go/ast, rather than repeating them here.
//
// A hand-written list is the failure this avoids: the eighth tool lands, nobody
// adds it to the list, and the completeness checks below go on passing while
// covering seven of eight. The traversal is checked by its own positive control
// in the test that calls this.
func toolNamesFromSource(t *testing.T) []string {
	t.Helper()

	path := filepath.Join("..", "tools", "tools.go")
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	var names []string
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if len(ident.Name) < 5 || ident.Name[:4] != "Tool" || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquoting %s: %v", lit.Value, err)
				}
				names = append(names, value)
			}
		}
	}
	slices.Sort(names)
	return names
}

// TestCatalogueCoversEveryTool holds the catalogue to the tool set.
//
// A tool the catalogue does not describe cannot be repaired towards: the model
// gets a generic message instead of that one tool's shape, which is the failure
// docs/SLICE-1.md §3 exists to avoid. A name the catalogue offers and no tool
// answers is worse — the model is invited to call something that will then be
// journaled as a harness failure.
func TestCatalogueCoversEveryTool(t *testing.T) {
	names := toolNamesFromSource(t)

	// Positive control. If the AST walk found nothing, or found only some of
	// the tools this repo is known to ship, then every assertion below passes
	// over an empty or partial list and asserts nothing.
	if len(names) < 7 {
		t.Fatalf("positive control failed: the source walk found %d tool constants (%v); "+
			"internal/tools ships at least 7, so the traversal is broken", len(names), names)
	}
	for _, want := range []string{"read_file", "edit_file", "edit_file_fuzzy", "run_shell"} {
		if !slices.Contains(names, want) {
			t.Fatalf("positive control failed: the source walk did not find %q among %v", want, names)
		}
	}

	cat := engine.Catalogue()
	for _, name := range names {
		schema, ok := cat.Schema(name)
		if !ok {
			t.Errorf("no schema for tool %q; the repair loop cannot tell a model the shape of a call "+
				"it has no schema for", name)
			continue
		}
		if schema.Name != name {
			t.Errorf("schema for %q names itself %q", name, schema.Name)
		}
		if len(schema.Params) == 0 {
			t.Errorf("schema for %q declares no arguments, so no call of it can ever be wrong", name)
		}
	}

	if diff := slices.Compare(names, cat.Names()); diff != 0 {
		t.Errorf("catalogue names %v, tools declare %v", cat.Names(), names)
	}

	// Negative control on the lookup itself: it must be exact, so a near miss
	// is a finding rather than something resolved silently.
	if _, ok := cat.Schema("read_fil"); ok {
		t.Error("the catalogue resolved a misspelled tool name")
	}
}

// permissiveTools is a parse.Tools that recognises every name and declares no
// arguments, so a call reaches the dispatcher whatever it looks like.
//
// It exists so the test below is about the *dispatcher's* coverage. Under the
// real catalogue an empty argument object is rejected by the repair loop for a
// missing required argument, which is correct behaviour and would mean the
// dispatch table was never consulted at all.
type permissiveTools struct{ names []string }

func (p permissiveTools) Schema(name string) (parse.Schema, bool) {
	return parse.Schema{Name: name}, true
}
func (p permissiveTools) Names() []string { return slices.Clone(p.names) }

// TestEveryCatalogueToolDispatches is the other half of the pairing: every name
// the catalogue offers reaches a tool.
//
// It drives a real session per tool rather than reading the dispatch table,
// because the table is unexported and because what matters is the observable
// outcome — a ToolCallFailed naming an unknown tool is exactly what a model
// would get.
func TestEveryCatalogueToolDispatches(t *testing.T) {
	names := engine.Catalogue().Names()
	if len(names) < 7 {
		t.Fatalf("positive control failed: the catalogue offers %d tools (%v)", len(names), names)
	}

	unknownFor := func(t *testing.T, tool string) bool {
		t.Helper()
		replies := []scriptedReply{
			{calls: []wireCall{nativeCall("call-1", tool, `{}`)}, usage: wireUsage{Prompt: 1, Completion: 1, Total: 2}},
			{text: "done.", usage: wireUsage{Prompt: 2, Completion: 1, Total: 3}},
		}
		h := scriptHarness(t, replies, oneAttemptPerTurn(2), nil,
			withMaxTurns(2), withCatalogue(permissiveTools{names: names}))
		if _, err := h.eng.Run(t.Context(), "go"); err != nil {
			t.Fatalf("Run for %s: %v", tool, err)
		}
		for _, failed := range payloadsOf[journal.ToolCallFailed](t, h.events()) {
			if failed.Reason == parse.KindUnknownTool.String() {
				return true
			}
		}
		return false
	}

	// Positive control: a name nothing dispatches must be detected, or the
	// loop below is asserting the absence of something that never appears.
	if !unknownFor(t, "teleport") {
		t.Fatal("positive control failed: a name no tool answers did not produce an unknown-tool failure")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if unknownFor(t, name) {
				t.Errorf("the catalogue offers %q and no tool answers it", name)
			}
		})
	}
}
