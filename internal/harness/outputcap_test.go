package harness_test

import (
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// KOP-179. The cap changes what the model is shown, so it is part of the arm, but
// only when set: every registered arm shows everything and must keep its hash.
func TestOutputCapMovesTheHashOnlyWhenSet(t *testing.T) {
	cfg, ok := harness.ConfigByName(harness.DefaultConfigName)
	if !ok {
		t.Fatal("no default config")
	}
	off := cfg.Hash()
	if cfg.OutputCap != 0 {
		t.Fatalf("a registered arm has OutputCap %d; every benchmark arm must show the model everything", cfg.OutputCap)
	}
	cfg.OutputCap = 32768
	on := cfg.Hash()
	if on == off {
		t.Error("turning the cap on left the hash alone; the two would pool")
	}
	cfg.OutputCap = 8192
	if cfg.Hash() == on {
		t.Error("two different caps share a hash")
	}
	cfg.OutputCap = 0
	if cfg.Hash() != off {
		t.Error("setting the cap back to 0 did not restore the original hash")
	}
}

func TestOutputCapDefaultsOnInTheREPLOnly(t *testing.T) {
	userHome(t, "")
	dir := repoWith(t, "")
	n := func(v int) *int { return &v }

	cases := []struct {
		name string
		o    harness.Overrides
		want int
	}{
		{"headless and resident front ends show everything", harness.Overrides{}, 0},
		{"the REPL is capped", harness.Overrides{Interactive: true}, harness.DefaultOutputCap},
		{"the flag beats the REPL default", harness.Overrides{Interactive: true, OutputCap: n(4096)}, 4096},
		{"the flag can turn the REPL's off", harness.Overrides{Interactive: true, OutputCap: n(0)}, 0},
		{"the flag can turn it on headless", harness.Overrides{OutputCap: n(10000)}, 10000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := harness.Resolve(dir, tc.o)
			if err != nil {
				t.Fatal(err)
			}
			if sel.Config.OutputCap != tc.want {
				t.Errorf("OutputCap = %d, want %d", sel.Config.OutputCap, tc.want)
			}
			if sel.HarnessConfigHash != sel.Config.Hash() {
				t.Error("the selection's hash is not its configuration's hash")
			}
		})
	}
}
