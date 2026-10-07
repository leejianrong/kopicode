package engine_test

import (
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

func f64(v float64) *float64 { return &v }

// TestUsageSeparatesTheContextInUseFromTheSumSpent. The context in use is the
// last request's prompt; the sum is what was billed. Reporting only the sum
// would make a healthy session look nearly full.
func TestUsageSeparatesTheContextInUseFromTheSumSpent(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("c1", "read_file", `{"path":"internal/greet/greet.go"}`)},
			usage: wireUsage{Prompt: 1000, Completion: 50, Total: 1050,
				Cost: f64(0.001), Details: &wireUsageDetails{Cached: 800, CacheWrite: 100}}},
		{text: "Done.", usage: wireUsage{Prompt: 1500, Completion: 20, Total: 1520,
			Cost: f64(0.002), Details: &wireUsageDetails{Cached: 1000}}},
	}
	s, _ := openWithProvider(t, script(t, replies, oneAttemptPerTurn(2)), engine.Options{})
	if _, err := s.Run(t.Context(), "look at greet.go"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	u := s.Usage()
	if u.ContextTokens != 1500 {
		t.Errorf("ContextTokens = %d, want 1500 — the latest request's prompt, not the sum", u.ContextTokens)
	}
	if u.Prompt != 2500 || u.Completion != 70 || u.Total != 2570 {
		t.Errorf("sum = %d/%d/%d, want 2500/70/2570", u.Prompt, u.Completion, u.Total)
	}
	if u.CacheRead != 1800 || u.CacheWrite != 100 {
		t.Errorf("cache = read %d write %d, want 1800 and 100", u.CacheRead, u.CacheWrite)
	}
	if u.CostUSD == nil || *u.CostUSD < 0.00299 || *u.CostUSD > 0.00301 {
		t.Errorf("CostUSD = %v, want 0.003 — the sum of what the provider reported", u.CostUSD)
	}
	if u.Requests != 2 || u.Turns != 2 {
		t.Errorf("requests = %d, turns = %d, want 2 and 2", u.Requests, u.Turns)
	}
	if u.MaxTurns <= 0 || u.TokenBudget <= 0 {
		t.Errorf("bounds = %d turns, %d tokens, want both reported", u.MaxTurns, u.TokenBudget)
	}
}

// TestACostWithAGapIsUnknownNotPartial. One request that reported no price
// makes the total unknown: a sum that skipped it would read as a price.
func TestACostWithAGapIsUnknownNotPartial(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("c1", "read_file", `{"path":"internal/greet/greet.go"}`)},
			usage: wireUsage{Prompt: 10, Completion: 5, Total: 15, Cost: f64(0.001)}},
		{text: "Done.", usage: wireUsage{Prompt: 20, Completion: 5, Total: 25}},
	}
	s, _ := openWithProvider(t, script(t, replies, oneAttemptPerTurn(2)), engine.Options{})
	if _, err := s.Run(t.Context(), "look"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if u := s.Usage(); u.CostUSD != nil {
		t.Errorf("CostUSD = %v, want nil: one of two requests reported no cost", *u.CostUSD)
	}
}

// TestNoRequestsMeansNothingMeasured. Before the first response the context is
// 0 and the cost is unknown rather than zero.
func TestNoRequestsMeansNothingMeasured(t *testing.T) {
	replies := []scriptedReply{{text: "unused", usage: wireUsage{Prompt: 1, Completion: 1, Total: 2}}}
	s, _ := openWithProvider(t, script(t, replies, oneAttemptPerTurn(1)), engine.Options{})
	u := s.Usage()
	if u.Requests != 0 || u.ContextTokens != 0 || u.CostUSD != nil {
		t.Errorf("fresh session usage = %+v, want nothing measured", u)
	}
}

// TestProviderResponseEventCarriesTheSplit. A surface reads the split off the
// event, and the journal records the same numbers.
func TestProviderResponseEventCarriesTheSplit(t *testing.T) {
	var seen []engine.Event
	replies := []scriptedReply{
		{text: "Hi.", usage: wireUsage{Prompt: 100, Completion: 10, Total: 110,
			Cost: f64(0.0005), Details: &wireUsageDetails{Cached: 64}}},
	}
	s, _ := openWithProvider(t, script(t, replies, oneAttemptPerTurn(1)), engine.Options{
		Events: func(ev engine.Event) { seen = append(seen, ev) },
	})
	if _, err := s.Run(t.Context(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var got *engine.TokenUsage
	for _, ev := range seen {
		if ev.Kind == engine.EventProviderResponse {
			got = ev.Tokens
		}
	}
	if got == nil {
		t.Fatal("no provider_response event carried Tokens")
	}
	if got.Prompt != 100 || got.Completion != 10 || got.CacheRead != 64 || got.CostUSD == nil || *got.CostUSD != 0.0005 {
		t.Errorf("event tokens = %+v, want 100/10, cache 64, cost 0.0005", got)
	}

	if err := s.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	resp := sole[journal.ProviderResponse](t, readJournal(t, s.Path()))
	if resp.Tokens.CacheRead != 64 || resp.CostUSD == nil {
		t.Errorf("journal response = %+v, want the cache split and the cost recorded", resp)
	}
}
