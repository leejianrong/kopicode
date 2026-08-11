// Package bench holds the benchmark runner and the A/B scoring it reports
// (docs/adr/0005-benchmark-and-ab-methodology.md).
//
// Only the paired scorer exists today. ADR-0005 decision 1 rules out
// difference-of-means for this rig and the reason is arithmetic rather than
// taste: at n=20 tasks and a ~40% pass rate the 95% confidence interval on a
// single arm's score is roughly ±21 percentage points, which is the same
// magnitude as the harness effect the project exists to measure. Two such
// scores subtracted are noise. Pairing removes task difficulty as a variance
// source and scores only the tasks the two arms disagreed about, which is what
// makes a corpus this small usable at all.
//
// The scorer is scaffold in slice 1 (docs/SLICE-1.md affordance B3): there is
// one harness configuration and therefore no second arm to feed it. It is
// complete and correct on its own terms, driven entirely by unit tests, and it
// takes its input as data rather than reaching into a runner that does not yet
// exist.
package bench

import (
	"fmt"
	"math"
)

// ExactMaxDiscordant is the largest discordant count scored by the exact test.
//
// **The rule, stated once, here.** With fewer than [ExactMaxDiscordant]
// discordant pairs the scorer uses the exact conditional binomial test; at
// [ExactMaxDiscordant] or more it uses the chi-squared approximation with
// Edwards' continuity correction. The threshold is the conventional one for
// McNemar's test: below roughly 25 discordant pairs the chi-squared
// approximation to the conditional binomial is not trustworthy, and an exact
// test is the honest answer rather than a more comfortable one.
//
// Which side of the threshold a result landed on is reported in
// [Result.Method]. That is the point of naming it: a scorer that quietly
// switches method between two arms of the same experiment is worse than one
// that refuses, because the switch is invisible in the number it prints.
//
// For this project's corpus the exact path is effectively the only one. A
// 10-to-20 task corpus cannot produce 25 discordant pairs, so the asymptotic
// branch exists for a corpus that has grown, and for the reader who wants to
// know that the small-sample case was chosen rather than defaulted into.
const ExactMaxDiscordant = 25

// Method says which test produced [Result.P].
type Method uint8

const (
	// MethodNone means there was nothing to test: the two arms agreed on
	// every task, so the discordant count is zero and no statistic is
	// defined. [Result.P] is 1 and [Result.Statistic] is 0.
	//
	// This is a legitimate outcome, not an error — the arms genuinely
	// produced no evidence of a difference — but it is a distinct value
	// because "p = 1 because b+c = 0" and "p = 1 because the flips
	// cancelled" are different facts about a run, and a report that
	// conflates them is hiding that the comparison never had any signal.
	MethodNone Method = iota

	// MethodExactBinomial is the exact conditional test: under the null,
	// each discordant pair is an independent coin flip, so the number
	// favouring arm A is Binomial(b+c, 1/2). The two-sided p-value is twice
	// the smaller tail, capped at 1.
	MethodExactBinomial

	// MethodChiSquared is the asymptotic test with Edwards' continuity
	// correction: chi-squared = (|b-c| - 1)^2 / (b+c) on 1 degree of
	// freedom. The correction is the default in R's mcnemar.test and it errs
	// conservative, which is the direction to be wrong in for a project
	// whose whole claim is a measured delta.
	MethodChiSquared
)

var methodText = map[Method]string{
	MethodNone:          "none",
	MethodExactBinomial: "exact-binomial",
	MethodChiSquared:    "chi-squared-corrected",
}

// String returns the report form of the method.
func (m Method) String() string {
	if s, ok := methodText[m]; ok {
		return s
	}
	return fmt.Sprintf("method(%d)", uint8(m))
}

// Direction says which arm the discordant pairs favoured.
//
// It exists because a bare p-value cannot tell a reader which arm won, and a
// significant result with no direction is unreadable. Its underlying values are
// the sign of b - c, so callers can compare and sort on it.
type Direction int8

const (
	// FavoursB means arm B passed more of the tasks the arms disagreed on.
	FavoursB Direction = -1
	// NoDifference means the flips cancelled exactly, or there were none.
	NoDifference Direction = 0
	// FavoursA means arm A passed more of the tasks the arms disagreed on.
	FavoursA Direction = 1
)

var directionText = map[Direction]string{
	FavoursB:     "favours B",
	NoDifference: "no difference",
	FavoursA:     "favours A",
}

// String returns the report form of the direction.
func (d Direction) String() string {
	if s, ok := directionText[d]; ok {
		return s
	}
	return fmt.Sprintf("direction(%d)", int8(d))
}

// Table is the 2x2 paired contingency table over one task set run by two arms.
//
// The cells use the conventional McNemar names in their comments (a, b, c, d)
// so the arithmetic below can be read against any textbook, but the field names
// say what they mean, because b and c are the two that get swapped by accident.
type Table struct {
	// BothPassed is cell a: tasks both arms passed. Carries no information
	// about the delta.
	BothPassed int
	// AOnly is cell b: tasks arm A passed and arm B failed.
	AOnly int
	// BOnly is cell c: tasks arm B passed and arm A failed.
	BOnly int
	// BothFailed is cell d: tasks both arms failed. Carries no information
	// about the delta.
	BothFailed int
}

// Pairs is the number of tasks in the table.
func (t Table) Pairs() int {
	return t.BothPassed + t.AOnly + t.BOnly + t.BothFailed
}

// Discordant is b + c, the only cells the test reads.
func (t Table) Discordant() int { return t.AOnly + t.BOnly }

