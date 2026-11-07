package harness_test

import (
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// ADR-0042. Limiting verification to a changed tree changes what the model is
// shown, so it is part of the arm, but only when set: every registered arm keeps
// its hash.
func TestVerifyChangedOnlyMovesTheHashOnlyWhenSet(t *testing.T) {
	cfg, ok := harness.ConfigByName(harness.DefaultConfigName)
	if !ok {
		t.Fatal("no default config")
	}
	if cfg.VerifyChangedOnly {
		t.Fatal("a registered arm has VerifyChangedOnly on; every benchmark arm must keep its hash")
	}
	off := cfg.Hash()
	cfg.VerifyChangedOnly = true
	if cfg.Hash() == off {
		t.Error("turning it on left the hash alone; the two would pool")
	}
	cfg.VerifyChangedOnly = false
	if cfg.Hash() != off {
		t.Error("turning it back off did not restore the original hash")
	}
}

func TestDeclaredVerifyChangedOnlyIsZeroOrOne(t *testing.T) {
	base := "base = \"" + harness.DefaultConfigName + "\"\n"
	on, err := harness.LoadDeclaredConfig(writeDeclared(t, base+"verify_changed_only = 1\n"))
	if err != nil || !on.VerifyChangedOnly {
		t.Fatalf("verify_changed_only = 1: err %v, on %v", err, on.VerifyChangedOnly)
	}
	off, err := harness.LoadDeclaredConfig(writeDeclared(t, base+"verify_changed_only = 0\n"))
	if err != nil || off.VerifyChangedOnly {
		t.Fatalf("verify_changed_only = 0: err %v, on %v", err, off.VerifyChangedOnly)
	}
	if _, err := harness.LoadDeclaredConfig(writeDeclared(t, base+"verify_changed_only = 2\n")); err == nil ||
		!strings.Contains(err.Error(), "0 (off) or 1 (on)") {
		t.Errorf("verify_changed_only = 2: err %v, want the 0-or-1 message", err)
	}
}

func TestTheVerifyChangedOnlyFlagTurnsItOn(t *testing.T) {
	sel, err := harness.Resolve(t.TempDir(), harness.Overrides{VerifyChangedOnly: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !sel.Config.VerifyChangedOnly {
		t.Error("the flag did not set VerifyChangedOnly")
	}
}
