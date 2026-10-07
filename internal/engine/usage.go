package engine

import (
	"sync"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
)

// TokenUsage is what the provider reported for one request, or the sum over
// several. It is the engine's own type for the reason [Event] is: a front end
// may not import internal/journal or internal/provider, so the numbers cross as
// plain fields.
type TokenUsage struct {
	Prompt     int
	Completion int
	Total      int
	// CacheRead and CacheWrite are subsets of Prompt, zero when the route does
	// not report a cache split.
	CacheRead  int
	CacheWrite int
	// CostUSD is the provider-reported price, or nil when it is unknown. For a
	// sum it is non-nil only if *every* request in it reported one: a total
	// that silently skipped the requests with no price would read as a price.
	CostUSD *float64
}

// Usage is a session's measure of itself, as of now.
//
// The two numbers people mean by "how much has this used" are different and
// both are here. ContextTokens is the prompt of the most recent request: what
// the model is holding, and so what runs into its window. The embedded
// [TokenUsage] is the sum over every request, which is what was billed and what
// the token budget counts; it grows much faster, because each request resends
// the whole history, and it says nothing about how near the window is.
type Usage struct {
	TokenUsage

	// ContextTokens is the latest request's prompt tokens. Zero until the
	// session has had a response — see Requests — which is "not measured", not
	// "empty".
	ContextTokens int
	// ContextWindow is the model's context size in tokens, or 0 when this
	// binary does not know it. It is never estimated.
	ContextWindow int
	// Requests is the number of provider responses that have reported usage in
	// this process.
	Requests int

	// Turns is the session-wide turn count; MaxTurns is the per-exchange cap and
	// TokenBudget the whole session's, both from the harness configuration.
	Turns       int
	MaxTurns    int
	TokenBudget int
}

// meter is the part of Usage that is not already the budget's own running sum.
//
// Engine is single-threaded over its own turn, but a resident front end asks
// for usage while a turn is running (serve's session.usage), so every write to
// the usage numbers and every read of them for a report goes through mu. The
// loop's own read of the budget does not: it runs on the writer's goroutine.
type meter struct {
	mu         sync.Mutex
	turn       int
	lastPrompt int
	requests   int
	// cost is the sum of reported costs and costed counts the requests that
	// reported one, so a sum is only offered when costed == requests.
	cost   float64
	costed int
}

// recordSpend folds one response's usage into the budget's running sum and the
// meter. It runs after the response is journaled.
func (e *Engine) recordSpend(turn int, r provider.Reply) {
	e.meter.mu.Lock()
	defer e.meter.mu.Unlock()
	e.spent.Prompt += r.Usage.Prompt
	e.spent.Completion += r.Usage.Completion
	e.spent.Total += r.Usage.Total
	e.spent.CacheRead += r.Usage.CacheRead
	e.spent.CacheWrite += r.Usage.CacheWrite
	e.meter.turn = turn
	e.meter.requests++
	e.meter.lastPrompt = r.Usage.Prompt
	if r.Cost != nil {
		e.meter.cost += *r.Cost
		e.meter.costed++
	}
}

// Usage reports the session's usage so far.
func (e *Engine) Usage() Usage {
	e.meter.mu.Lock()
	defer e.meter.mu.Unlock()
	u := Usage{
		TokenUsage: TokenUsage{
			Prompt:     e.spent.Prompt,
			Completion: e.spent.Completion,
			Total:      e.spent.Total,
			CacheRead:  e.spent.CacheRead,
			CacheWrite: e.spent.CacheWrite,
		},
		ContextTokens: e.meter.lastPrompt,
		ContextWindow: e.cfg.Selection.ContextWindow,
		Requests:      e.meter.requests,
		Turns:         e.meter.turn,
		MaxTurns:      e.cfg.Selection.Config.MaxTurns,
		TokenBudget:   e.cfg.Selection.Config.TokenBudget,
	}
	if e.meter.requests > 0 && e.meter.costed == e.meter.requests {
		c := e.meter.cost
		u.CostUSD = &c
	}
	return u
}

// usageOf renders one journaled response for an [Event].
func usageOf(p journal.ProviderResponse) *TokenUsage {
	return &TokenUsage{
		Prompt:     p.Tokens.Prompt,
		Completion: p.Tokens.Completion,
		Total:      p.Tokens.Total,
		CacheRead:  p.Tokens.CacheRead,
		CacheWrite: p.Tokens.CacheWrite,
		CostUSD:    p.CostUSD,
	}
}
