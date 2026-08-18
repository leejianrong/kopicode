package engine_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/leejianrong/kopicode/internal/engine"
	harnesscfg "github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/provider"
)

// harnesscfg, not harness: this package's own test helpers already declare a
// type named harness (harness_test.go), and the two live in the same
// engine_test binary.

// TestHarnessToolCatalogueMatchesEngineCatalogue closes the loop KAN-844's card
// leaves open by construction: harness.Config.ToolCatalogue is a literal,
// hand-written in internal/harness/harness.go, because internal/harness cannot
// import internal/engine (the dependency runs the other way —
// internal/engine/selection.go) and so cannot compute it by calling into the
// package that actually derives a tool's schema from its argument struct.
//
// That literal is only as trustworthy as this test, which renders the *real*
// catalogue — engine.Catalogue(cfg.ToolSet), the same schemas engine.schemaOf
// derives by reflection off the `json`, `kopicode` and `kopicode_desc` tags on
// catalogue.go's argument structs — through provider.RenderTools, the same
// function the live client's wire-building path calls, and requires the
// result to equal the harness literal field for field. A description edited on
// one side and not the other fails here, the same way
// harness.TestSystemPromptDocumentsEveryToolArgument catches a prompt and a
// struct tag disagreeing.
func TestHarnessToolCatalogueMatchesEngineCatalogue(t *testing.T) {
	cfg, ok := harnesscfg.ConfigByName(harnesscfg.DefaultConfigName)
	if !ok {
		t.Fatalf("no %q harness configuration is registered", harnesscfg.DefaultConfigName)
	}

	// Positive control 1: the configuration must actually carry a rendered
	// catalogue, or the comparison below passes by both sides being empty.
	if len(cfg.ToolCatalogue) == 0 {
		t.Fatal("positive control failed: the default harness configuration's ToolCatalogue is empty")
	}
	if len(cfg.ToolCatalogue) != len(cfg.ToolSet) {
		t.Fatalf("ToolCatalogue has %d entries but ToolSet lists %d tools; they must be the same "+
			"tools in the same order", len(cfg.ToolCatalogue), len(cfg.ToolSet))
	}

	cat, err := engine.Catalogue(cfg.ToolSet)
	if err != nil {
		t.Fatalf("building the engine catalogue over %v: %v", cfg.ToolSet, err)
	}

	// Rendered in ToolSet's own order — the presentation order, not
	// catalogue.Names()'s sorted one (engine.Catalogue sorts its name list for
	// a stable unknown-tool message, which is a different concern from
	// presentation order; see engine.Engine.wireTools' own doc comment).
	var schemas []parse.Schema
	for _, name := range cfg.ToolSet {
		schema, ok := cat.Schema(name)
		if !ok {
			t.Fatalf("engine.Catalogue built over %v has no schema for %q, which ToolSet lists",
				cfg.ToolSet, name)
		}
		schemas = append(schemas, schema)
	}

	// Positive control 2: every schema found must actually carry a
	// description, or a comparison against a harness literal that also forgot
	// one would pass for the wrong reason. engine.schemaOf panics at package
	// init on a tool or argument with no description, so this is a sanity
	// check on the walk above rather than on schemaOf itself.
	for _, s := range schemas {
		if s.Description == "" {
			t.Fatalf("positive control failed: engine.Catalogue's schema for %q carries no description", s.Name)
		}
	}

	want := provider.RenderTools(schemas)
	if diff := cmp.Diff(want, cfg.ToolCatalogue); diff != "" {
		t.Errorf("harness.Config.ToolCatalogue disagrees with the engine's own rendered catalogue "+
			"(-engine +harness):\n%s\n"+
			"ToolCatalogue is a literal copy of what internal/engine/catalogue.go's argument structs "+
			"declare (see ToolCatalogue's own doc comment in internal/harness/harness.go); a name, "+
			"type, required flag or description changed on one side and not the other", diff)
	}
}
