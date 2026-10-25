package engine_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

func TestValidateNewSessionID(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(journal.SessionDir(dir, "taken"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journal.SessionDir(dir, "taken"), journal.EventsFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"cuttlefish-3f2a", "a", "run_1.2", strings.Repeat("a", 128)} {
		if err := engine.ValidateNewSessionID(dir, id); err != nil {
			t.Errorf("ValidateNewSessionID(%q) = %v, want nil", id, err)
		}
	}
	for _, id := range []string{"", "../x", "a/b", `a\b`, ".hidden", "-x", "a b", "..", strings.Repeat("a", 129), "é"} {
		if err := engine.ValidateNewSessionID(dir, id); err == nil {
			t.Errorf("ValidateNewSessionID(%q) = nil, want a refusal: it is not a safe directory name", id)
		}
	}
	err := engine.ValidateNewSessionID(dir, "taken")
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("an id that already has a record: %v, want the refusal to say so", err)
	}
}

func TestValidateNewSessionIDWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if err := engine.ValidateNewSessionID(dir, "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".kopicode")); !os.IsNotExist(err) {
		t.Errorf("a check created %s", filepath.Join(dir, ".kopicode"))
	}
}
