// Package engine is the agent loop and the interfaces it drives.
//
// The loop itself — turn bounds, context assembly, tool dispatch, journaling —
// is KAN-789 and is not here yet. What is here is the seam the loop consumes
// the model provider through, declared before the loop so that the replay
// provider (KAN-773) and the real OpenRouter client (KAN-776) are written
// against one interface rather than two that later have to be reconciled.
package engine

import (
	"context"

	"github.com/leejianrong/kopicode/internal/provider"
)

// Provider is the model provider, as the loop consumes it.
//
// It is declared here and not in internal/provider because an interface belongs
// where it is used: the engine is the consumer, so the engine says what it
// needs and each implementation satisfies it structurally without importing
// this package. That is the same rule parse.Tools follows, and it is what keeps
// the dependency arrow pointing one way — engine depends on provider's data
// types, and provider depends on nothing of the engine's.
//
// One method, because one is all the loop needs. Retries, backoff and provider
// pinning are the implementation's business (docs/SLICE-1.md §Build Plan step
// 8): a loop that could see them would start making policy out of them.
type Provider interface {
	// Complete sends one request and returns the reply as a stream.
	//
	// The returned stream is pulled by the caller and must be closed. An error
	// here is a request that never produced a reply — a transport failure, a
	// non-success status, or a replay that has nothing to serve. A reply that
	// arrived and then failed part way through is reported by the stream
	// instead, because the deltas that did arrive are evidence and discarding
	// them would lose it.
	//
	// Cancelling ctx cancels the reply mid-stream. The stream stops at its next
	// increment with an error wrapping context.Canceled, which is what Ctrl-C
	// during a long reply has to do.
	Complete(ctx context.Context, req provider.Request) (*provider.Stream, error)
}
