package tools_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// applyFuzzy runs a fuzzy edit that is expected to succeed.
func applyFuzzy(t *testing.T, s *tools.Set, req tools.FuzzyEditRequest) tools.EditResult {
	t.Helper()
	res, err := s.EditFileFuzzy(context.Background(), req)
	if err != nil {
		t.Fatalf("EditFileFuzzy(%+v): %v", req, err)
	}
	if res.Rejection != nil {
		t.Fatalf("EditFileFuzzy(%+v): rejected: %s", req, res.Rejection.Summary)
	}
	return res
}

// rejectFuzzy runs a fuzzy edit that is expected to be refused, and holds the
// three halves of "fails closed" together: the refusal is structured, it is
// classified as the model's problem and not the harness's, and — the one that
// matters most on this path — **the file did not move a byte**. An approximate
// matcher that no-op'd and one that refused are indistinguishable from the
// error alone, and the first is the defect ADR-0006 exists to prevent.
func rejectFuzzy(t *testing.T, f *fixture, s *tools.Set, name string,
	req tools.FuzzyEditRequest,
) *tools.EditRejection {
	t.Helper()
	before := onDisk(t, f, name)

	res, err := s.EditFileFuzzy(context.Background(), req)
	if err == nil {
		t.Fatalf("EditFileFuzzy(%+v) was accepted; it must be refused", req)
	}
	if !errors.Is(err, tools.ErrEditRejected) {
		t.Fatalf("EditFileFuzzy(%+v): error %v does not wrap ErrEditRejected", req, err)
	}
	wantFault(t, err, tools.FaultTask)

	if res.Rejection == nil {
		t.Fatalf("EditFileFuzzy(%+v) refused with no structured rejection; the engine "+
			"cannot journal EditRejected without inventing one", req)
	}
	if res.Mode != tools.ModeFuzzy || res.Fuzzy == nil {
		t.Errorf("a refused fuzzy edit reported mode %q with Fuzzy=%v; SLICE-1 §9 marks the "+
			"session unattributed on any fuzzy use, refusals included",
			res.Mode, res.Fuzzy)
	}
	if after := onDisk(t, f, name); after != before {
		t.Fatalf("the file changed on a refused fuzzy edit:\nbefore %q\nafter  %q", before, after)
	}
	return res.Rejection
}

// --- the adversarial corpus -------------------------------------------------

// nearDuplicates is the corpus this card exists for: files where two or more
// regions are close enough that an approximate matcher would have to *choose*.
// Every case asserts a refusal and a byte-identical file, because a matcher
// that picks one of these and applies cleanly reports success, runs the session
// to a clean stop, and gets recorded as a `model` failure — a harness defect
// laundered into the number this project exists to measure (ADR-0006 §3).
//
// The floor does not decide any of these. Every one of them is above it, which
// is the point: the ambiguity refusal is the safety mechanism, not the floor.
var nearDuplicates = []struct {
	name string
	body string
	// before is the text the model claims is in the file.
	before string
	// wantMatches is how many regions the refusal must return. "Return both"
	// is literal, and so is "return all three".
	wantMatches int
	// wantLines are the 1-based first lines of those regions, ascending.
	wantLines []int
}{
	{
		name: "the same function body twice with one literal changed",
		body: `package main

func alpha(count int) int {
	total := count * 2
	total += 1
	return total
}

func beta(count int) int {
	total := count * 2
	total += 2
	return total
}
`,
		// The model's text matches alpha's body exactly. beta's body differs by
		// one character in forty — far above any usable floor — so both are
		// candidates and neither may be chosen.
		before:      "\ttotal := count * 2\n\ttotal += 1\n\treturn total\n",
		wantMatches: 2,
		wantLines:   []int{4, 10},
	},
	{
		name: "two regions differing only in indentation",
		body: "func a() {\n\tif ok {\n\t\treturn\n\t}\n}\n\nfunc b() {\n    if ok {\n        return\n    }\n}\n",
		// Normalisation is what makes these collide, and when it does the
		// answer is a refusal rather than a tiebreak.
		before:      "\tif ok {\n\t\treturn\n\t}\n",
		wantMatches: 2,
		wantLines:   []int{2, 8},
	},
	{
		name: "two identical error checks",
		body: `package main

func run() error {
	if err := first(); err != nil {
		return err
	}
	if err := second(); err != nil {
		return err
	}
	return nil
}
`,
		before:      "\t\treturn err\n\t}\n",
		wantMatches: 2,
		wantLines:   []int{5, 8},
	},
	{
		name: "a block appearing verbatim three times",
		body: "a\nlog(x)\nb\nc\nlog(x)\nd\ne\nlog(x)\nf\n",
		// The one-line region is the sharpest form of the problem: nothing
		// distinguishes the three, so nothing may choose between them.
		before:      "log(x)\n",
		wantMatches: 3,
		wantLines:   []int{2, 5, 8},
	},
	{
		name: "an exact match beside a paraphrase of itself",
		body: `package main

func handle(req Request) (Response, error) {
	if err := validate(req); err != nil {
		return Response{}, err
	}
	return process(req)
}

func handle2(req Request) (Response, error) {
	if err := validate(req); err != nil {
		return Response{}, err
	}
	return process(req)
}
`,
		before: "func handle(req Request) (Response, error) {\n\tif err := validate(req); err != nil {\n" +
			"\t\treturn Response{}, err\n\t}\n\treturn process(req)\n}\n",
		wantMatches: 2,
		wantLines:   []int{3, 10},
	},
	{
		name: "the whole file duplicated",
		body: "one\ntwo\nthree\none\ntwo\nthree\n",
		// Overlapping windows are still distinct regions: the naive matcher
		// that scanned for the first occurrence would take lines 1-3 without
		// ever noticing 4-6.
		before:      "one\ntwo\nthree\n",
		wantMatches: 2,
		wantLines:   []int{1, 4},
	},
}

