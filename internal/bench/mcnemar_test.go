package bench_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/bench"
)

// Every expected value in this file comes from somewhere outside the
// implementation, and each case says where. A test whose expectation was
// produced by running the code proves only that the code is deterministic.
//
// Three kinds of source are used:
//
//   - **Exact rationals.** Under the null, the discordant pairs are a
//     Binomial(b+c, 1/2) draw, so the two-sided p-value is an integer over a
//     power of two. Every exact-branch expectation below is written as that
//     fraction with the binomial coefficients spelled out, so it can be checked
//     with a pencil. These agree with R's binom.test(min(b,c), b+c, 0.5).
//   - **Published contingency tables**, scored by hand with the published
//     formula. The 101/121/59/33 table is the worked example in Wikipedia's
//     "McNemar's test" article.
//   - **Published chi-squared and standard-normal tables.** The upper-tail
//     function is pinned against textbook critical values in
//     tail_internal_test.go; here the asymptotic cases assert the statistic
//     exactly and bracket the p-value between two published critical values.

// relTol is the tolerance for a float comparison against a hand-computed
// constant. The exact-branch values are integers over powers of two and are
// representable without rounding, so this is slack for summation order and
// nothing else — it is nowhere near wide enough to hide a wrong formula, a
// missing factor of two, or a dropped continuity correction, all of which move
// a p-value by percent to orders of magnitude.
const relTol = 1e-12

func closeEnough(got, want, tol float64) bool {
	if got == want {
		return true
	}
	if want == 0 {
		return math.Abs(got) <= tol
	}
	return math.Abs(got-want)/math.Abs(want) <= tol
}

// TestMcNemarExactBranch covers every discordant count below the threshold,
// which for this project's 10-to-20 task corpus is every run it will ever see.
func TestMcNemarExactBranch(t *testing.T) {
	tests := []struct {
		name      string
		table     bench.Table
		method    bench.Method
		p         float64
		direction bench.Direction
		source    string
	}{
		{
			name:      "no discordant pairs",
			table:     bench.Table{BothPassed: 8, AOnly: 0, BOnly: 0, BothFailed: 12},
			method:    bench.MethodNone,
			p:         1,
			direction: bench.NoDifference,
			source: "b+c = 0. Both formulas divide by b+c, so neither test is " +
				"defined; the arms agreed on all 20 tasks and there is no " +
				"evidence either way, so p is exactly 1.",
		},
		{
			name:      "six flips all one way",
			table:     bench.Table{BothPassed: 4, AOnly: 6, BOnly: 0, BothFailed: 10},
			method:    bench.MethodExactBinomial,
			p:         2 * 1.0 / 64.0, // 2 * C(6,0)/2^6 = 2/64 = 0.03125
			direction: bench.FavoursA,
			source: "The classic sign test on 6 discordant pairs all favouring " +
				"one arm: 2 * P(X = 0), X ~ Bin(6, 1/2) = 2/64 = 0.03125. " +
				"R: binom.test(0, 6, 0.5)$p.value.",
		},
		{
			name:      "eight-two split",
			table:     bench.Table{BothPassed: 5, AOnly: 8, BOnly: 2, BothFailed: 5},
			method:    bench.MethodExactBinomial,
			p:         2 * 56.0 / 1024.0, // C(10,0)+C(10,1)+C(10,2) = 1+10+45 = 56
			direction: bench.FavoursA,
			source: "2 * P(X <= 2), X ~ Bin(10, 1/2) = 2*56/1024 = 0.109375. " +
				"Not significant at 5% on ten flips, which is the whole reason " +
				"the exact test is used down here. R: binom.test(2, 10, 0.5).",
		},
		{
			name:      "nine-one split favouring B",
			table:     bench.Table{BothPassed: 5, AOnly: 1, BOnly: 9, BothFailed: 5},
			method:    bench.MethodExactBinomial,
			p:         2 * 11.0 / 1024.0, // C(10,0)+C(10,1) = 1+10 = 11
			direction: bench.FavoursB,
			source: "2 * P(X <= 1), X ~ Bin(10, 1/2) = 22/1024 = 0.021484375. " +
				"Same magnitude as the row above, opposite direction — the " +
				"p-value cannot distinguish them and Direction must.",
		},
		{
			name:      "even split is capped at one",
			table:     bench.Table{BothPassed: 5, AOnly: 5, BOnly: 5, BothFailed: 5},
			method:    bench.MethodExactBinomial,
			p:         1,
			direction: bench.NoDifference,
			source: "2 * P(X <= 5), X ~ Bin(10, 1/2) = 2*638/1024 = 1.24609375, " +
				"which exceeds 1 by the weight of the median and is capped. " +
				"C(10,0..5) = 1+10+45+120+210+252 = 638.",
		},
		{
			name:      "single discordant pair",
			table:     bench.Table{BothPassed: 9, AOnly: 1, BOnly: 0, BothFailed: 10},
			method:    bench.MethodExactBinomial,
			p:         1,
			direction: bench.FavoursA,
			source: "2 * P(X = 0), X ~ Bin(1, 1/2) = 2 * 1/2 = 1. One flip is " +
				"never evidence, but it still has a direction.",
		},
		{
			name:      "twenty-four discordant, lopsided, still exact",
			table:     bench.Table{BothPassed: 0, AOnly: 20, BOnly: 4, BothFailed: 0},
			method:    bench.MethodExactBinomial,
			p:         2 * 12951.0 / 16777216.0, // C(24,0..4) = 1+24+276+2024+10626
			direction: bench.FavoursA,
			source: "b+c = 24 is the last count below ExactMaxDiscordant. " +
				"2 * P(X <= 4), X ~ Bin(24, 1/2) = 25902/16777216 " +
				"= 0.00154387950897216796875.",
		},
		{
			name:      "twenty-four discordant, even split",
			table:     bench.Table{BothPassed: 0, AOnly: 12, BOnly: 12, BothFailed: 0},
			method:    bench.MethodExactBinomial,
			p:         1,
			direction: bench.NoDifference,
			source: "2 * P(X <= 12), X ~ Bin(24, 1/2) = 2*9740686/16777216 " +
				"= 1.16117... , capped at 1.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bench.McNemar(tc.table)
			if err != nil {
				t.Fatalf("McNemar(%+v): unexpected error: %v", tc.table, err)
			}
			if got.Method != tc.method {
				t.Errorf("Method = %v, want %v (%s)", got.Method, tc.method, tc.source)
			}
			if !closeEnough(got.P, tc.p, relTol) {
				t.Errorf("P = %v, want %v (%s)", got.P, tc.p, tc.source)
			}
			if got.Direction != tc.direction {
				t.Errorf("Direction = %v, want %v", got.Direction, tc.direction)
			}
			if got.Statistic != 0 {
				t.Errorf("Statistic = %v, want 0: the exact branch computes no chi-squared value", got.Statistic)
			}
			if got.Discordant() != tc.table.AOnly+tc.table.BOnly {
				t.Errorf("Discordant() = %d, want %d", got.Discordant(), tc.table.AOnly+tc.table.BOnly)
			}
			if got.Table != tc.table {
				t.Errorf("Table = %+v, want %+v", got.Table, tc.table)
			}
		})
	}
}

