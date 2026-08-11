package bench

import (
	"math"
	"testing"
)

// The chi-squared upper tail is the one piece of arithmetic here that is not
// integer, so it gets pinned directly against published tables rather than
// only through the scorer. It is an internal test because the function is an
// implementation detail: the package's contract is Result.P, and exporting a
// tail function so a test can reach it would make a published statistics table
// part of the API.

// TestChiSquaredUpperTail1DFMatchesPublishedCriticalValues checks the tail at
// the standard critical values for 1 degree of freedom. The values are the
// textbook ones — NIST/SEMATECH e-Handbook of Statistical Methods, table
// 1.3.6.7.4, and identically Abramowitz & Stegun table 26.8 — quoted to six
// decimals, which is what sets the tolerance below.
func TestChiSquaredUpperTail1DFMatchesPublishedCriticalValues(t *testing.T) {
	tests := []struct {
		name      string
		statistic float64
		p         float64
	}{
		{"upper 10 percent point", 2.705543, 0.10},
		{"upper 5 percent point", 3.841459, 0.05},
		{"upper 2.5 percent point", 5.023886, 0.025},
		{"upper 1 percent point", 6.634897, 0.01},
		{"upper 0.5 percent point", 7.879439, 0.005},
		{"upper 0.1 percent point", 10.827566, 0.001},
	}

	// The critical values are published to six decimals. Propagating that
	// through the tail's derivative gives an error well under 1e-6 relative
	// at every row, so 1e-5 is slack for the quotation and nothing else.
	const tol = 1e-5

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chiSquaredUpperTail1DF(tc.statistic)
			if rel := math.Abs(got-tc.p) / tc.p; rel > tol {
				t.Errorf("chiSquaredUpperTail1DF(%v) = %v, want %v (relative error %v)",
					tc.statistic, got, tc.p, rel)
			}
		})
	}
}

// TestChiSquaredUpperTail1DFMatchesTheTwoSidedNormalTail is the same function
// read the other way. On 1 degree of freedom, P(X > z^2) is the two-sided
// standard normal tail P(|Z| > z), so the well-known normal-table values pin it
// from a second published source.
func TestChiSquaredUpperTail1DFMatchesTheTwoSidedNormalTail(t *testing.T) {
	tests := []struct {
		z float64
		p float64
	}{
		{1, 0.3173105},    // P(|Z| > 1)
		{2, 0.0455003},    // P(|Z| > 2)
		{3, 0.0026998},    // P(|Z| > 3)
		{1.96, 0.0499958}, // P(|Z| > 1.96), the 5% two-sided point to 6 s.f.
	}

	const tol = 1e-5

	for _, tc := range tests {
		got := chiSquaredUpperTail1DF(tc.z * tc.z)
		if rel := math.Abs(got-tc.p) / tc.p; rel > tol {
			t.Errorf("chiSquaredUpperTail1DF(%v^2) = %v, want P(|Z| > %v) = %v (relative error %v)",
				tc.z, got, tc.z, tc.p, rel)
		}
	}
}

// TestChiSquaredUpperTail1DFAtZero holds the boundary the continuity correction
// produces whenever |b-c| <= 1: a statistic of 0 is a p-value of exactly 1, not
// NaN and not a number just under 1.
func TestChiSquaredUpperTail1DFAtZero(t *testing.T) {
	// math.Copysign is how you get a real negative zero; the literal -0.0 is
	// just 0.0 in Go.
	for _, x := range []float64{0, math.Copysign(0, -1), -1} {
		if got := chiSquaredUpperTail1DF(x); got != 1 {
			t.Errorf("chiSquaredUpperTail1DF(%v) = %v, want 1", x, got)
		}
	}
}

// TestExactTwoSidedBinomialPIsExact checks the exact branch against the
// fractions it is defined by. Every expectation is an integer over a power of
// two, written with the binomial coefficients spelled out so it can be checked
// by hand; these agree with R's binom.test(min(b,c), b+c, 0.5).
func TestExactTwoSidedBinomialPIsExact(t *testing.T) {
	tests := []struct {
		b, c int
		want float64
		why  string
	}{
		{1, 0, 2 * 1.0 / 2.0, "2 * C(1,0)/2^1 = 1"},
		{6, 0, 2 * 1.0 / 64.0, "2 * C(6,0)/2^6 = 2/64 = 0.03125"},
		{0, 6, 2 * 1.0 / 64.0, "symmetric in b and c"},
		{5, 1, 2 * 7.0 / 64.0, "C(6,0)+C(6,1) = 1+6 = 7; 14/64 = 0.21875"},
		{9, 1, 2 * 11.0 / 1024.0, "C(10,0)+C(10,1) = 11; 22/1024 = 0.021484375"},
		{8, 2, 2 * 56.0 / 1024.0, "C(10,0..2) = 1+10+45 = 56; 112/1024 = 0.109375"},
		{7, 3, 2 * 176.0 / 1024.0, "C(10,0..3) = 1+10+45+120 = 176; 352/1024 = 0.34375"},
		{5, 5, 1, "2 * 638/1024 = 1.246..., capped at 1"},
		{20, 4, 2 * 12951.0 / 16777216.0, "C(24,0..4) = 1+24+276+2024+10626 = 12951"},
		{24, 0, 2 * 1.0 / 16777216.0, "2 * C(24,0)/2^24"},
	}

	for _, tc := range tests {
		got := exactTwoSidedBinomialP(tc.b, tc.c)
		if got != tc.want {
			t.Errorf("exactTwoSidedBinomialP(%d, %d) = %v, want %v exactly (%s)",
				tc.b, tc.c, got, tc.want, tc.why)
		}
	}
}

// TestCorrectedChiSquaredClampsAtOne holds the clamp in Edwards' correction.
// Squaring |b-c| - 1 without it turns b = c into a *larger* statistic than
// |b-c| = 1, which reads as evidence of a difference the data does not contain.
func TestCorrectedChiSquaredClampsAtOne(t *testing.T) {
	tests := []struct {
		b, c int
		want float64
	}{
		{20, 20, 0},               // |b-c| = 0
		{21, 20, 0},               // |b-c| = 1: (1-1)^2 = 0
		{20, 21, 0},               // and mirrored
		{22, 20, 1.0 / 42.0},      // (2-1)^2 / 42
		{20, 22, 1.0 / 42.0},      // and mirrored
		{66, 34, 961.0 / 100.0},   // (32-1)^2 / 100 = 9.61
		{101, 0, 10000.0 / 101.0}, // (101-1)^2 / 101
	}

	for _, tc := range tests {
		if got := correctedChiSquared(tc.b, tc.c); got != tc.want {
			t.Errorf("correctedChiSquared(%d, %d) = %v, want %v", tc.b, tc.c, got, tc.want)
		}
	}
}