// TestFuzzyRefusesEveryNearDuplicate is the deliverable of this card.
func TestFuzzyRefusesEveryNearDuplicate(t *testing.T) {
	for _, tc := range nearDuplicates {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"f.go": tc.body})
			s := f.set(t)

			rej := rejectFuzzy(t, f, s, "f.go", tools.FuzzyEditRequest{
				Path: "f.go", Before: tc.before, After: "// replaced\n",
			})

			if rej.Reason != tools.RejectAmbiguousAnchor {
				t.Fatalf("reason = %q, want %q — these regions are indistinguishable to the "+
					"matcher, so the only correct answer is to refuse", rej.Reason,
					tools.RejectAmbiguousAnchor)
			}
			if len(rej.Matches) != tc.wantMatches {
				t.Errorf("returned %d matches, want all %d; \"refuse as ambiguous and return "+
					"both\" is literal, and showing fewer makes the file look less repetitive "+
					"than it is: %+v", len(rej.Matches), tc.wantMatches, rej.Matches)
			}
			got := make([]int, len(rej.Matches))
			for i, m := range rej.Matches {
				got[i] = m.StartLine
			}
			if !equalInts(got, tc.wantLines) {
				t.Errorf("matched regions start at %v, want %v", got, tc.wantLines)
			}
			if !equalInts(rej.Candidates, tc.wantLines) {
				t.Errorf("candidates = %v, want %v", rej.Candidates, tc.wantLines)
			}

			// The model has to be able to act on this, so every region it must
			// choose between is named in the text it reads.
			for _, want := range tc.wantLines {
				if !strings.Contains(rej.Detail, strconv.Itoa(want)+"|") {
					t.Errorf("the detail does not quote the region at line %d:\n%s",
						want, rej.Detail)
				}
			}
		})
	}
}

// TestFuzzyNeverPicksTheBetterMatch is the tiebreak rule, stated as a test
// because every plausible tiebreak is wrong in the same way. The file holds one
// region matching the model's text exactly and one differing by a single
// character; preferring the higher score, the earlier region, or the one nearer
// anything would all resolve this — and each of them is a rule that, on the day
// it resolves it the other way, corrupts a file and reports success.
func TestFuzzyNeverPicksTheBetterMatch(t *testing.T) {
	body := "func alpha(n int) int {\n\treturn n * 2\n}\n\nfunc alpha2(n int) int {\n\treturn n * 2\n}\n"
	before := "func alpha(n int) int {\n\treturn n * 2\n}\n"

	f := newFixture(t, map[string]string{"f.go": body})
	s := f.set(t)

	rej := rejectFuzzy(t, f, s, "f.go", tools.FuzzyEditRequest{
		Path: "f.go", Before: before, After: "// gone\n",
	})
	if rej.Reason != tools.RejectAmbiguousAnchor {
		t.Fatalf("reason = %q, want %q", rej.Reason, tools.RejectAmbiguousAnchor)
	}
	if len(rej.Matches) != 2 {
		t.Fatalf("got %d matches, want 2", len(rej.Matches))
	}

	// One of them scores 1.0 and the other does not. That difference must not
	// have been used.
	exact := 0
	for _, m := range rej.Matches {
		if m.Similarity == 1 {
			exact++
		}
	}
	if exact != 1 {
		t.Fatalf("the fixture is not the case under test: %d of the matches score 1.0",
			exact)
	}
	if !strings.Contains(rej.Detail, "never") {
		t.Errorf("the refusal does not tell the model this tool will not choose:\n%s", rej.Detail)
	}
}

// --- normalisation cuts both ways -------------------------------------------