// TestMcNemarAsymptoticBranch pins the statistic exactly and brackets the
// p-value between published chi-squared critical values on 1 degree of freedom.
// The bracket is a factor of five or tighter, so it cannot absorb a dropped
// continuity correction (which moves 9.61 to 10.24) let alone a wrong formula.
func TestMcNemarAsymptoticBranch(t *testing.T) {
	// pLow and pHi bracket the p-value between two published upper-tail
	// critical values for chi-squared on 1 degree of freedom; the source
	// string names which. When pLow == pHi the p-value is exact and is
	// compared directly. The critical values themselves are pinned against
	// their published p-levels in tail_internal_test.go.
	tests := []struct {
		name      string
		table     bench.Table
		statistic float64
		pLow, pHi float64
		direction bench.Direction
		source    string
	}{
		{
			name: "wikipedia mcnemar worked example",
			// Test 1 positive / Test 2 positive = 101, Test 1 positive /
			// Test 2 negative = 121, Test 1 negative / Test 2 positive =
			// 59, both negative = 33.
			table:     bench.Table{BothPassed: 101, AOnly: 121, BOnly: 59, BothFailed: 33},
			statistic: 3721.0 / 180.0, // (|121-59| - 1)^2 / 180 = 61^2/180 = 20.67222...
			pLow:      0,
			pHi:       0.001,
			direction: bench.FavoursA,
			source: "Wikipedia, \"McNemar's test\", worked example. It quotes the " +
				"uncorrected statistic 3844/180 = 21.35 and p < 0.001; the " +
				"continuity-corrected statistic is 3721/180 = 20.672, still " +
				"far above the 0.001 critical value 10.827566.",
		},
		{
			name:      "sixty-six thirty-four",
			table:     bench.Table{BothPassed: 0, AOnly: 66, BOnly: 34, BothFailed: 0},
			statistic: 961.0 / 100.0, // (32-1)^2/100 = 9.61
			pLow:      0.001,
			pHi:       0.005,
			direction: bench.FavoursA,
			source: "Hand arithmetic on Edwards' formula. 9.61 sits between the " +
				"published 0.005 and 0.001 critical values 7.879439 and " +
				"10.827566, so 0.001 < p < 0.005.",
		},
		{
			name:      "thirty-eight twelve",
			table:     bench.Table{BothPassed: 0, AOnly: 12, BOnly: 38, BothFailed: 0},
			statistic: 625.0 / 50.0, // (26-1)^2/50 = 12.5
			pLow:      0,
			pHi:       0.001,
			direction: bench.FavoursB,
			source: "Hand arithmetic. 12.5 exceeds the 0.001 critical value " +
				"10.827566, and the direction is the mirror of the row above.",
		},
		{
			name:      "twenty-five discordant is the first asymptotic count",
			table:     bench.Table{BothPassed: 0, AOnly: 13, BOnly: 12, BothFailed: 0},
			statistic: 0, // |13-12| = 1, so (1-1)^2/25 = 0
			pLow:      1,
			pHi:       1,
			direction: bench.FavoursA,
			source: "b+c = 25 = ExactMaxDiscordant, the first count scored " +
				"asymptotically. The continuity correction takes |b-c| = 1 " +
				"to a statistic of exactly 0, hence p = 1: a single net flip " +
				"out of 25 is no evidence at all.",
		},
		{
			name:      "twenty-six discordant, dead even",
			table:     bench.Table{BothPassed: 0, AOnly: 13, BOnly: 13, BothFailed: 0},
			statistic: 0, // |b-c| = 0, clamped by the continuity correction
			pLow:      1,
			pHi:       1,
			direction: bench.NoDifference,
			source: "The flips cancelled exactly. Edwards' correction subtracts 1 " +
				"from |b-c| before squaring, so without the clamp at |b-c| < 1 " +
				"this table would score (0-1)^2/26 = 0.0385 and read as weak " +
				"evidence of a difference the data does not contain.",
		},
		{
			name:      "thirty discordant, twenty-two to eight",
			table:     bench.Table{BothPassed: 0, AOnly: 22, BOnly: 8, BothFailed: 0},
			statistic: 169.0 / 30.0, // (14-1)^2/30 = 5.6333...
			pLow:      0.01,
			pHi:       0.05,
			direction: bench.FavoursA,
			source: "Hand arithmetic. 5.633 sits between the published 0.05 and " +
				"0.01 critical values 3.841459 and 6.634897.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bench.McNemar(tc.table)
			if err != nil {
				t.Fatalf("McNemar(%+v): unexpected error: %v", tc.table, err)
			}
			if got.Method != bench.MethodChiSquared {
				t.Errorf("Method = %v, want %v", got.Method, bench.MethodChiSquared)
			}
			if !closeEnough(got.Statistic, tc.statistic, relTol) {
				t.Errorf("Statistic = %v, want %v (%s)", got.Statistic, tc.statistic, tc.source)
			}
			if got.Direction != tc.direction {
				t.Errorf("Direction = %v, want %v", got.Direction, tc.direction)
			}
			switch tc.pLow {
			case tc.pHi:
				if !closeEnough(got.P, tc.pHi, relTol) {
					t.Errorf("P = %v, want %v (%s)", got.P, tc.pHi, tc.source)
				}
			default:
				if got.P <= tc.pLow || got.P >= tc.pHi {
					t.Errorf("P = %v, want in (%v, %v) (%s)", got.P, tc.pLow, tc.pHi, tc.source)
				}
			}
		})
	}
}

