package harness_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// TestDefaultModelResolves is the check that keeps the binary startable.
//
// An unrecognised model id is a usage error (ADR-0007 decision 4), so a typo in
// the built-in default would make kopicode refuse to start out of the box with
// no flag and no config file — the one failure this whole mechanism must not
// have. docs/provider-pin.md verified the slug against OpenRouter's catalogue on
// 2026-08-14; this verifies the binary agrees with itself.
func TestDefaultModelResolves(t *testing.T) {
	sel, err := harness.Resolve(t.TempDir(), harness.Overrides{})
	if err != nil {
		t.Fatalf("resolving with no flag and no config file failed: %v\n"+
			"the built-in default (%s) must be a registered model, or the binary cannot start "+
			"without arguments", err, harness.DefaultModelID)
	}
	if sel.ModelID != harness.DefaultModelID {
		t.Errorf("default resolution chose %q, want %q", sel.ModelID, harness.DefaultModelID)
	}
	if sel.Config.Name != harness.DefaultConfigName {
		t.Errorf("default resolution chose harness %q, want %q", sel.Config.Name, harness.DefaultConfigName)
	}
	if sel.HarnessConfigHash == "" {
		t.Error("the resolved selection carries no harness config hash, so SessionStarted would " +
			"record an empty comparison key")
	}
}

// TestAddedModelsResolve is KAN-850's "a test that its pin resolves" for each
// row registered alongside qwen/qwen3-coder-next: minimax/minimax-m2 and
// z-ai/glm-5.2, both argued from live OpenRouter endpoint data on 2026-08-18
// (docs/provider-pin.md). It does not touch qwen/qwen3-coder-next — that row
// already has TestDefaultModelResolves above — and it does not touch the
// README's "frontier ceiling" role, which is deliberately not a registry row.
//
// Each case resolves with --model set (the SourceFlag path, since neither
// model is the built-in default), and checks the same three things
// TestDefaultModelResolves checks for the default: the model, harness and pin
// resolutions do not silently disagree with what the registry actually holds.
func TestAddedModelsResolve(t *testing.T) {
	cases := []struct {
		name          string
		modelID       string
		wantOrder     []string
		wantQuant     []string
		wantValidated string
	}{
		{
			name:          "minimax/minimax-m2",
			modelID:       "minimax/minimax-m2",
			wantOrder:     []string{"minimax/fp8"},
			wantQuant:     []string{"fp8"},
			wantValidated: "2026-08-18",
		},
		{
			name:          "z-ai/glm-5.2",
			modelID:       "z-ai/glm-5.2",
			wantOrder:     []string{"sail-research/fp8"},
			wantQuant:     []string{"fp8"},
			wantValidated: "2026-08-18",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !slices.Contains(harness.SupportedModels(), tc.modelID) {
				t.Fatalf("SupportedModels does not list %q; it must be a registry row for this test "+
					"to mean anything", tc.modelID)
			}

			sel, err := harness.Resolve(t.TempDir(), harness.Overrides{Model: tc.modelID})
			if err != nil {
				t.Fatalf("resolving --model %s failed: %v\n"+
					"a registered model must resolve, or the row is unreachable from the front ends",
					tc.modelID, err)
			}
			if sel.ModelID != tc.modelID {
				t.Errorf("resolution chose %q, want %q", sel.ModelID, tc.modelID)
			}
			if sel.ModelSource != harness.SourceFlag {
				t.Errorf("model source = %q, want %q (resolved via --model)", sel.ModelSource, harness.SourceFlag)
			}
			if _, ok := harness.ConfigByName(sel.Config.Name); !ok {
				t.Errorf("resolution named harness configuration %q, which does not exist", sel.Config.Name)
			}
			if sel.HarnessConfigHash == "" {
				t.Error("the resolved selection carries no harness config hash, so SessionStarted would " +
					"record an empty comparison key")
			}
			if !slices.Equal(sel.Pin.Order, tc.wantOrder) {
				t.Errorf("resolved pin.order = %v, want %v", sel.Pin.Order, tc.wantOrder)
			}
			if sel.Pin.AllowFallbacks {
				t.Errorf("resolved pin allows fallbacks; every benchmark request pins exactly one provider " +
					"(docs/adr/0005-benchmark-and-ab-methodology.md §2)")
			}
			if !slices.Equal(sel.Pin.Quantizations, tc.wantQuant) {
				t.Errorf("resolved pin.quantizations = %v, want %v", sel.Pin.Quantizations, tc.wantQuant)
			}

			entry, ok := harness.Lookup(tc.modelID)
			if !ok {
				t.Fatalf("Lookup(%q) failed after Resolve succeeded for it", tc.modelID)
			}
			if entry.Validated != tc.wantValidated {
				t.Errorf("registry row for %q is dated %q, want %q (docs/provider-pin.md)",
					tc.modelID, entry.Validated, tc.wantValidated)
			}
		})
	}
}