// TestNormalisationDoesNotMergeGenuinelyDifferentRegions is the other direction
// of the whitespace rule. Collapsing runs of whitespace must not collapse the
// difference between having whitespace and not having it, or two regions that a
// programmer would call different would become one apparent match — and this
// tool would apply an edit to whichever one it happened to see.
func TestNormalisationDoesNotMergeGenuinelyDifferentRegions(t *testing.T) {
	body := "package main\n\nvar tight = a+b+c+d\n\nvar spaced = a + b + c + d\n"
	f := newFixture(t, map[string]string{"f.go": body})
	s := f.set(t)

	res := applyFuzzy(t, s, tools.FuzzyEditRequest{
		Path: "f.go", Before: "var tight = a+b+c+d\n", After: "var tight = sum()\n",
	})
	if res.StartLine != 3 || res.EndLine != 3 {
		t.Errorf("applied at lines %d-%d, want line 3", res.StartLine, res.EndLine)
	}
	if res.Fuzzy.Similarity != 1 {
		t.Errorf("similarity = %v, want an exact normalised match", res.Fuzzy.Similarity)
	}
	want := "package main\n\nvar tight = sum()\n\nvar spaced = a + b + c + d\n"
	if got := onDisk(t, f, "f.go"); got != want {
		t.Errorf("file on disk =\n%q\nwant\n%q", got, want)
	}
}

// TestNormalisationTurningOneMatchIntoTwoIsRefused is the cost of normalising,
// stated rather than hidden. The same block indented with tabs in one place and
// spaces in another is one region to a programmer and two to the file; under
// normalisation they become indistinguishable, and the answer is to refuse.
// Applying to either would be a coin flip dressed as a match.
func TestNormalisationTurningOneMatchIntoTwoIsRefused(t *testing.T) {
	body := "func a() {\n\treturn compute(x, y)\n}\n\nfunc b() {\n        return compute(x,  y)\n}\n"
	f := newFixture(t, map[string]string{"f.go": body})
	s := f.set(t)

	// The needle is byte-identical to line 2 and differs from line 6 only in
	// whitespace, so without normalisation this would be a unique match.
	rej := rejectFuzzy(t, f, s, "f.go", tools.FuzzyEditRequest{
		Path: "f.go", Before: "\treturn compute(x, y)\n", After: "\treturn 0\n",
	})
	if rej.Reason != tools.RejectAmbiguousAnchor {
		t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectAmbiguousAnchor)
	}
	if len(rej.Matches) != 2 {
		t.Errorf("got %d matches, want both regions", len(rej.Matches))
	}
	for _, m := range rej.Matches {
		if m.Similarity != 1 {
			t.Errorf("region at line %d scored %v; normalisation should have made these "+
				"identical", m.StartLine, m.Similarity)
		}
	}
}

// --- below the floor --------------------------------------------------------

// TestFuzzyBelowFloorReturnsThreeNearMissesWithLineNumbers is the model-facing
// contract from SLICE-1 §4. The model uses this to retry against reality, so
// "three" and "with line numbers" are both load-bearing.
func TestFuzzyBelowFloorReturnsThreeNearMissesWithLineNumbers(t *testing.T) {
	body := `package main

func one() int { return 1 }
func two() int { return 2 }
func three() int { return 3 }
func four() int { return 4 }
func five() int { return 5 }
`
	f := newFixture(t, map[string]string{"f.go": body})
	s := f.set(t)

	rej := rejectFuzzy(t, f, s, "f.go", tools.FuzzyEditRequest{
		Path: "f.go", Before: "func ninetynine() int { return 99 }\n", After: "x\n",
	})
	if rej.Reason != tools.RejectBelowFloor {
		t.Fatalf("reason = %q, want %q", rej.Reason, tools.RejectBelowFloor)
	}
	if len(rej.Matches) != 3 {
		t.Fatalf("returned %d near misses, want exactly 3: %+v", len(rej.Matches), rej.Matches)
	}

	for i, m := range rej.Matches {
		if i > 0 && m.Similarity > rej.Matches[i-1].Similarity {
			t.Errorf("near miss %d scores %v, above the one before it (%v); the report is "+
				"closest-first", i, m.Similarity, rej.Matches[i-1].Similarity)
		}
		if m.Similarity >= s.Limits.FuzzyFloor {
			t.Errorf("near miss at line %d scored %v, at or above the floor %v — it would "+
				"have been a match", m.StartLine, m.Similarity, s.Limits.FuzzyFloor)
		}
		if m.StartLine < 1 || m.EndLine < m.StartLine {
			t.Errorf("near miss %d has a nonsensical region %d-%d", i, m.StartLine, m.EndLine)
		}
		if len(m.Lines) != m.EndLine-m.StartLine+1 {
			t.Errorf("near miss %d returned %d lines for region %d-%d",
				i, len(m.Lines), m.StartLine, m.EndLine)
		}
		// The line number is what makes this actionable: it is how the model
		// gets from "not found" to a read_file of the right place.
		if !strings.Contains(rej.Detail, strconv.Itoa(m.StartLine)+"|") {
			t.Errorf("the detail does not carry line %d:\n%s", m.StartLine, rej.Detail)
		}
		if !strings.Contains(rej.Detail, m.Lines[0]) {
			t.Errorf("the detail does not quote the near miss %q:\n%s", m.Lines[0], rej.Detail)
		}
	}

	// And the floor is stated, so the number in the report means something.
	if !strings.Contains(rej.Detail, "0.90") {
		t.Errorf("the refusal does not state the floor it applied:\n%s", rej.Detail)
	}
}

