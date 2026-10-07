package harness_test

import (
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

func resolveLimits(t *testing.T, o harness.Overrides) harness.Selection {
	t.Helper()
	sel, err := harness.Resolve(t.TempDir(), o)
	if err != nil {
		t.Fatalf("Resolve(%+v): %v", o, err)
	}
	return sel
}

func TestTheCorpusDefaultStaysTwenty(t *testing.T) {
	sel := resolveLimits(t, harness.Overrides{})
	if sel.Config.MaxTurns != 20 || sel.Config.TokenBudget != 2_000_000 {
		t.Fatalf("default limits = %d turns, %d tokens; the corpus arm must not move",
			sel.Config.MaxTurns, sel.Config.TokenBudget)
	}
}

func TestInteractiveRaisesTheCapAndMovesTheHash(t *testing.T) {
	base := resolveLimits(t, harness.Overrides{})
	sel := resolveLimits(t, harness.Overrides{Interactive: true})
	if sel.Config.MaxTurns != harness.InteractiveMaxTurns {
		t.Fatalf("interactive cap = %d, want %d", sel.Config.MaxTurns, harness.InteractiveMaxTurns)
	}
	if sel.HarnessConfigHash == base.HarnessConfigHash || sel.HarnessConfigHash != sel.Config.Hash() {
		t.Fatalf("hash must follow the amended config: %s vs %s", sel.HarnessConfigHash, base.HarnessConfigHash)
	}
}

func TestAnExplicitCapBeatsTheInteractiveDefault(t *testing.T) {
	sel := resolveLimits(t, harness.Overrides{Interactive: true, MaxTurns: 7})
	if sel.Config.MaxTurns != 7 {
		t.Fatalf("cap = %d, want 7", sel.Config.MaxTurns)
	}
}

func TestTokenBudgetOverride(t *testing.T) {
	zero, big := 0, 5_000_000
	if got := resolveLimits(t, harness.Overrides{TokenBudget: &zero}).Config.TokenBudget; got != 0 {
		t.Fatalf("explicit 0 must mean unbounded, got %d", got)
	}
	if got := resolveLimits(t, harness.Overrides{TokenBudget: &big}).Config.TokenBudget; got != big {
		t.Fatalf("budget = %d, want %d", got, big)
	}
}

func TestBadLimitsAreUsageErrors(t *testing.T) {
	neg := -1
	for name, o := range map[string]harness.Overrides{
		"negative turns":  {MaxTurns: -3},
		"negative budget": {TokenBudget: &neg},
	} {
		_, err := harness.Resolve(t.TempDir(), o)
		if err == nil || !harness.IsUsageError(err) {
			t.Errorf("%s: err = %v, want a usage error", name, err)
		}
	}
}