// TestEveryRegistryRowIsUsable holds the registry to itself rather than to a
// copy of it written out here.
//
// Every row is walked, so an eleventh model cannot land unchecked, and each is
// held to the three things a row has to be true about: the harness
// configuration it names exists, the pin is a pin (ADR-0005 §2 — one slug, no
// fallbacks, a fixed quantization), and the row says when a person last looked
// at it (ADR-0007 §Consequences).
func TestEveryRegistryRowIsUsable(t *testing.T) {
	models := harness.SupportedModels()
	if len(models) == 0 {
		t.Fatal("the registry is empty: this binary supports no model at all")
	}

	for _, id := range models {
		t.Run(id, func(t *testing.T) {
			entry, ok := harness.Lookup(id)
			if !ok {
				t.Fatalf("SupportedModels lists %q but Lookup does not find it", id)
			}
			if _, ok := harness.ConfigByName(entry.HarnessConfig); !ok {
				t.Errorf("row %q names harness configuration %q, which this binary does not have",
					id, entry.HarnessConfig)
			}
			if len(entry.Pin.Order) != 1 {
				t.Errorf("row %q pins provider.order to %v; a benchmark request pins exactly one slug "+
					"(docs/adr/0005-benchmark-and-ab-methodology.md §2)", id, entry.Pin.Order)
			}
			if entry.Pin.AllowFallbacks {
				t.Errorf("row %q allows fallbacks, so a result could be served by a provider nobody "+
					"declared and the pin would be decorative", id)
			}
			if len(entry.Pin.Quantizations) == 0 {
				t.Errorf("row %q fixes no quantization: two runs of the same model can be different "+
					"weights, which is why ADR-0005 exists", id)
			}
			if entry.Validated == "" {
				t.Errorf("row %q does not say when the mapping was last validated "+
					"(ADR-0007 §Consequences)", id)
			}
		})
	}
}