func TestFuzzyBelowFloorOnFilesWithNoRegionToCompare(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		before  string
		matches int
		says    string
	}{
		{
			name: "an empty file", body: "", before: "anything\n",
			matches: 0, says: "write_file",
		},
		{
			name: "text taller than the file", body: "one\ntwo\n",
			before: "one\ntwo\nthree\nfour\n", matches: 0, says: "no region",
		},
		{
			name: "a file with fewer near misses than three", body: "alpha\nbravo\n",
			before: "zulu\n", matches: 2, says: "closest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"f.txt": tc.body})
			s := f.set(t)

			rej := rejectFuzzy(t, f, s, "f.txt", tools.FuzzyEditRequest{
				Path: "f.txt", Before: tc.before, After: "x\n",
			})
			if rej.Reason != tools.RejectBelowFloor {
				t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectBelowFloor)
			}
			if len(rej.Matches) != tc.matches {
				t.Errorf("returned %d near misses, want %d", len(rej.Matches), tc.matches)
			}
			if !strings.Contains(rej.Detail, tc.says) {
				t.Errorf("the refusal does not say %q:\n%s", tc.says, rej.Detail)
			}
		})
	}
}

// --- the floor is a parameter -----------------------------------------------

// TestTheFloorIsAParameter: SLICE-1 treats the floor as tunable because
// measuring what the harness buys means varying it. A constant would make the
// fallback rate unmeasurable, and an A/B arm that varied it without recording
// it would not be evidence — so the value in force is on the result.
func TestTheFloorIsAParameter(t *testing.T) {
	body := "package main\n\nfunc alpha(a int) int {\n\treturn a + 1\n}\n"
	// A paraphrase rather than a copy: the region begins and ends where the
	// model says, and its middle is not what the model remembers. Well below
	// the default floor and comfortably above a lenient one, which is what
	// makes it show the parameter doing something.
	before := "func alpha(a int) int {\n\tpanic(\"unimplemented\")\n}\n"

	t.Run("refused at the default", func(t *testing.T) {
		f := newFixture(t, map[string]string{"f.go": body})
		s := f.set(t)
		rej := rejectFuzzy(t, f, s, "f.go", tools.FuzzyEditRequest{
			Path: "f.go", Before: before, After: "x\n",
		})
		if rej.Reason != tools.RejectBelowFloor {
			t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectBelowFloor)
		}
	})

	t.Run("applied under a lenient floor", func(t *testing.T) {
		f := newFixture(t, map[string]string{"f.go": body})
		s := f.set(t)
		s.Limits.FuzzyFloor = 0.5

		res := applyFuzzy(t, s, tools.FuzzyEditRequest{
			Path: "f.go", Before: before, After: "// gone\n",
		})
		if res.Fuzzy.Floor != 0.5 {
			t.Errorf("the result records a floor of %v, want the 0.5 it ran under",
				res.Fuzzy.Floor)
		}
		if res.Fuzzy.Similarity >= tools.DefaultFuzzyFloor {
			t.Errorf("similarity %v is above the default floor, so this case does not "+
				"exercise the parameter", res.Fuzzy.Similarity)
		}
	})

	t.Run("refused under a strict floor", func(t *testing.T) {
		f := newFixture(t, map[string]string{"f.go": body})
		s := f.set(t)
		s.Limits.FuzzyFloor = 1

		// Exact but for one character, which a floor of 1 refuses.
		rej := rejectFuzzy(t, f, s, "f.go", tools.FuzzyEditRequest{
			Path: "f.go", Before: "func alpha(a int) int {\n\treturn a + 2\n}\n", After: "x\n",
		})
		if rej.Reason != tools.RejectBelowFloor {
			t.Errorf("reason = %q, want %q", rej.Reason, tools.RejectBelowFloor)
		}
	})
}

// --- applying ---------------------------------------------------------------

