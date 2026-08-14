package harness

import (
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