// TestMcNemarRefusesUnscorableTables covers the two inputs that are not a
// result waiting to be computed. A table over no tasks is not the same fact as
// a table with no discordant pairs, and the scorer must not turn either into a
// p-value that reads like evidence.
func TestMcNemarRefusesUnscorableTables(t *testing.T) {
	tests := []struct {
		name  string
		table bench.Table
		want  error
	}{
		{"empty table", bench.Table{}, bench.ErrNoPairs},
		{"negative both-passed", bench.Table{BothPassed: -1, AOnly: 3, BOnly: 2}, bench.ErrNegativeCount},
		{"negative a-only", bench.Table{AOnly: -1, BOnly: 2}, bench.ErrNegativeCount},
		{"negative b-only", bench.Table{AOnly: 2, BOnly: -1}, bench.ErrNegativeCount},
		{"negative both-failed", bench.Table{AOnly: 2, BOnly: 1, BothFailed: -4}, bench.ErrNegativeCount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bench.McNemar(tc.table)
			if !errors.Is(err, tc.want) {
				t.Fatalf("McNemar(%+v) error = %v, want %v", tc.table, err, tc.want)
			}
			if got != (bench.Result{}) {
				t.Errorf("McNemar returned %+v alongside the error; a refused table must score nothing", got)
			}
			var e *bench.Error
			if !errors.As(err, &e) {
				t.Fatalf("error %v is not a *bench.Error", err)
			}
			if e.Op != "mcnemar" {
				t.Errorf("Op = %q, want %q", e.Op, "mcnemar")
			}
			if e.Detail == "" {
				t.Error("Detail is empty; a refusal must say what was wrong")
			}
		})
	}
}

