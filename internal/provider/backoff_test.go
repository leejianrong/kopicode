package provider_test

import (
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/provider"
)

// manualClock is the clock the retry tests run on.
//
// It records every delay it is asked for and, by default, fires the timer at
// once. So a test asserts the durations the policy *chose* without living
// through them: a suite that actually slept 15 seconds to check a 15-second
// backoff would be either slow or shortened to a policy nobody ships.
type manualClock struct {
	mu     sync.Mutex
	asked  []time.Duration
	freeze bool
}

func (c *manualClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	c.asked = append(c.asked, d)
	freeze := c.freeze
	c.mu.Unlock()

	ch := make(chan time.Time, 1)
	if !freeze {
		ch <- time.Time{}
	}
	return ch, func() {}
}

// delays returns the durations the clock was asked to wait, in order.
func (c *manualClock) delays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.asked...)
}

// fixedRand answers every draw with the same fraction of the interval, and
// records the intervals it was handed.
//
// pick is given n — the exclusive bound the policy passed — so a test can drive
// the very top of the interval (n-1) and the very bottom (0) without knowing the
// ceiling arithmetic, which is the thing under test.
type fixedRand struct {
	pick   func(n int64) int64
	bounds []int64
}

func (r *fixedRand) Int64N(n int64) int64 {
	r.bounds = append(r.bounds, n)
	return r.pick(n)
}

func topOfInterval() *fixedRand    { return &fixedRand{pick: func(n int64) int64 { return n - 1 }} }
func bottomOfInterval() *fixedRand { return &fixedRand{pick: func(int64) int64 { return 0 }} }

// TestCeilingIsCappedExponential pins the ceiling schedule the delays are drawn
// from: base, doubling, saturating at the cap and staying there.
func TestCeilingIsCappedExponential(t *testing.T) {
	policy := provider.Retry{MaxAttempts: 6, Base: time.Second, Cap: 8 * time.Second}

	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		8 * time.Second,
		8 * time.Second,
	}
	for n, w := range want {
		if got := policy.Ceiling(n); got != w {
			t.Errorf("Ceiling(%d) = %s, want %s", n, got, w)
		}
	}
}

// TestDelayCoversTheWholeIntervalIncludingZero is the assertion that separates
// full jitter from the half-jitter variant that gets written by accident.
//
// Full jitter is uniform over [0, ceiling]. Half jitter is ceiling/2 +
// uniform(0, ceiling/2), which never returns a short delay — and the two are
// indistinguishable without an assertion at the ends of the interval.
func TestDelayCoversTheWholeIntervalIncludingZero(t *testing.T) {
	policy := provider.Retry{MaxAttempts: 6, Base: time.Second, Cap: 8 * time.Second}

	bottom := bottomOfInterval()
	if got := policy.Delay(3, bottom); got != 0 {
		t.Errorf("the bottom of the interval is %s, want 0 — full jitter includes an immediate retry", got)
	}

	top := topOfInterval()
	if got := policy.Delay(3, top); got != 8*time.Second {
		t.Errorf("the top of the interval is %s, want the ceiling 8s", got)
	}

	// The bound handed to the RNG says which interval was drawn from. 8s+1ns
	// means [0, 8s] inclusive at both ends.
	if len(top.bounds) != 1 || top.bounds[0] != int64(8*time.Second)+1 {
		t.Errorf("the RNG was asked for a value in [0, %v), want [0, %d)", top.bounds, int64(8*time.Second)+1)
	}
}

// TestSeededDelaysAreExactAndReproducible is the "assert the exact delays"
// half: a given seed produces a given schedule, and the same seed produces it
// again.
func TestSeededDelaysAreExactAndReproducible(t *testing.T) {
	policy := provider.Retry{MaxAttempts: 6, Base: time.Second, Cap: 8 * time.Second}

	draw := func() []time.Duration {
		rnd := rand.New(rand.NewPCG(1, 2))
		var out []time.Duration
		for n := range 5 {
			out = append(out, policy.Delay(n, rnd))
		}
		return out
	}

	first, second := draw(), draw()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("delay %d differs between two identically seeded runs: %s then %s\n"+
				"a jitter source that is not reproducible makes every retry assertion a guess",
				i, first[i], second[i])
		}
		if ceiling := policy.Ceiling(i); first[i] < 0 || first[i] > ceiling {
			t.Errorf("delay %d is %s, outside [0, %s]", i, first[i], ceiling)
		}
	}

	// A seeded run must reach the short half of the interval at least once
	// across a schedule this long, or the policy is not full jitter whatever its
	// bounds say.
	var sawShort bool
	for i, d := range first {
		if d < policy.Ceiling(i)/2 {
			sawShort = true
		}
	}
	if !sawShort {
		t.Errorf("no delay in %v fell in the lower half of its ceiling; that is half jitter, not full", first)
	}
}

// TestCeilingDoesNotOverflow guards the arithmetic rather than the policy.
//
// base << n overflows int64 at n=63 and comes back negative, and a negative
// ceiling is a zero delay — a retry storm produced by the code that exists to
// prevent one.
func TestCeilingDoesNotOverflow(t *testing.T) {
	policy := provider.Retry{MaxAttempts: 2, Base: time.Second, Cap: time.Duration(math.MaxInt64)}

	for _, n := range []int{62, 63, 64, 1000} {
		got := policy.Ceiling(n)
		if got <= 0 {
			t.Errorf("Ceiling(%d) = %s, want a positive duration", n, got)
		}
	}
	// A real generator, not a scripted one: Int64N panics on a bound that is
	// zero or negative, so an off-by-one at the top of the range crashes the
	// retry path rather than shortening it.
	if got := policy.Delay(1000, rand.New(rand.NewPCG(7, 9))); got < 0 {
		t.Errorf("Delay(1000) = %s, want a non-negative duration", got)
	}
}

// TestZeroPolicyFallsBackToTheDefault keeps a zero-valued Retry from meaning
// "never retry, instantly".
func TestZeroPolicyFallsBackToTheDefault(t *testing.T) {
	var zero provider.Retry
	if got, want := zero.Ceiling(0), provider.DefaultRetry.Base; got != want {
		t.Errorf("zero policy's first ceiling is %s, want the default %s", got, want)
	}
	if got, want := zero.Ceiling(9), provider.DefaultRetry.Cap; got != want {
		t.Errorf("zero policy saturates at %s, want the default cap %s", got, want)
	}
}
