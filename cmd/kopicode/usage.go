package main

import "github.com/leejianrong/kopicode/internal/engine"

// recordUsage is one provider response's usage on the event stream, nested
// under `usage` on a provider_response record. `size` on the same record is
// still the total, for a consumer that predates the split.
type recordUsage struct {
	Prompt     int      `json:"prompt"`
	Completion int      `json:"completion"`
	Total      int      `json:"total"`
	CacheRead  int      `json:"cache_read,omitempty"`
	CacheWrite int      `json:"cache_write,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
}

func recordUsageOf(t *engine.TokenUsage) *recordUsage {
	if t == nil {
		return nil
	}
	return &recordUsage{
		Prompt: t.Prompt, Completion: t.Completion, Total: t.Total,
		CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, CostUSD: t.CostUSD,
	}
}

// usageSummary is a session's usage so far, as `usage` on a turn result and as
// session.usage's result. Optional fields are absent, never zero-filled, when
// the answer is "unknown": a zero context_window or a zero cost_usd would be a
// claim.
//
// context_tokens is the latest request's prompt, the size of what the model is
// holding; prompt, completion and the rest are cumulative over the session.
// The two are not interchangeable and a client deciding when to checkpoint
// wants the first.
type usageSummary struct {
	// ContextTokens is 0 until the session has had a response (requests is 0).
	ContextTokens int `json:"context_tokens"`
	// ContextWindow is the model's window, absent when this binary does not
	// know it. Never estimated.
	ContextWindow int `json:"context_window,omitempty"`

	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
	// CostUSD is the sum of provider-reported costs, present only when every
	// request reported one.
	CostUSD *float64 `json:"cost_usd,omitempty"`

	Requests int `json:"requests"`
	Turns    int `json:"turns"`
	// MaxTurns is the cap on one prompt's turns; TokenBudget caps the whole
	// session's total.
	MaxTurns    int `json:"max_turns"`
	TokenBudget int `json:"token_budget"`
}

func usageSummaryOf(u engine.Usage) *usageSummary {
	return &usageSummary{
		ContextTokens: u.ContextTokens, ContextWindow: u.ContextWindow,
		Prompt: u.Prompt, Completion: u.Completion, Total: u.Total,
		CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CostUSD: u.CostUSD,
		Requests: u.Requests, Turns: u.Turns, MaxTurns: u.MaxTurns, TokenBudget: u.TokenBudget,
	}
}
