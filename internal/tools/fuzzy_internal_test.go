package tools

import (
	"math"
	"strings"
	"testing"
)

// normalizeLinesJoined is what the matcher compares: the normalised lines, in
// the one string [rankRegions] scores. Two blocks are the same text to this
// tool exactly when this returns the same value for both.
func normalizeLinesJoined(lines []string) string {
	return strings.Join(normalizeLines(lines), "\n")
}

// The similarity metric's properties, tested directly rather than only through
// the edit path. SLICE-1's unit test plan names three — symmetric,
// whitespace-normalising, monotonic in edit distance — and a property asserted
// only through a tool is a property nobody can see fail on its own.

func TestSimilarityIsSymmetric(t *testing.T) {
	pairs := [][2]string{
		{"", ""},
		{"", "a"},
		{"abc", "abc"},
		{"abc", "abd"},
		{"return a + 1", "return b + 1"},
		{"func alpha() {\nreturn 1\n}", "func beta() {\nreturn 2\n}"},
		{"short", "a considerably longer string than the other one"},
		{"naïve", "naive"},
	}
	for _, p := range pairs {
		ab, ba := similarity(p[0], p[1]), similarity(p[1], p[0])
		if ab != ba {
			t.Errorf("similarity(%q, %q) = %v but the other way round it is %v", p[0], p[1], ab, ba)
		}
		if ab < 0 || ab > 1 {
			t.Errorf("similarity(%q, %q) = %v, outside [0, 1]", p[0], p[1], ab)
		}
	}
}

// TestSimilarityIsMonotonicInEditDistance is the property the floor's meaning
// rests on. If a further-away string could score higher, "reached the floor"
// would not mean "close", and every number in the near-miss report would be
// noise.
func TestSimilarityIsMonotonicInEditDistance(t *testing.T) {
	base := "func alpha(a int) int { return a + 1 }"
	if strings.ContainsRune(base, 'X') {
		t.Fatal("the substitution character already appears in the base, so each step " +
			"would not add exactly one edit")
	}

	// Step k is the base with its first k characters substituted, so the length
	// is fixed and the edit distance is exactly k.
	prev := math.Inf(1)
	for k := range len(base) {
		s := strings.Repeat("X", k) + base[k:]
		got := similarity(base, s)
		if got >= prev {
			t.Errorf("%d edits (%q) scored %v, not below the previous %v; similarity must "+
				"fall as edit distance rises", k, s, got, prev)
		}
		prev = got
	}

	// And the same distance over a longer string scores higher, which is what
	// normalising by length is for: one wrong character in forty is closer
	// than one wrong character in four.
	short, long := similarity("abcd", "abcX"), similarity(strings.Repeat("ab", 20), strings.Repeat("ab", 19)+"aX")
	if long <= short {
		t.Errorf("one edit in %d scored %v, not above one edit in 4 (%v)", 40, long, short)
	}
}

func TestSimilarityIsOneOnlyForIdenticalNormalisedText(t *testing.T) {
	if got := similarity("a", "a"); got != 1 {
		t.Errorf("identical text scored %v", got)
	}
	if got := similarity("", ""); got != 1 {
		t.Errorf("two empty blocks scored %v", got)
	}
	if got := similarity("abc", "abd"); got == 1 {
		t.Error("different text scored 1")
	}
	if got := similarity("", "abcd"); got != 0 {
		t.Errorf("nothing against something scored %v, want 0", got)
	}
}

// TestNormalisationCollapsesExactlyWhatItClaims is the statement of what the
// fallback treats as the same text — in both directions, because normalising
// is what makes the fallback useful and it is also what makes two distinct
// regions collide.
func TestNormalisationCollapsesExactlyWhatItClaims(t *testing.T) {
	same := []struct {
		name string
		a, b []string
	}{
		{"tabs against spaces", []string{"\tif x {"}, []string{"    if x {"}},
		{"any depth of indentation", []string{"\t\t\treturn 1"}, []string{"return 1"}},
		{"trailing whitespace", []string{"return 1   "}, []string{"return 1"}},
		{"trailing tabs", []string{"return 1\t"}, []string{"return 1"}},
		{"a run of inner spaces", []string{"a  +   b"}, []string{"a + b"}},
		{"an inner tab against a space", []string{"a\t+\tb"}, []string{"a + b"}},
		{"a carriage return left on a line", []string{"return 1\r"}, []string{"return 1"}},
		{"a non-breaking space", []string{"a b"}, []string{"a b"}},
		{"a blank line against a whitespace-only one", []string{"a", "   \t", "b"}, []string{"a", "", "b"}},
	}
	for _, tc := range same {
		t.Run("collapses "+tc.name, func(t *testing.T) {
			if got, want := normalizeLinesJoined(tc.a), normalizeLinesJoined(tc.b); got != want {
				t.Errorf("normalised to %q and %q, which do not match", got, want)
			}
		})
	}

	different := []struct {
		name string
		a, b []string
	}{
		{"whitespace against none", []string{"foo(a, b)"}, []string{"foo(a,b)"}},
		{"a space between identifiers", []string{"a b"}, []string{"ab"}},
		{"a blank line present or absent", []string{"a", "", "b"}, []string{"a", "b"}},
		{"letter case", []string{"Alpha"}, []string{"alpha"}},
		{"one renamed identifier", []string{"return alpha + 1"}, []string{"return beta + 1"}},
		{"line order", []string{"a", "b"}, []string{"b", "a"}},
	}
	for _, tc := range different {
		t.Run("keeps "+tc.name+" apart", func(t *testing.T) {
			if got, want := normalizeLinesJoined(tc.a), normalizeLinesJoined(tc.b); got == want {
				t.Errorf("both normalised to %q; the fallback would treat two different "+
					"regions as one", got)
			}
		})
	}
}