// Result is a scored paired comparison.
//
// It reports the table and the direction alongside the p-value on purpose.
// ADR-0005 decision 8 asks every result to state what it covered; a p-value
// with no discordant counts cannot be checked, and one with no direction cannot
// be read.
type Result struct {
	// ArmA and ArmB name the arms, when the caller supplied names. [Compare]
	// fills them; [McNemar] leaves them empty because a bare table has no
	// names to carry.
	ArmA, ArmB string

	// Table is the contingency table that was scored.
	Table Table

	// Method is the test that produced P.
	Method Method

	// Statistic is the continuity-corrected chi-squared value, on 1 degree
	// of freedom. It is 0 for [MethodExactBinomial] and [MethodNone], which
	// compute no such statistic — read Method before reading this field.
	Statistic float64

	// P is the two-sided p-value: the probability of a split of the
	// discordant pairs at least this lopsided if the two arms were equally
	// capable. It is 1 when there are no discordant pairs.
	P float64

	// Direction says which arm the flips favoured.
	Direction Direction
}

// Discordant is b + c, the pairs the comparison actually rests on.
func (r Result) Discordant() int { return r.Table.Discordant() }

// McNemar scores a contingency table.
//
// It returns an error for a negative cell, and for a table covering no tasks at
// all. Neither is a small p-value waiting to be computed: a comparison over
// zero tasks is not evidence, and ADR-0005 exists because this project's
// numbers are only worth anything if the comparisons behind them are refusable.
// A table whose cells are all zero is that case, and it is distinct from the
// zero-discordant case — b = c = 0 with tasks in the table is a real result
// meaning the arms never disagreed.
func McNemar(t Table) (Result, error) {
	for _, cell := range []struct {
		name string
		n    int
	}{
		{"BothPassed", t.BothPassed},
		{"AOnly", t.AOnly},
		{"BOnly", t.BOnly},
		{"BothFailed", t.BothFailed},
	} {
		if cell.n < 0 {
			return Result{}, &Error{
				Op:     "mcnemar",
				Detail: fmt.Sprintf("cell %s is %d", cell.name, cell.n),
				err:    ErrNegativeCount,
			}
		}
	}
	if t.Pairs() == 0 {
		return Result{}, &Error{
			Op:     "mcnemar",
			Detail: "the contingency table covers no tasks",
			err:    ErrNoPairs,
		}
	}

	r := Result{Table: t, Direction: direction(t.AOnly, t.BOnly)}

	n := t.Discordant()
	switch {
	case n == 0:
		// Both formulas divide by b + c. Nothing is being approximated
		// here and nothing is being hidden: the arms agreed on every
		// task, so there is no evidence either way and p is exactly 1.
		r.Method = MethodNone
		r.P = 1
	case n < ExactMaxDiscordant:
		r.Method = MethodExactBinomial
		r.P = exactTwoSidedBinomialP(t.AOnly, t.BOnly)
	default:
		r.Method = MethodChiSquared
		r.Statistic = correctedChiSquared(t.AOnly, t.BOnly)
		r.P = chiSquaredUpperTail1DF(r.Statistic)
	}
	return r, nil
}

func direction(b, c int) Direction {
	switch {
	case b > c:
		return FavoursA
	case c > b:
		return FavoursB
	default:
		return NoDifference
	}
}

// exactTwoSidedBinomialP is the exact conditional test. Under the null the b
// discordant pairs favouring A are a Binomial(b+c, 1/2) draw, so the two-sided
// p-value is twice the smaller tail, capped at 1 (the cap bites whenever the
// split is near even, where twice the tail exceeds 1 by the weight of the
// median).
//
// The sum is computed in float64 and is *exact* over the range this function is
// reached with. Only b+c < [ExactMaxDiscordant] gets here, so n <= 24: every
// partial binomial coefficient and the 2^-n scaling are representable without
// rounding, and the iteration C(n,i) = C(n,i-1) * (n-i+1) / i is an exact
// integer at each step. It is not written to be correct for large n because it
// is not reachable with large n.
func exactTwoSidedBinomialP(b, c int) float64 {
	n := b + c
	k := min(b, c)

	sum, coefficient := 1.0, 1.0 // i = 0: C(n,0) = 1
	for i := 1; i <= k; i++ {
		coefficient = coefficient * float64(n-i+1) / float64(i)
		sum += coefficient
	}
	return min(2*sum*math.Ldexp(1, -n), 1)
}

// correctedChiSquared is McNemar's statistic with Edwards' continuity
// correction: (|b-c| - 1)^2 / (b+c), on 1 degree of freedom.
//
// |b-c| < 1 is clamped to a statistic of 0 rather than squaring a negative
// difference, which would turn a table with b = c into a *larger* statistic
// than one with |b-c| = 1 and read as weak evidence of a difference that the
// data says nothing about.
func correctedChiSquared(b, c int) float64 {
	d := b - c
	if d < 0 {
		d = -d
	}
	if d < 1 {
		return 0
	}
	return float64((d-1)*(d-1)) / float64(b+c)
}

// chiSquaredUpperTail1DF is P(X > x) for X chi-squared on 1 degree of freedom.
//
// On 1 degree of freedom the chi-squared upper tail is the two-sided standard
// normal tail at sqrt(x): P(X > x) = 2 * (1 - Phi(sqrt(x))) = erfc(sqrt(x/2)),
// from the standard-normal relation Phi(z) = (1 + erf(z/sqrt(2))) / 2. That is
// a closed form over a stdlib function, which is why this package needs no
// statistics dependency and hand-rolls no series.
func chiSquaredUpperTail1DF(x float64) float64 {
	if x <= 0 {
		return 1
	}
	return math.Erfc(math.Sqrt(x / 2))
}
