package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultStreamAttempts is how many times a reply may be requested when its
// stream fails part way: the first request and two more.
//
// It is a separate, smaller budget than [Retry].MaxAttempts, which bounds the
// cheap failures (a 429, a refused connection) and can afford six sends over 23
// seconds. A stream that broke may have been billed, and a silent one has already
// cost its whole idle limit, so three sends is the most this is worth. A policy
// whose MaxAttempts is lower than this wins, and 1 turns every retry off.
const DefaultStreamAttempts = 3

// DefaultStreamIdleTimeout is how long a stream the provider opened may send no
// data before the attempt is ended as [ErrStreamSilent] (KOP-221).
//
// Three minutes is longer than any pause between two chunks of a working model on
// a provider kopicode pins, reasoning included (reasoning text arrives as data, and
// so does each token), and far shorter than the fifteen minutes two sessions hung
// for. It bounds the silence and not the reply: a stream that keeps sending is
// never cut off however long it runs. An endpoint that really is slower sets
// [WithStreamIdleTimeout].
const DefaultStreamIdleTimeout = 3 * time.Minute

// streamRetryable reports whether a stream failure is worth asking for the reply
// again: it went silent, its connection broke, or it ended before the provider
// said it was finished. A frame this build could not decode, a cancellation and a
// line over the size bound would all fail the same way again.
func streamRetryable(err error) bool {
	return errors.Is(err, ErrStreamSilent) ||
		errors.Is(err, ErrStreamBroken) ||
		errors.Is(err, ErrStreamIncomplete)
}

// streamCause names a stream failure for [RetryEvent].Cause, in the short form
// the request-level causes use ("http 429", "transport failure").
func streamCause(err error) string {
	switch {
	case errors.Is(err, ErrStreamSilent):
		return "stream silent"
	case errors.Is(err, ErrStreamBroken):
		return "stream broken"
	default:
		return "stream incomplete"
	}
}

// resilient is what a live [Client] puts inside the [Stream] it returns: the
// attempt being read, and the means to start another when it fails.
//
// The retry happens inside Next, and that placement is the safety argument. The
// loop acts on a reply only after the stream has been drained and Reply has
// returned it, so a failure seen here is by construction before any tool call
// from this reply was dispatched, and a reply whose calls ran is never asked for
// again. engine.Provider stays one method for the same reason it always has:
// the loop learns of a retry by reading the journal, not from anything Complete
// returns.
//
// What a person sees: deltas the failed attempt already delivered stay delivered,
// and the retry's deltas follow them. The journal's ProviderRetried event says
// where the restart happened, and the ProviderResponse that follows holds only the
// attempt that finished. The failed attempt's bytes are not kept; its cause, quoted
// whole in the event's detail, is.
type resilient struct {
	ctx      context.Context
	cur      *Stream
	resend   func(context.Context) (*Stream, error)
	attempts int
	delay    func(try int) time.Duration
	clock    Clock
	observe  RetryObserver
	log      *slog.Logger
	turn     int
	attempt  int

	tries int // sends made so far, the first included
	err   error
}

// next advances the current attempt, replacing it when it fails in a way that is
// worth another send and the budget allows.
func (r *resilient) next() bool {
	for {
		if r.err != nil {
			return false
		}
		if r.cur.step() {
			return true
		}
		err := r.cur.Err()
		if err == nil || !streamRetryable(err) || r.ctx.Err() != nil {
			return false
		}

		// tries is the sends made so far, the one that just failed included.
		if r.tries >= r.attempts {
			if r.attempts > 1 {
				r.err = fmt.Errorf("%w after %d attempt(s): %w", ErrRetriesExhausted, r.tries, err)
			}
			return false
		}

		delay := r.delay(r.tries - 1)
		cause := streamCause(err)
		r.log.LogAttrs(r.ctx, slog.LevelDebug, "provider stream failed, retrying",
			slog.Int("attempt", r.tries),
			slog.Int("of", r.attempts),
			slog.Duration("backoff", delay),
			slog.String("cause", cause),
		)
		if r.observe != nil {
			r.observe(r.ctx, RetryEvent{
				Turn: r.turn, Attempt: r.attempt,
				Try: r.tries, OfTries: r.attempts,
				Delay: delay, Cause: cause, Detail: err.Error(),
			})
		}
		_ = r.cur.Close() // the attempt already failed; closing releases its connection.
		if err := wait(r.ctx, r.clock, delay); err != nil {
			r.err = err
			return false
		}
		n, err := r.resend(r.ctx)
		if err != nil {
			r.err = err
			return false
		}
		r.cur = n
		r.tries++
	}
}

// failure is the error the stream ends with: the retry budget's, when it ran out,
// otherwise the current attempt's.
func (r *resilient) failure() error {
	if r.err != nil {
		return r.err
	}
	return r.cur.Err()
}