// TestFuzzyAppliesWhenExactlyOneRegionMatches: the fallback does have to work,
// and it has to work on text the model paraphrased — that is the whole reason
// it exists. Indentation and inner spacing differ from the file here, and the
// match is still exact after normalisation.
func TestFuzzyAppliesWhenExactlyOneRegionMatches(t *testing.T) {
	body := "package main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n"
	f := newFixture(t, map[string]string{"main.go": body})
	s := f.set(t)

	res := applyFuzzy(t, s, tools.FuzzyEditRequest{
		Path: "main.go",
		// The model has lost the tab, indented with spaces instead, and left
		// whitespace on the end. All three are collapsed, so this is an exact
		// match after normalisation.
		Before: "    fmt.Println(\"hello\")   \n",
		After:  "\tfmt.Println(\"goodbye\")\n",
	})

	want := "package main\n\nfunc main() {\n\tfmt.Println(\"goodbye\")\n}\n"
	if got := onDisk(t, f, "main.go"); got != want {
		t.Errorf("file on disk =\n%q\nwant\n%q", got, want)
	}
	if res.StartLine != 4 || res.EndLine != 4 {
		t.Errorf("region = %d-%d, want line 4", res.StartLine, res.EndLine)
	}
	if res.LinesRemoved != 1 || res.LinesAdded != 1 {
		t.Errorf("removed/added = %d/%d, want 1/1", res.LinesRemoved, res.LinesAdded)
	}
	if res.Fuzzy.Scanned != 5 {
		t.Errorf("scanned %d regions, want one per line of a five-line file", res.Fuzzy.Scanned)
	}
	if res.Fuzzy.Similarity != 1 {
		t.Errorf("similarity = %v; whitespace should have normalised away", res.Fuzzy.Similarity)
	}
}

// TestFuzzyAppliesToAnImperfectLoneMatchAndSaysSo pins the residual risk rather
// than pretending it is closed. Exactly one region is above the floor and it is
// not an exact match — the tool applies, because refusing every inexact match
// would make the fallback an exact-match tool with extra steps. If that lone
// match is the wrong region, nothing here detects it. What the harness does
// instead is *declare* it: the mode is fuzzy, and SLICE-1 §9 marks the whole
// session `unattributed` rather than charging the failure to the model.
func TestFuzzyAppliesToAnImperfectLoneMatchAndSaysSo(t *testing.T) {
	body := "package main\n\nfunc handler(w http.ResponseWriter, r *http.Request) error {\n" +
		"\treturn writeJSON(w, http.StatusOK, payload)\n}\n"
	f := newFixture(t, map[string]string{"h.go": body})
	s := f.set(t)

	res := applyFuzzy(t, s, tools.FuzzyEditRequest{
		Path: "h.go",
		// One character off: the model remembers "StatusOk" where the file says
		// "StatusOK".
		Before: "\treturn writeJSON(w, http.StatusOk, payload)\n",
		After:  "\treturn writeJSON(w, http.StatusCreated, payload)\n",
	})

	if res.Fuzzy.Similarity == 1 {
		t.Fatal("the fixture matched exactly, so it does not exercise an imperfect match")
	}
	if res.Fuzzy.Similarity < res.Fuzzy.Floor {
		t.Fatalf("similarity %v is below the floor %v but the edit applied",
			res.Fuzzy.Similarity, res.Fuzzy.Floor)
	}
	if res.Mode != tools.ModeFuzzy {
		t.Errorf("mode = %q, want %q — this is the session's unattributed trigger",
			res.Mode, tools.ModeFuzzy)
	}
	if !strings.Contains(res.Output, tools.ModeFuzzy) {
		t.Errorf("the model-facing output does not say the match was fuzzy:\n%s", res.Output)
	}
}