// TestRegistryPinMatchesShippedFixtures ties the registry's pin to the one the
// recorded traffic declares.
//
// This is the reason the pin belongs in the registry at all. Before this row
// existed the pin lived in three fixture files and a markdown document, with
// nothing in code to disagree with; ADR-0007 decision 5 puts the default pin on
// the registry row, so there are now two copies and they must not drift. The
// consequence is not cosmetic: the replay provider refuses traffic recorded
// under another arm's pin, so a registry that disagreed with the fixtures would
// fail every mock-driven engine test — much later, and looking like a mock bug.
func TestRegistryPinMatchesShippedFixtures(t *testing.T) {
	fixtures, err := fixture.LoadAll(fixture.FS())
	if err != nil {
		t.Fatalf("loading shipped fixtures: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("positive control failed: no fixtures were loaded, so this test compares nothing")
	}

	for _, f := range fixtures {
		entry, ok := harness.Lookup(f.ModelID)
		if !ok {
			t.Errorf("fixture %s was taken under model %q, which the registry does not know; "+
				"the mock provider would serve traffic for an arm this binary cannot select",
				f.Name, f.ModelID)
			continue
		}
		if !slices.Equal(entry.Pin.Order, f.Pin.Order) ||
			entry.Pin.AllowFallbacks != f.Pin.AllowFallbacks ||
			!slices.Equal(entry.Pin.Quantizations, f.Pin.Quantizations) {
			t.Errorf("fixture %s declares pin %v/%t/%v but the registry row for %s carries %v/%t/%v\n"+
				"docs/provider-pin.md is where a changed pin is argued and dated; both copies move together",
				f.Name, f.Pin.Order, f.Pin.AllowFallbacks, f.Pin.Quantizations,
				f.ModelID, entry.Pin.Order, entry.Pin.AllowFallbacks, entry.Pin.Quantizations)
		}
	}
}

// TestToolSetMatchesInternalTools derives the expected tool set from
// internal/tools' source rather than restating it.
//
// The harness configuration lists the tools as presented to the model, and that
// list is in the hash preimage. A tool added to internal/tools and not to the
// configuration would be offered to the model — or not offered — with the arm's
// identity unchanged, which is the preimage lying about what the harness is. A
// hand-written expectation here would have to be updated by whoever forgot, at
// the moment they forgot, so the set is read out of the source instead.
//
// It checks every registered configuration (harness.ConfigNames()), not only
// the default: [MinimaxM2ConfigName] happens to carry the same ToolSet today
// (KAN-942 varies only the sampling seed policy), but nothing enforces that
// staying true, and a configuration with its own tool set deserves this
// guard from the day it is registered rather than from whenever someone
// notices the gap.
//
// [NaiveToolsetConfigName] is the one exception, and it is named rather than
// silently allowed to differ: it deliberately drops "edit_file" from ToolSet
// (KAN-1020), so its own set can never equal internal/tools' full declared
// set by design, not by drift. What still holds for it is that every name it
// does present is a real, declared tool — a typo or a stale name would still
// fail here — and TestNaiveToolsetConfigOnlyDropsEditFile
// (hash_internal_test.go) is what proves the one omission is exactly and only
// edit_file, transitively anchored to this same declared set through the
// `default` subtest below.
func TestToolSetMatchesInternalTools(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	path := filepath.Join(root, "internal", "tools", "tools.go")

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	var declared []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			if !strings.HasPrefix(vs.Names[0].Name, "Tool") {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("unquoting %s: %v", lit.Value, err)
			}
			declared = append(declared, value)
		}
	}

	// Positive control: a walk that found nothing would let any configuration
	// pass, including an empty one.
	if len(declared) == 0 {
		t.Fatalf("positive control failed: no Tool* string constants were found in %s, so this "+
			"test would accept any tool set at all", path)
	}

	sort.Strings(declared)
	declaredSet := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredSet[name] = true
	}

	for _, cfg := range everyConfig(t) {
		t.Run(cfg.Name, func(t *testing.T) {
			got := append([]string(nil), cfg.ToolSet...)
			sort.Strings(got)

			if cfg.Name == harness.NaiveToolsetConfigName {
				// See this test's own doc comment: this configuration is the
				// one deliberate exception, so it is checked as a subset
				// rather than by equality.
				for _, name := range got {
					if !declaredSet[name] {
						t.Errorf("harness configuration %q presents %q, which internal/tools does not "+
							"declare — a typo or a stale name, not the deliberate edit_file omission this "+
							"configuration is allowed", cfg.Name, name)
					}
				}
				return
			}

			if !slices.Equal(got, declared) {
				t.Errorf("harness configuration %q presents %v; internal/tools declares %v\n"+
					"the tool set is in the harness config hash preimage "+
					"(docs/adr/0007-model-selection-and-harness-config-shape.md decision 6), so a tool "+
					"added on one side and not the other changes what the model is offered without "+
					"changing the arm's identity", cfg.Name, got, declared)
			}
		})
	}
}

// TestNearMatchSuggestionsStayHonest.
//
// A confident wrong suggestion is worse than none: "did you mean
// qwen/qwen3-coder-next?" in answer to a request for an unrelated model reads
// as though the harness knows something it does not, while the supported
// list — always printed — already says everything true. Three registered
// candidates (KAN-850 added two) means three chances at a wrong guess rather
// than one, so this still has to hold per candidate, not just for the first.
func TestNearMatchSuggestionsStayHonest(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		wantSuggest bool
	}{
		{"one letter dropped", "qwen/qwen3-coder-nxt", true},
		{"one letter wrong", "qwen/qwen3-coder-nezt", true},
		{"a different model entirely", "openai/gpt-5", false},
		{"a real neighbour that is not the pinned one", "qwen/qwen3-coder-flash", false},
		{"one digit off a second registered row", "z-ai/glm-5.1", true},
		{"a real neighbour of a second registered row", "z-ai/glm-4.6v", false},
		{"one letter off a third registered row", "minimax/minimax-m3", true},
		{"a real neighbour of a third registered row", "minimax/minimax-m2-her", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harness.Resolve(t.TempDir(), harness.Overrides{Model: tc.model})
			if err == nil {
				t.Fatalf("resolving model %q succeeded; it is not registered", tc.model)
			}
			suggested := strings.Contains(err.Error(), "did you mean")
			if suggested != tc.wantSuggest {
				t.Errorf("suggestion offered = %t, want %t, for %q\nmessage was:\n%s",
					suggested, tc.wantSuggest, tc.model, err)
			}
			for _, id := range harness.SupportedModels() {
				if !strings.Contains(err.Error(), id) {
					t.Errorf("the error does not list the supported model %q:\n%s", id, err)
				}
			}
		})
	}
}
