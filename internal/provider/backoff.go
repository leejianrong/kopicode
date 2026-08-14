package provider

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"time"
)

// Clock is where a delay comes from.
//
// One method, and the same one procgroup.Clock declares, so a caller that
// already injected a clock for a timeout can pass the same value here rather
// than running the retry path off a second source of time that could disagree
// with the first. tools.RealClock and syntax.RealClock both satisfy it without
// either package importing this one.
type Clock interface {
	// NewTimer returns a channel that receives once d has elapsed, and a
	// function that releases the timer. The caller always calls it.
	NewTimer(d time.Duration) (<-chan time.Time, func())
}

// RealClock is the production clock.
type RealClock struct{}

// NewTimer wraps time.NewTimer.
func (RealClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Rand is the jitter source.
//
// It is one method so that *math/rand/v2.Rand satisfies it directly: the
// production client seeds one and a test injects either a seeded one, to assert
// the exact delays a given seed produces, or a scripted one, to drive the ends
// of the interval. Jitter that is not injected is jitter no test can assert, and
// "it slept for somewhere between nothing and eight seconds" is not an
// assertion.
type Rand interface {
	// Int64N returns a uniform value in [0, n). n is always positive.
	Int64N(n int64) int64
}

// globalRand is the default jitter source: math/rand/v2's top-level generator,
// which is seeded from the runtime and safe for concurrent use.
//
// A fixed seed would make every process back off in step, which is the herd the
// jitter exists to break up.
type globalRand struct{}

func (globalRand) Int64N(n int64) int64 { return rand.Int64N(n) }

// Retry is the retry policy: how many times a request may be sent, and the
// backoff between sends.
//
// # The formula
//
// Capped exponential backoff with full jitter, in the form AWS's "Exponential
// Backoff And Jitter" states it:
//
//	ceiling = min(Cap, Base * 2^n)      // n is the 0-based retry index
//	delay   = uniform random in [0, ceiling]
//
// Full jitter means the whole interval, down to and including zero. Half jitter
// — `ceiling/2 + rand(0, ceiling/2)` — is the variant that gets written by
// accident, and it is a different policy: it never retries promptly, so a client
// that was unlucky once stays behind a client that was not. The distinction is
// invisible without an assertion on the bounds, which is why [Retry.Delay] is a
// pure function of (n, Rand) and is tested at both ends of the interval.
//
// The ceiling doubles; the delay drawn from it does not. Two consecutive delays
// may well be 4s then 30ms, and that is the design rather than a bug in it.
type Retry struct {
	// MaxAttempts is how many times one request may be *sent*, first send
	// included. 1 disables retrying. Zero means [DefaultRetry]'s value.
	MaxAttempts int
	// Base is the first ceiling, doubled per retry.
	Base time.Duration
	// Cap bounds the ceiling however many retries have happened.
	Cap time.Duration
}

// DefaultRetry is the shipped policy.
//
// Five retries over six sends, with the ceiling saturating at 8s on the fourth
// one: 1s, 2s, 4s, 8s, 8s. The worst case is 23s of waiting and the expected
// case is half of it. The cap is reached inside the default attempt budget on
// purpose — a cap the default policy can never reach is a constant nobody has
// tested.
var DefaultRetry = Retry{
	MaxAttempts: 6,
	Base:        time.Second,
	Cap:         8 * time.Second,
}

// maxAttempts is the policy's attempt budget, or the default's.
func (r Retry) maxAttempts() int {
	if r.MaxAttempts <= 0 {
		return DefaultRetry.MaxAttempts
	}
	return r.MaxAttempts
}

func (r Retry) base() time.Duration {
	if r.Base <= 0 {
		return DefaultRetry.Base
	}
	return r.Base
}

func (r Retry) capped() time.Duration {
	if r.Cap <= 0 {
		return DefaultRetry.Cap
	}
	return r.Cap
}

// Ceiling is the longest delay retry n may draw: min(Cap, Base * 2^n), where n
// is the 0-based retry index.
//
// The doubling is a shift with an explicit guard rather than math.Pow: at n=63 a
// shift overflows int64 into a negative duration, and a negative ceiling is a
// delay of zero — a retry storm produced by the code that exists to prevent one.
func (r Retry) Ceiling(n int) time.Duration {
	base, limit := r.base(), r.capped()
	if n < 0 {
		n = 0
	}
	ceiling := base
	for range n {
		if ceiling >= limit || ceiling > limit/2 {
			return limit
		}
		ceiling *= 2
	}
	if ceiling > limit {
		return limit
	}
	return ceiling
}

// Delay draws the delay before retry n, 0-based, from rnd.
//
// The interval is closed at both ends: zero is a legal draw, and so is the
// ceiling itself.
func (r Retry) Delay(n int, rnd Rand) time.Duration {
	if rnd == nil {
		rnd = globalRand{}
	}
	ceiling := r.Ceiling(n)
	if ceiling <= 0 {
		return 0
	}
	// The interval is [0, ceiling], so the exclusive bound is one more than the
	// ceiling — unless the ceiling is already the largest duration there is, in
	// which case adding one wraps negative and Int64N panics on it. A cap of
	// MaxInt64 is not a sensible policy, and "not sensible" is not "may crash
	// the loop".
	bound := int64(ceiling)
	if bound < math.MaxInt64 {
		bound++
	}
	return time.Duration(rnd.Int64N(bound))
}

// retryableStatus reports whether an HTTP status is worth sending again.
//
// 429 and 5xx, and nothing else. A 4xx that is not 429 describes the request:
// 400 is a malformed body, 401 is the credential, 402 is the account, 404 is the
// model id — none of which a second identical send can change, so retrying one
// spends the attempt budget and the wall clock to arrive at the same answer with
// the failure reported later.
func retryableStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// wait blocks for d, or until ctx is done.
//
// The timer comes from the injected clock, so a test drives the delay rather
// than living through it. Cancellation is honoured *during* the wait and not
// only around it: a Ctrl-C while the client is backing off from a 429 has to
// return, not finish the sleep first.
func wait(ctx context.Context, clock Clock, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("provider: retry cancelled: %w", err)
	}
	if d <= 0 {
		return nil
	}
	c, stop := clock.NewTimer(d)
	defer stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("provider: retry cancelled: %w", ctx.Err())
	case <-c:
		return nil
	}
}