// TestFuzzySplicesLikeAnchoredMode holds that the fallback reuses edit_file's
// splice and its diff renderer rather than growing a second copy of either. A
// second renderer is how the byte-identical-journal criterion breaks, and a
// second splice is how a CRLF checkout silently becomes LF.
func TestFuzzySplicesLikeAnchoredMode(t *testing.T) {
	t.Run("CRLF survives, including on the edited line", func(t *testing.T) {
		f := newFixture(t, map[string]string{"f.txt": "alpha\r\nbeta\r\ngamma\r\n"})
		s := f.set(t)
		applyFuzzy(t, s, tools.FuzzyEditRequest{
			Path: "f.txt", Before: "beta\n", After: "one\ntwo\n",
		})
		if got, want := onDisk(t, f, "f.txt"), "alpha\r\none\r\ntwo\r\ngamma\r\n"; got != want {
			t.Errorf("file on disk = %q, want %q", got, want)
		}
	})

	t.Run("a missing trailing newline stays missing", func(t *testing.T) {
		f := newFixture(t, map[string]string{"f.txt": "alpha\nbeta\ngamma"})
		s := f.set(t)
		applyFuzzy(t, s, tools.FuzzyEditRequest{
			Path: "f.txt", Before: "gamma\n", After: "GAMMA\n",
		})
		if got, want := onDisk(t, f, "f.txt"), "alpha\nbeta\nGAMMA"; got != want {
			t.Errorf("file on disk = %q, want %q", got, want)
		}
	})

	t.Run("empty After deletes the region", func(t *testing.T) {
		f := newFixture(t, map[string]string{"f.txt": "one\ntwo\nthree\n"})
		s := f.set(t)
		res := applyFuzzy(t, s, tools.FuzzyEditRequest{
			Path: "f.txt", Before: "two\n", After: "",
		})
		if got, want := onDisk(t, f, "f.txt"), "one\nthree\n"; got != want {
			t.Errorf("file on disk = %q, want %q", got, want)
		}
		if res.LinesAdded != 0 {
			t.Errorf("LinesAdded = %d, want 0", res.LinesAdded)
		}
	})

	t.Run("the diff is the one edit_file would have rendered", func(t *testing.T) {
		f := newFixture(t, map[string]string{"main.go": editSample})
		s := f.set(t)
		res := applyFuzzy(t, s, tools.FuzzyEditRequest{
			Path: "main.go", Before: "\tfmt.Println(\"hello\")\n", After: "\tfmt.Println(\"goodbye\")\n",
		})
		want := "--- a/main.go\n" +
			"+++ b/main.go\n" +
			"@@ -3,5 +3,5 @@\n" +
			" import \"fmt\"\n" +
			" \n" +
			" func main() {\n" +
			"-\tfmt.Println(\"hello\")\n" +
			"+\tfmt.Println(\"goodbye\")\n" +
			" }\n"
		if res.Diff != want {
			t.Errorf("diff =\n%s\nwant\n%s", res.Diff, want)
		}
	})
}

// --- the mode is unmistakable -----------------------------------------------

// TestFuzzyModeIsUnmistakable is the guard on SLICE-1 §9's headline number.
//
// A session that used the fallback and did not say so is classified `model`
// instead of `unattributed`, which under-counts the bucket in the flattering
// direction — the harness looks better than it is, on the one metric the
// project exists to produce. So every fuzzy return path carries the mode twice,
// in a string and in a pointer, and no anchored path carries either.
func TestFuzzyModeIsUnmistakable(t *testing.T) {
	body := "alpha\nbravo\ncharlie\n"

	ended, cancel := context.WithCancel(context.Background())
	cancel()

	fuzzyCalls := map[string]func(*tools.Set) tools.EditResult{
		"applied": func(s *tools.Set) tools.EditResult {
			res, err := s.EditFileFuzzy(context.Background(), tools.FuzzyEditRequest{
				Path: "f.txt", Before: "bravo\n", After: "BRAVO\n"})
			if err != nil {
				t.Fatalf("applied: %v", err)
			}
			return res
		},
		"below the floor": func(s *tools.Set) tools.EditResult {
			res, _ := s.EditFileFuzzy(context.Background(), tools.FuzzyEditRequest{
				Path: "f.txt", Before: "nothing like it\n", After: "x\n"})
			return res
		},
		"ambiguous": func(s *tools.Set) tools.EditResult {
			res, _ := s.EditFileFuzzy(context.Background(), tools.FuzzyEditRequest{
				Path: "dup.txt", Before: "same\n", After: "x\n"})
			return res
		},
		"cancelled": func(s *tools.Set) tools.EditResult {
			res, _ := s.EditFileFuzzy(ended, tools.FuzzyEditRequest{
				Path: "f.txt", Before: "bravo\n", After: "x\n"})
			return res
		},
	}

	for name, call := range fuzzyCalls {
		t.Run("fuzzy/"+name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"f.txt": body, "dup.txt": "same\nx\nsame\n"})
			res := call(f.set(t))
			if res.Mode != tools.ModeFuzzy {
				t.Errorf("Mode = %q, want %q", res.Mode, tools.ModeFuzzy)
			}
			if res.Fuzzy == nil {
				t.Error("Fuzzy is nil; a caller checking the pointer instead of the string " +
					"would miss that the fallback ran")
			} else if res.Fuzzy.Floor != tools.DefaultFuzzyFloor {
				t.Errorf("Fuzzy.Floor = %v, want the floor in force", res.Fuzzy.Floor)
			}
			if res.AnchorVersion != "" {
				t.Errorf("AnchorVersion = %q; no anchor was derived, and stamping the anchor "+
					"contract on a record that never used one invites the reader of "+
					"journal.EditApplied to conclude the edit was anchored", res.AnchorVersion)
			}
		})
	}

	anchoredCalls := map[string]func(*tools.Set, []string) tools.EditResult{
		"applied": func(s *tools.Set, a []string) tools.EditResult {
			return applyEdit(t, s, tools.EditRequest{
				Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "BRAVO\n"})
		},
		"rejected": func(s *tools.Set, _ []string) tools.EditResult {
			res, _ := s.EditFile(context.Background(), tools.EditRequest{
				Path: "f.txt", AnchorStart: "deadbeef", AnchorEnd: "deadbeef", NewText: "x\n"})
			return res
		},
		"cancelled": func(s *tools.Set, a []string) tools.EditResult {
			res, _ := s.EditFile(ended, tools.EditRequest{
				Path: "f.txt", AnchorStart: a[1], AnchorEnd: a[1], NewText: "x\n"})
			return res
		},
	}

	for name, call := range anchoredCalls {
		t.Run("anchored/"+name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"f.txt": body})
			res := call(f.set(t), derivedAnchors(body))
			if res.Mode != tools.ModeAnchored {
				t.Errorf("Mode = %q, want %q", res.Mode, tools.ModeAnchored)
			}
			if res.Fuzzy != nil {
				t.Errorf("an anchored edit carried fuzzy detail %+v; the unattributed bucket "+
					"would over-count, which is the same laundering running the other way",
					res.Fuzzy)
			}
		})
	}
}

