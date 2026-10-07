package repl_test

import (
	"context"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

func fp(v float64) *float64 { return &v }

func TestContextReport(t *testing.T) {
	tests := []struct {
		name string
		u    engine.Usage
		want []string
		not  []string
	}{
		{
			name: "known window and cost",
			u: engine.Usage{
				TokenUsage:    engine.TokenUsage{Prompt: 300_000, Completion: 9_000, Total: 309_000, CacheRead: 120_000, CostUSD: fp(0.0123)},
				ContextTokens: 31_000, ContextWindow: 262_144, Requests: 12, Turns: 12, MaxTurns: 20, TokenBudget: 2_000_000,
			},
			want: []string{
				"31,000 of 262,144 tokens (11.8%) in the latest request",
				"309,000 tokens this session (prompt 300,000, completion 9,000), budget 2,000,000",
				"120,000 prompt tokens read from the provider's cache",
				"$0.0123, as reported by the provider",
				"12 so far; the cap is 20 per prompt",
			},
		},
		{
			name: "unknown window is said to be unknown",
			u:    engine.Usage{ContextTokens: 5_000, Requests: 1, TokenUsage: engine.TokenUsage{Total: 5_100}},
			want: []string{"5,000 tokens in the latest request (this model's window is not known)"},
			not:  []string{"%", " of "},
		},
		{
			name: "a cost with a gap is unknown, not partial",
			u:    engine.Usage{ContextTokens: 1, Requests: 2},
			want: []string{"cost     unknown: the provider did not report a price for every request"},
		},
		{
			name: "before any response nothing is measured",
			u:    engine.Usage{ContextWindow: 262_144},
			want: []string{"not measured yet"},
			not:  []string{"cost", "0.0%"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(repl.ContextReport(tc.u), "\n")
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("report lacks %q:\n%s", w, got)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(got, n) {
					t.Errorf("report contains %q, which it must not:\n%s", n, got)
				}
			}
		})
	}
}

// TestSlashContextPrintsTheReportWithoutRunningATurn. /context is the loop's
// own command: it must never reach the model as a prompt.
func TestSlashContextPrintsTheReportWithoutRunningATurn(t *testing.T) {
	var out strings.Builder
	turns := 0
	loop, err := repl.New(repl.Config{
		In:  strings.NewReader("/context\n/exit\n"),
		Out: &out,
		Usage: func() engine.Usage {
			return engine.Usage{ContextTokens: 1234, ContextWindow: 10_000, Requests: 1, Turns: 3, MaxTurns: 20}
		},
		Turn: func(context.Context, string, repl.Surface) (engine.Result, error) {
			turns++
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if turns != 0 {
		t.Errorf("/context ran %d turns, want 0", turns)
	}
	if !strings.Contains(out.String(), "1,234 of 10,000 tokens (12.3%)") {
		t.Errorf("the report was not printed:\n%s", out.String())
	}
}

func TestSlashContextWithNoUsageSaysSo(t *testing.T) {
	var out strings.Builder
	loop, err := repl.New(repl.Config{
		In:  strings.NewReader("/context\n"),
		Out: &out,
		Turn: func(context.Context, string, repl.Surface) (engine.Result, error) {
			return engine.Result{Stop: engine.StopCompleted}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = loop.Run(context.Background())
	if !strings.Contains(out.String(), "not available") {
		t.Errorf("no usage source and no explanation:\n%s", out.String())
	}
}
