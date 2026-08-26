package harness

import (
	"reflect"
	"slices"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// TestHashIncludesAnchorVersion holds ADR-0006 §7's coupling, restated by
// ADR-0007 decision 6: an anchor-format bump is an experiment-series boundary,
// and it is enforced mechanically because every arm's hash moves with it.
//
// It is an internal test because anchor.Version is a constant and the only way
// to drive the coupling is to reach the unexported hashWith. A coupling nobody
// can drive is one somebody tidying the preimage deletes without noticing — the
// ADR says so in as many words, which is why this exists.
func TestHashIncludesAnchorVersion(t *testing.T) {
	cfg, ok := ConfigByName(DefaultConfigName)
	if !ok {
		t.Fatalf("no %q configuration", DefaultConfigName)
	}

	// Positive control: the parameter is actually the one Hash uses, so a test
	// that varies it is varying the real preimage rather than a parallel one.
	if got := cfg.hashWith(anchor.Version); got != cfg.Hash() {
		t.Fatalf("hashWith(anchor.Version) = %s but Hash() = %s — this test is exercising a "+
			"different preimage from the shipped one", got, cfg.Hash())
	}

	if cfg.hashWith(anchor.Version+"-next") == cfg.Hash() {
		t.Errorf("bumping the anchor version left the harness config hash unchanged\n" +
			"docs/adr/0006-hash-anchored-edits-and-failure-attribution.md §7 requires the anchor\n" +
			"version inside this hash, so that an anchor-format bump invalidates pooling on its\n" +
			"own rather than by anyone remembering to")
	}
}

// TestPreimageIsUnambiguous checks the length prefixing does its job.
//
// Two configurations whose field values concatenate to the same bytes must not
// collide. Without length prefixes, a name of "a" with a route of "bc" and a
// name of "ab" with a route of "c" would hash identically, and two arms sharing
// a hash is the exact failure the hash exists to prevent.
func TestPreimageIsUnambiguous(t *testing.T) {
	a := Config{Name: "a", ParseRoutes: []string{"bc"}}
	b := Config{Name: "ab", ParseRoutes: []string{"c"}}

	if a.Hash() == b.Hash() {
		t.Errorf("%+v and %+v hash to the same value (%s) — the preimage is being concatenated "+
			"without length prefixes, so field boundaries can be forged by content", a, b, a.Hash())
	}
}

// TestMinimaxM2ConfigOnlyChangesSeedPolicy holds MinimaxM2ConfigName's own
// doc comment in harness.go to the code it describes: the configuration is
// the default's value in every field except the sampling seed policy (and
// the seed value that policy makes meaningless), and the name that
// identifies it.
//
// It is an internal test because minimaxM2Config is unexported — the whole
// point of building the second configuration by copying the first (see
// defaultHarnessConfig's own doc comment) is that the diff is checkable
// structurally rather than by eye across two long literals, and this is the
// check.
func TestMinimaxM2ConfigOnlyChangesSeedPolicy(t *testing.T) {
	base := defaultHarnessConfig
	got := minimaxM2Config(base)

	if got.Name != MinimaxM2ConfigName {
		t.Errorf("Name = %q, want %q", got.Name, MinimaxM2ConfigName)
	}
	if got.Sampling.SeedPolicy != SeedUnseeded {
		t.Errorf("Sampling.SeedPolicy = %q, want %q — the argument for this configuration "+
			"(docs/provider-pin.md §minimax/minimax-m2) is that the pinned endpoint does not "+
			"support seed at all", got.Sampling.SeedPolicy, SeedUnseeded)
	}
	if got.Sampling.Seed != 0 {
		t.Errorf("Sampling.Seed = %d, want 0: meaningless once SeedPolicy is SeedUnseeded, and a "+
			"nonzero value sitting here unused would read as a seed nobody meant to send", got.Sampling.Seed)
	}

	// Name and Version are expected to differ — a configuration is
	// identified by its name (Config.Name's own doc comment) — so normalise
	// those and the two seed fields already checked above, then require
	// everything else to compare equal wholesale. That is what proves
	// nothing else moved, without hand-listing every remaining field of
	// Config the way the doc comment's claim would otherwise rest on
	// nothing but review.
	normalised := got
	normalised.Name = base.Name
	normalised.Version = base.Version
	normalised.Sampling.SeedPolicy = base.Sampling.SeedPolicy
	normalised.Sampling.Seed = base.Sampling.Seed

	if !reflect.DeepEqual(base, normalised) {
		t.Errorf("minimaxM2Config(base) differs from base in more than Name, Version and the seed "+
			"fields; MinimaxM2ConfigName's doc comment in harness.go claims exactly one axis changed\n"+
			"base: %+v\ngot:  %+v", base, normalised)
	}
}

// TestNaiveV1ConfigOnlyChangesItsClaimedFields holds NaiveV1ConfigName's own
// doc comment in harness.go to the code it describes: the configuration is
// the default's value in every field except SystemPrompt, ParseRoutes,
// RepairBudget and Verification.Forced (KAN-1012's design record), and the
// name and version that identify it.
//
// It is an internal test for the same reason
// TestMinimaxM2ConfigOnlyChangesSeedPolicy is: naiveV1Config is unexported,
// and the whole point of building the second configuration by copying the
// first is that the diff is checkable structurally rather than by eye across
// two long literals.
func TestNaiveV1ConfigOnlyChangesItsClaimedFields(t *testing.T) {
	base := defaultHarnessConfig
	got := naiveV1Config(base)

	if got.Name != NaiveV1ConfigName {
		t.Errorf("Name = %q, want %q", got.Name, NaiveV1ConfigName)
	}
	if got.SystemPrompt != NaiveSystemPrompt {
		t.Error("SystemPrompt is not NaiveSystemPrompt — the whole argument for a separate prompt " +
			"(NaiveSystemPrompt's own doc comment) is that it honestly reflects ParseRoutes and " +
			"Verification.Forced changing, not DefaultSystemPrompt reused")
	}
	if got.SystemPrompt == "" {
		t.Fatal("positive control failed: naiveV1Config carries no system prompt at all")
	}
	if !reflect.DeepEqual(got.ParseRoutes, []string{"native"}) {
		t.Errorf("ParseRoutes = %v, want [native] — KAN-1012's argument is that a naive harness "+
			"tolerates no fallback extraction route", got.ParseRoutes)
	}
	if got.RepairBudget != 0 {
		t.Errorf("RepairBudget = %d, want 0 — a naive harness gives a malformed tool call no repair "+
			"round trips; internal/engine treats 0 as the legal floor, not an edge case", got.RepairBudget)
	}
	if got.Verification.Forced {
		t.Error("Verification.Forced = true, want false — this is the headline lever KAN-1012 chose: " +
			"a naive harness never runs a forced check-and-correct loop at all")
	}

	// Name, Version and the four claimed fields are expected to differ.
	// Normalise those, then require everything else to compare equal
	// wholesale — the same technique
	// TestMinimaxM2ConfigOnlyChangesSeedPolicy uses, and for the same reason:
	// it proves nothing else moved without hand-listing every remaining
	// field of Config.
	normalised := got
	normalised.Name = base.Name
	normalised.Version = base.Version
	normalised.SystemPrompt = base.SystemPrompt
	normalised.ParseRoutes = base.ParseRoutes
	normalised.RepairBudget = base.RepairBudget
	normalised.Verification.Forced = base.Verification.Forced

	if !reflect.DeepEqual(base, normalised) {
		t.Errorf("naiveV1Config(base) differs from base in more than Name, Version, SystemPrompt, "+
			"ParseRoutes, RepairBudget and Verification.Forced; NaiveV1ConfigName's doc comment in "+
			"harness.go claims exactly those axes changed\nbase: %+v\ngot:  %+v", base, normalised)
	}
}

// TestNaiveV2ConfigOnlyChangesVerificationForced holds NaiveV2ConfigName's own
// doc comment in harness.go to the code it describes: unlike naiveV1Config's
// three claimed fields, this configuration differs from the default in
// exactly one independent field, Verification.Forced, plus the prompt that is
// its required consequence — RepairBudget and ParseRoutes must stay at their
// default values, which is the entire point of a single-field probe.
//
// Same technique and reason as TestMinimaxM2ConfigOnlyChangesSeedPolicy and
// TestNaiveV1ConfigOnlyChangesItsClaimedFields: naiveV2Config is unexported,
// and the diff is checked structurally rather than by eye.
func TestNaiveV2ConfigOnlyChangesVerificationForced(t *testing.T) {
	base := defaultHarnessConfig
	got := naiveV2Config(base)

	if got.Name != NaiveV2ConfigName {
		t.Errorf("Name = %q, want %q", got.Name, NaiveV2ConfigName)
	}
	if got.SystemPrompt != NaiveVerifyOnlySystemPrompt {
		t.Error("SystemPrompt is not NaiveVerifyOnlySystemPrompt — the whole argument for a separate " +
			"prompt is that it honestly reflects Verification.Forced changing and nothing else")
	}
	if got.SystemPrompt == "" {
		t.Fatal("positive control failed: naiveV2Config carries no system prompt at all")
	}
	if got.Verification.Forced {
		t.Error("Verification.Forced = true, want false — this is the one field this configuration " +
			"exists to isolate")
	}
	if !reflect.DeepEqual(got.ParseRoutes, base.ParseRoutes) {
		t.Errorf("ParseRoutes = %v, want the default's %v unchanged — a single-field probe that also "+
			"narrows parse routes is not isolating Verification.Forced any more than naive-v1 does",
			got.ParseRoutes, base.ParseRoutes)
	}
	if got.RepairBudget != base.RepairBudget {
		t.Errorf("RepairBudget = %d, want the default's %d unchanged — a single-field probe that also "+
			"zeroes the repair budget is not isolating Verification.Forced any more than naive-v1 does",
			got.RepairBudget, base.RepairBudget)
	}

	// Name, Version, SystemPrompt and Verification.Forced are expected to
	// differ. Normalise those, then require everything else to compare equal
	// wholesale — proves nothing else moved without hand-listing every
	// remaining field of Config.
	normalised := got
	normalised.Name = base.Name
	normalised.Version = base.Version
	normalised.SystemPrompt = base.SystemPrompt
	normalised.Verification.Forced = base.Verification.Forced

	if !reflect.DeepEqual(base, normalised) {
		t.Errorf("naiveV2Config(base) differs from base in more than Name, Version, SystemPrompt and "+
			"Verification.Forced; NaiveV2ConfigName's doc comment in harness.go claims exactly that one "+
			"independent axis changed\nbase: %+v\ngot:  %+v", base, normalised)
	}
}

// TestNaiveToolsetConfigOnlyDropsEditFile holds NaiveToolsetConfigName's own
// doc comment in harness.go to the code it describes: unlike naiveV1Config
// and naiveV2Config, which probe the verification/parse-repair and
// verification-alone axes, this configuration differs from the default in
// exactly one independent axis — ToolSet/ToolCatalogue losing "edit_file" —
// plus the prompt that is its required consequence. RepairBudget, ParseRoutes
// and Verification.Forced must stay at their default values, which is the
// entire point of a tool-set-only probe.
//
// Same technique and reason as the other two naiveVN diff tests:
// naiveToolsetConfig is unexported, and the diff is checked structurally
// rather than by eye.
func TestNaiveToolsetConfigOnlyDropsEditFile(t *testing.T) {
	base := defaultHarnessConfig
	got := naiveToolsetConfig(base)

	if got.Name != NaiveToolsetConfigName {
		t.Errorf("Name = %q, want %q", got.Name, NaiveToolsetConfigName)
	}
	if got.SystemPrompt != NaiveToolsetSystemPrompt {
		t.Error("SystemPrompt is not NaiveToolsetSystemPrompt — the whole argument for a separate " +
			"prompt is that it honestly reflects ToolSet losing edit_file and nothing else")
	}
	if got.SystemPrompt == "" {
		t.Fatal("positive control failed: naiveToolsetConfig carries no system prompt at all")
	}
	if slices.Contains(got.ToolSet, "edit_file") {
		t.Errorf("ToolSet = %v, still contains edit_file — this is the one axis this configuration "+
			"exists to isolate", got.ToolSet)
	}
	for _, name := range base.ToolSet {
		if name == "edit_file" {
			continue
		}
		if !slices.Contains(got.ToolSet, name) {
			t.Errorf("ToolSet dropped %q along with edit_file; only edit_file should be removed", name)
		}
	}
	for _, def := range got.ToolCatalogue {
		if def.Function.Name == "edit_file" {
			t.Error("ToolCatalogue still carries an edit_file entry; TestHarnessToolCatalogueMatchesEngineCatalogue " +
				"requires ToolCatalogue and ToolSet to name the same tools in the same order")
		}
	}
	if len(got.ToolCatalogue) != len(got.ToolSet) {
		t.Errorf("ToolCatalogue has %d entries but ToolSet lists %d tools; they must match",
			len(got.ToolCatalogue), len(got.ToolSet))
	}
	if !reflect.DeepEqual(got.ParseRoutes, base.ParseRoutes) {
		t.Errorf("ParseRoutes = %v, want the default's %v unchanged — a tool-set-only probe that also "+
			"narrows parse routes is not isolating the tool-set axis alone", got.ParseRoutes, base.ParseRoutes)
	}
	if got.RepairBudget != base.RepairBudget {
		t.Errorf("RepairBudget = %d, want the default's %d unchanged — a tool-set-only probe that also "+
			"zeroes the repair budget is not isolating the tool-set axis alone", got.RepairBudget, base.RepairBudget)
	}
	if got.Verification.Forced != base.Verification.Forced {
		t.Errorf("Verification.Forced = %v, want the default's %v unchanged — a tool-set-only probe that "+
			"also disables forced verification is not isolating the tool-set axis alone",
			got.Verification.Forced, base.Verification.Forced)
	}

	// Name, Version, SystemPrompt, ToolSet and ToolCatalogue are expected to
	// differ. Normalise those, then require everything else to compare equal
	// wholesale — proves nothing else moved without hand-listing every
	// remaining field of Config.
	normalised := got
	normalised.Name = base.Name
	normalised.Version = base.Version
	normalised.SystemPrompt = base.SystemPrompt
	normalised.ToolSet = base.ToolSet
	normalised.ToolCatalogue = base.ToolCatalogue

	if !reflect.DeepEqual(base, normalised) {
		t.Errorf("naiveToolsetConfig(base) differs from base in more than Name, Version, SystemPrompt, "+
			"ToolSet and ToolCatalogue; NaiveToolsetConfigName's doc comment in harness.go claims exactly "+
			"that one independent axis changed\nbase: %+v\ngot:  %+v", base, normalised)
	}
}