// --- fuzzy is reachable only by calling it ----------------------------------

// TestAnchoredModeCannotReachTheFuzzyFallback is the structural half of "fuzzy
// never catches anchor drift".
//
// The design rests on the fallback being for a model that could not produce an
// anchor *at all*, never a rescue for one that drifted: a drifted anchor means
// the file moved under the model, and answering that with an approximate match
// on remembered text is exactly how a stale edit lands somewhere plausible. A
// comment saying "there is no fallthrough" is worth nothing the day somebody
// adds one to make a failing test pass, so this walks the package's own call
// graph from EditFile and proves it never arrives at the matcher.
func TestAnchoredModeCannotReachTheFuzzyFallback(t *testing.T) {
	funcs := packageFuncs(t)

	// The machinery that decides *where* an approximate edit goes. Nothing
	// reachable from anchored mode may touch any of it.
	forbidden := []string{
		"EditFileFuzzy", "rankRegions", "keepNearest", "lengthBound",
		"similarity", "levenshtein", "normalizeLines", "normalizeLine", "fuzzyReject",
	}

	reached := reachableFrom(funcs, "EditFile")
	for _, name := range forbidden {
		if reached[name] {
			t.Errorf("(*Set).EditFile reaches %s: anchored mode has a path into the fuzzy "+
				"fallback. A drifted anchor is a hard rejection (ADR-0006 decision 1) and "+
				"must never degrade into an approximate match", name)
		}
	}

	// The positive control. Without it a traversal that resolved nothing at all
	// would pass the check above while asserting nothing whatsoever.
	fromFuzzy := reachableFrom(funcs, "EditFileFuzzy")
	for _, name := range []string{"rankRegions", "similarity", "normalizeLines"} {
		if !fromFuzzy[name] {
			t.Fatalf("EditFileFuzzy does not reach %s, so the call graph is broken and this "+
				"test is asserting nothing", name)
		}
	}
	if !reached["apply"] || !reached["unifiedDiff"] {
		t.Fatal("EditFile does not reach apply/unifiedDiff, so the call graph is broken")
	}
}

// packageFiles parses this package's non-test sources. The test's working
// directory is the package directory, which is what makes "." the package.
func packageFiles(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", name, parseErr)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		t.Fatal("no package sources parsed; this guard would assert nothing")
	}
	return out
}

// packageFuncs returns every top-level function and method by name.
func packageFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	out := map[string]*ast.FuncDecl{}
	for _, file := range packageFiles(t) {
		for _, d := range file.Decls {
			if fn, isFunc := d.(*ast.FuncDecl); isFunc && fn.Body != nil {
				out[fn.Name.Name] = fn
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no functions parsed")
	}
	return out
}

// declaredRejectReasons is every constant of type RejectReason the package
// declares, read out of the source rather than listed by hand.
//
// A hand-written table is a table somebody forgets, and this package has
// already chosen the other discipline once: cancel_test.go derives the set of
// tools by reflection so a sixth one fails the suite instead of diverging.
// Reasons cannot be enumerated by reflection — they are untyped string
// constants — so the source is the next best register.
func declaredRejectReasons(t *testing.T) []tools.RejectReason {
	t.Helper()
	var out []tools.RejectReason
	for _, file := range packageFiles(t) {
		for _, d := range file.Decls {
			gen, isGen := d.(*ast.GenDecl)
			if !isGen || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				if id, isIdent := vs.Type.(*ast.Ident); !isIdent || id.Name != "RejectReason" {
					continue
				}
				for _, v := range vs.Values {
					lit, isLit := v.(*ast.BasicLit)
					if !isLit || lit.Kind != token.STRING {
						t.Fatalf("a RejectReason constant is not a string literal: %v", v)
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquoting %s: %v", lit.Value, err)
					}
					out = append(out, tools.RejectReason(value))
				}
			}
		}
	}
	return out
}

