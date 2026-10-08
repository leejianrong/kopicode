package harness_test

import (
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// KAN-1971. The stall detector changes what the model is told, so a threshold is
// part of the arm, but only when set: every registered arm has none and must keep
// the hash every recorded result was filed under.
func TestStallThresholdMovesTheHashOnlyWhenSet(t *testing.T) {
	cfg, ok := harness.ConfigByName(harness.DefaultConfigName)
	if !ok {
		t.Fatal("no default config")
	}
	off := cfg.Hash()
	if cfg.StallThreshold != 0 {
		t.Fatalf("a registered arm has StallThreshold %d; every benchmark arm must keep the detector off", cfg.StallThreshold)
	}
	cfg.StallThreshold = 3
	on := cfg.Hash()
	if on == off {
		t.Error("turning the detector on left the hash alone; the two would pool")
	}
	cfg.StallThreshold = 5
	if cfg.Hash() == on {
		t.Error("two different thresholds share a hash")
	}
	cfg.StallThreshold = 0
	if cfg.Hash() != off {
		t.Error("setting the threshold back to 0 did not restore the original hash")
	}
}

func TestStallThresholdDefaultsOnInTheREPLOnly(t *testing.T) {
	userHome(t, "")
	dir := repoWith(t, "")
	n := func(v int) *int { return &v }

	cases := []struct {
		name string
		o    harness.Overrides
		want int
	}{
		{"headless and resident front ends are off", harness.Overrides{}, 0},
		{"the REPL is on", harness.Overrides{Interactive: true}, harness.DefaultStallThreshold},
		{"the flag beats the REPL default", harness.Overrides{Interactive: true, StallThreshold: n(5)}, 5},
		{"the flag can turn the REPL's off", harness.Overrides{Interactive: true, StallThreshold: n(0)}, 0},
		{"the flag can turn it on headless", harness.Overrides{StallThreshold: n(4)}, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := harness.Resolve(dir, tc.o)
			if err != nil {
				t.Fatal(err)
			}
			if sel.Config.StallThreshold != tc.want {
				t.Errorf("StallThreshold = %d, want %d", sel.Config.StallThreshold, tc.want)
			}
			if sel.HarnessConfigHash != sel.Config.Hash() {
				t.Error("the selection's hash is not its configuration's hash")
			}
		})
	}

	custom, err := harness.Resolve(dir, harness.Overrides{Interactive: true, Model: "m", ProviderURL: "http://localhost:1/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if custom.Config.StallThreshold != harness.DefaultStallThreshold {
		t.Errorf("a custom endpoint in the REPL has threshold %d; the REPL default applies there too", custom.Config.StallThreshold)
	}
}