// TestMcNemarSweepInvariants walks every discordant split across both branches
// and holds the properties that must survive a change to either. Enumerating
// beats a hand table here: a new Method, a new branch, or a threshold moved by
// one all fail this without anyone remembering to add a row.
func TestMcNemarSweepInvariants(t *testing.T) {
	const maxDiscordant = 40 // comfortably past ExactMaxDiscordant = 25

	for n := 0; n <= maxDiscordant; n++ {
		var prevP float64 = -1
		// Walk from the most even split outwards, so |b-c| increases and
		// p must not increase.
		for b := n / 2; b <= n; b++ {
			c := n - b
			table := bench.Table{BothPassed: 1, AOnly: b, BOnly: c, BothFailed: 1}

			r, err := bench.McNemar(table)
			if err != nil {
				t.Fatalf("McNemar(%+v): unexpected error: %v", table, err)
			}

			// The rule is not optional and not silent.
			wantMethod := bench.MethodChiSquared
			switch {
			case n == 0:
				wantMethod = bench.MethodNone
			case n < bench.ExactMaxDiscordant:
				wantMethod = bench.MethodExactBinomial
			}
			if r.Method != wantMethod {
				t.Fatalf("b=%d c=%d: Method = %v, want %v", b, c, r.Method, wantMethod)
			}

			// Every value the scorer can produce must have report text.
			// A Method or Direction added without one fails here rather
			// than printing "method(3)" into a benchmark report.
			if s := r.Method.String(); s == "" || strings.HasPrefix(s, "method(") {
				t.Fatalf("b=%d c=%d: Method.String() = %q has no report text", b, c, s)
			}
			if s := r.Direction.String(); s == "" || strings.HasPrefix(s, "direction(") {
				t.Fatalf("b=%d c=%d: Direction.String() = %q has no report text", b, c, s)
			}

			if r.P < 0 || r.P > 1 || math.IsNaN(r.P) {
				t.Fatalf("b=%d c=%d: P = %v is not a probability", b, c, r.P)
			}
			if math.IsNaN(r.Statistic) || r.Statistic < 0 {
				t.Fatalf("b=%d c=%d: Statistic = %v", b, c, r.Statistic)
			}

			// Monotone: a more lopsided split is never weaker evidence.
			if prevP >= 0 && r.P > prevP+relTol {
				t.Fatalf("b=%d c=%d: P = %v exceeds the P of the less lopsided split (%v)", b, c, r.P, prevP)
			}
			prevP = r.P

			// Swapping the arms mirrors the direction and changes
			// nothing else. This is the check that catches b and c
			// transposed, which is the single most likely bug here and
			// the one a symmetric hand table would never see.
			swapped, err := bench.McNemar(bench.Table{
				BothPassed: table.BothPassed,
				AOnly:      table.BOnly,
				BOnly:      table.AOnly,
				BothFailed: table.BothFailed,
			})
			if err != nil {
				t.Fatalf("McNemar(swapped b=%d c=%d): unexpected error: %v", c, b, err)
			}
			if swapped.P != r.P || swapped.Statistic != r.Statistic || swapped.Method != r.Method {
				t.Fatalf("b=%d c=%d: swapping the arms changed the test: %+v vs %+v", b, c, swapped, r)
			}
			if swapped.Direction != -r.Direction {
				t.Fatalf("b=%d c=%d: swapped Direction = %v, want %v", b, c, swapped.Direction, -r.Direction)
			}
		}
	}
}

// TestMcNemarDirectionNamesTheWinner is the reason Direction exists: two
// opposite results carry the same p-value, and a report with only the p-value
// cannot say which arm won.
func TestMcNemarDirectionNamesTheWinner(t *testing.T) {
	aWins, err := bench.McNemar(bench.Table{BothPassed: 3, AOnly: 7, BOnly: 2, BothFailed: 8})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	bWins, err := bench.McNemar(bench.Table{BothPassed: 3, AOnly: 2, BOnly: 7, BothFailed: 8})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if aWins.P != bWins.P {
		t.Fatalf("mirrored tables have different p-values: %v vs %v", aWins.P, bWins.P)
	}
	if aWins.Direction != bench.FavoursA {
		t.Errorf("Direction = %v, want %v", aWins.Direction, bench.FavoursA)
	}
	if bWins.Direction != bench.FavoursB {
		t.Errorf("Direction = %v, want %v", bWins.Direction, bench.FavoursB)
	}
	if got := aWins.Direction.String(); got != "favours A" {
		t.Errorf("FavoursA.String() = %q", got)
	}
	if got := bWins.Direction.String(); got != "favours B" {
		t.Errorf("FavoursB.String() = %q", got)
	}
}