// reachableFrom walks calls from one function, following only names declared in
// this package. It fails open by design: an unresolvable call is simply not
// followed, so the test can under-report reachability but the *positive
// control* above is what stops that from making it vacuous.
func reachableFrom(funcs map[string]*ast.FuncDecl, start string) map[string]bool {
	seen := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		fn, ok := funcs[name]
		if !ok {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			var called string
			switch f := call.Fun.(type) {
			case *ast.Ident:
				called = f.Name
			case *ast.SelectorExpr:
				// s.locate(...) and r.render() both land here; so does
				// anchor.Split, which simply is not in the map.
				called = f.Sel.Name
			}
			if called != "" && !seen[called] {
				seen[called] = true
				queue = append(queue, called)
			}
			return true
		})
	}
	return seen
}

// --- the ordinary refusals --------------------------------------------------

func TestFuzzyRefusesEmptyText(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": "alpha\nbravo\n"})
	s := f.set(t)

	for _, before := range []string{"", "\n\n\n"[:0]} {
		res, err := s.EditFileFuzzy(context.Background(), tools.FuzzyEditRequest{
			Path: "f.txt", Before: before, After: "x\n",
		})
		if !errors.Is(err, tools.ErrNoMatchText) {
			t.Fatalf("Before=%q: error %v does not wrap ErrNoMatchText", before, err)
		}
		wantFault(t, err, tools.FaultTask)
		if res.Rejection != nil {
			t.Errorf("an argument error carried a placement rejection: %+v", res.Rejection)
		}
	}
	if got := onDisk(t, f, "f.txt"); got != "alpha\nbravo\n" {
		t.Errorf("the file changed: %q", got)
	}
}

func TestFuzzyRefusesWhatItCannotRead(t *testing.T) {
	f := newFixture(t, map[string]string{
		"main.go":  editSample,
		"bin.dat":  "ok\x00\x01binary\n",
		"sub/a.go": "package sub\n",
	})
	f.symlink(t, filepath.Join(f.outside, "secret.txt"), "escape.txt")
	s := f.set(t)

	tests := []struct {
		name string
		path string
		want error
	}{
		{"a missing file", "nope.go", os.ErrNotExist},
		{"a directory", "sub", tools.ErrNotRegular},
		{"a binary file", "bin.dat", tools.ErrBinaryFile},
		{"a path outside the root", "../outside/secret.txt", tools.ErrOutsideRoot},
		{"a symlink out of the root", "escape.txt", tools.ErrOutsideRoot},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := s.EditFileFuzzy(context.Background(), tools.FuzzyEditRequest{
				Path: tc.path, Before: "anything\n", After: "x\n",
			})
			if err == nil {
				t.Fatalf("EditFileFuzzy(%q) succeeded", tc.path)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error %v does not wrap %v", err, tc.want)
			}
			if errors.Is(err, tools.ErrEditRejected) {
				t.Error("a file-level refusal was reported as a placement rejection")
			}
			if res.Rejection != nil {
				t.Errorf("a file-level refusal carried a rejection: %+v", res.Rejection)
			}
			wantFault(t, err, tools.FaultTask)
			if !strings.Contains(err.Error(), tools.ToolEditFileFuzzy) {
				t.Errorf("the error names %q rather than the tool that failed: %v",
					tools.ToolEditFile, err)
			}
		})
	}
}

func TestFuzzyRejectionDetailIsTheOutput(t *testing.T) {
	f := newFixture(t, map[string]string{"f.txt": "alpha\nbravo\n"})
	s := f.set(t)

	res, err := s.EditFileFuzzy(context.Background(), tools.FuzzyEditRequest{
		Path: "f.txt", Before: "nothing like it at all\n", After: "x\n",
	})
	if err == nil {
		t.Fatal("the edit was accepted")
	}
	if res.Rejection == nil {
		t.Fatal("no structured rejection")
	}
	if res.Output != res.Rejection.Detail {
		t.Errorf("Output and Rejection.Detail differ:\n%q\n%q", res.Output, res.Rejection.Detail)
	}
	if res.Diff != "" {
		t.Errorf("a refused edit returned a diff: %q", res.Diff)
	}
	if len(res.Rejection.Current) != 0 {
		t.Errorf("a fuzzy refusal returned anchored lines: %+v; anchors come only from "+
			"read_file, and this report quotes content the model may never have read",
			res.Rejection.Current)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
