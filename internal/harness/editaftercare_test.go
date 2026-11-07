package harness_test

import (
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// ADR-0040. The edit-aftercare note changes what the model is shown, so it is part
// of the arm, but only when set: every registered arm keeps its hash.
func TestEditAftercareMovesTheHashOnlyWhenSet(t *testing.T) {
	cfg, ok := harness.ConfigByName(harness.DefaultConfigName)
	if !ok {
		t.Fatal("no default config")
	}
	if cfg.EditAftercare {
		t.Fatal("a registered arm has EditAftercare on; every benchmark arm must keep its hash")
	}
	off := cfg.Hash()
	cfg.EditAftercare = true
	if cfg.Hash() == off {
		t.Error("turning the note on left the hash alone; the two would pool")
	}
	cfg.EditAftercare = false
	if cfg.Hash() != off {
		t.Error("turning it back off did not restore the original hash")
	}
}

func TestDeclaredEditAftercareIsZeroOrOne(t *testing.T) {
	on, err := harness.LoadDeclaredConfig(writeDeclared(t, "base = \""+harness.DefaultConfigName+"\"\nedit_aftercare = 1\n"))
	if err != nil || !on.EditAftercare {
		t.Fatalf("edit_aftercare = 1: err %v, EditAftercare %v", err, on.EditAftercare)
	}
	off, err := harness.LoadDeclaredConfig(writeDeclared(t, "base = \""+harness.DefaultConfigName+"\"\nedit_aftercare = 0\n"))
	if err != nil || off.EditAftercare {
		t.Fatalf("edit_aftercare = 0: err %v, EditAftercare %v", err, off.EditAftercare)
	}
	if _, err := harness.LoadDeclaredConfig(writeDeclared(t, "base = \""+harness.DefaultConfigName+"\"\nedit_aftercare = 2\n")); err == nil ||
		!strings.Contains(err.Error(), "0 (off) or 1 (on)") {
		t.Errorf("edit_aftercare = 2: err %v, want the 0-or-1 message", err)
	}
}

func TestTheEditAftercareFlagTurnsItOn(t *testing.T) {
	sel, err := harness.Resolve(t.TempDir(), harness.Overrides{EditAftercare: true})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !sel.Config.EditAftercare {
		t.Error("the flag did not set EditAftercare")
	}
}