// TestRegionScoreIsNoBetterThanItsWorstPart pins the boundary terms. They exist
// because a block's score is dominated by its interior, so a region offset by
// one line still scores extremely well — and without them nearly every
// multi-line match would arrive with a shifted neighbour above the floor and be
// refused as ambiguous, which is a refusal the model cannot act on.
func TestRegionScoreIsNoBetterThanItsWorstPart(t *testing.T) {
	want := []string{"func alpha(count int) int {", "total := count * 2", "return total", "}"}
	joined := strings.Join(want, "\n")

	score := func(got []string) float64 {
		return regionScore(want, joined, normalizeLines(got), normalizeLinesJoined(got))
	}

	if got := score([]string{"func alpha(count int) int {", "total := count * 2", "return total", "}"}); got != 1 {
		t.Errorf("the same region scored %v, want 1", got)
	}

	// The region shifted up by one line: three of its four lines are right, and
	// the whole-block score alone would put it far above any usable floor.
	shifted := []string{"", "func alpha(count int) int {", "total := count * 2", "return total"}
	whole := similarity(joined, normalizeLinesJoined(shifted))
	if whole < DefaultFuzzyFloor {
		t.Fatalf("the shifted region scores %v on the block alone, which is already below "+
			"the floor — this fixture does not exercise the boundary terms", whole)
	}
	if got := score(shifted); got >= DefaultFuzzyFloor {
		t.Errorf("the shifted region scored %v, at or above the floor; a region that does "+
			"not begin where the model said must not be a candidate", got)
	}

	// A wrong closing line is caught by the same rule.
	badEnd := []string{"func alpha(count int) int {", "total := count * 2", "return total", "	panic(\"no\")"}
	if got := score(badEnd); got >= DefaultFuzzyFloor {
		t.Errorf("a region ending somewhere else scored %v", got)
	}

	// And it never *raises* a score: the minimum of three terms is at most the
	// block term, so the boundary rule can only ever refuse more.
	for _, got := range [][]string{shifted, badEnd, want} {
		block := similarity(joined, normalizeLinesJoined(got))
		if score(got) > block {
			t.Errorf("regionScore %v exceeds the block score %v, so the boundary terms are "+
				"admitting regions rather than only excluding them", score(got), block)
		}
	}
}

// TestLengthBoundNeverExceedsTheRealScore is the correctness condition for the
// scan's only optimisation. The bound is used to skip a comparison entirely, so
// a bound that could sit *below* a real score would silently drop a match or a
// near miss — the scan would stop being exact and nothing would say so.
func TestLengthBoundNeverExceedsTheRealScore(t *testing.T) {
	blocks := []string{
		"", "a", "ab", "abc", "return a + 1", "return alpha + 1",
		"func alpha(a int) int {\nreturn a + 1\n}",
		"func beta(b int) int {\nreturn b + 2\n}",
		strings.Repeat("x", 64), strings.Repeat("xy", 40),
	}
	for _, a := range blocks {
		for _, b := range blocks {
			bound := lengthBound(len([]rune(a)), len([]rune(b)))
			got := similarity(a, b)
			if got > bound+1e-12 {
				t.Errorf("similarity(%q, %q) = %v exceeds its length bound %v; the scan "+
					"would skip a region that should have been compared", a, b, got, bound)
			}
		}
	}
}

// TestFuzzyFloorRefusesToMeanMatchAnything: the zero value of Limits is the
// realistic way this goes wrong, and it must not be the most dangerous
// configuration available.
func TestFuzzyFloorRefusesToMeanMatchAnything(t *testing.T) {
	cases := map[float64]float64{
		0:    DefaultFuzzyFloor,
		-1:   DefaultFuzzyFloor,
		1.5:  DefaultFuzzyFloor,
		0.5:  0.5,
		1:    1,
		0.99: 0.99,
	}
	for given, want := range cases {
		s := &Set{Limits: Limits{FuzzyFloor: given}}
		if got := s.fuzzyFloor(); got != want {
			t.Errorf("FuzzyFloor %v resolved to %v, want %v", given, got, want)
		}
	}
	if DefaultLimits().FuzzyFloor != DefaultFuzzyFloor {
		t.Errorf("DefaultLimits().FuzzyFloor = %v, want %v",
			DefaultLimits().FuzzyFloor, DefaultFuzzyFloor)
	}
}

// TestKeepNearestOrdersByScoreThenByLine holds the near-miss report's shape:
// closest first, at most three, and a tie broken by the earlier region so the
// report reads down the file. The tie-break is presentational and applies only
// to *reporting* — nothing in this package breaks a tie to choose where an edit
// lands.
func TestKeepNearestOrdersByScoreThenByLine(t *testing.T) {
	var near []FuzzyMatch
	for i, score := range []float64{0.1, 0.9, 0.5, 0.9, 0.95, 0.2} {
		near = keepNearest(near, FuzzyMatch{StartLine: i + 1, Similarity: score})
	}
	if len(near) != NearMissCount {
		t.Fatalf("kept %d near misses, want %d", len(near), NearMissCount)
	}
	want := []struct {
		line  int
		score float64
	}{{5, 0.95}, {2, 0.9}, {4, 0.9}}
	if len(want) != NearMissCount {
		t.Fatalf("NearMissCount is %d but this table expects %d; SLICE-1 §4 fixes the "+
			"near-miss count at 3 as a model-facing contract", NearMissCount, len(want))
	}
	for i, w := range want {
		if near[i].StartLine != w.line || near[i].Similarity != w.score {
			t.Errorf("near[%d] = line %d score %v, want line %d score %v",
				i, near[i].StartLine, near[i].Similarity, w.line, w.score)
		}
	}
}
