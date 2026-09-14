package harness_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// TestResolveDeclaredConfigFlag drives --harness-config through Resolve: a
// declared config named at the flag rung overrides the built-in harness the
// file or registry would supply, its resolved name carries the declared prefix
// so it cannot pool with a built-in, and the model axis still resolves on its
// own (a declared config changes the harness, not the model or the pin).
func TestResolveDeclaredConfigFlag(t *testing.T) {
	absDeclared := writeDeclared(t, "base = \"default\"\nmax_turns = 40\n")

	sel, err := harness.Resolve(t.TempDir(), harness.Overrides{HarnessConfig: absDeclared})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if sel.Config.Name != harness.DeclaredConfigNamePrefix+"default" {
		t.Errorf("harness name = %q, want a declared:default name", sel.Config.Name)
	}
	if sel.Config.MaxTurns != 40 {
		t.Errorf("max_turns = %d, want the overridden 40", sel.Config.MaxTurns)
	}
	if sel.HarnessSource != harness.SourceFlag {
		t.Errorf("harness source = %q, want %q", sel.HarnessSource, harness.SourceFlag)
	}
	if sel.HarnessConfigPath != absDeclared {
		t.Errorf("harness config path = %q, want %q", sel.HarnessConfigPath, absDeclared)
	}

	// The model and pin are unchanged: a declared config is the harness axis only.
	entry, _ := harness.Lookup(harness.DefaultModelID)
	if sel.ModelID != entry.ModelID {
		t.Errorf("model = %q, want the default %q", sel.ModelID, entry.ModelID)
	}
	if sel.Pin.String() != entry.Pin.String() {
		t.Errorf("pin = %q, want the default model's %q", sel.Pin.String(), entry.Pin.String())
	}

	// The hash must differ from the built-in it is based on, or a tuned run
	// could pool with a published number (ADR-0010 decision 3).
	base, _ := harness.ConfigByName("default")
	if sel.HarnessConfigHash == base.Hash() {
		t.Error("declared config hashes identically to its built-in base; it would pool with it")
	}
}

// TestResolveDeclaredConfigRelativePath resolves a --harness-config path
// against the session dir, so `--harness-config harness.toml` finds the file in
// the working tree rather than the process's accidental cwd.
func TestResolveDeclaredConfigRelativePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "harness.toml"), []byte("base = \"default\"\nmax_turns = 25\n"), 0o600); err != nil {
		t.Fatalf("writing declared config: %v", err)
	}

	sel, err := harness.Resolve(dir, harness.Overrides{HarnessConfig: "harness.toml"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.Config.MaxTurns != 25 {
		t.Errorf("max_turns = %d, want 25 (relative path resolved against dir)", sel.Config.MaxTurns)
	}
}

// TestResolveDeclaredConfigBeatsFileHarness confirms the flag rung beats the
// file rung: --harness-config wins over a `harness =` key in the repo config.
func TestResolveDeclaredConfigBeatsFileHarness(t *testing.T) {
	dir := writeConfig(t, t.TempDir(), "harness = \"naive-v1\"\n")
	declared := writeDeclared(t, "base = \"default\"\nmax_turns = 33\n")

	sel, err := harness.Resolve(dir, harness.Overrides{HarnessConfig: declared})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if sel.Config.Name != harness.DeclaredConfigNamePrefix+"default" {
		t.Errorf("harness name = %q, want the declared config to beat the file's harness = naive-v1",
			sel.Config.Name)
	}
	if sel.Config.MaxTurns != 33 {
		t.Errorf("max_turns = %d, want 33", sel.Config.MaxTurns)
	}
}

// TestResolveDeclaredConfigRefusals covers the usage errors the flag adds: both
// harness spellings at once, a path that does not exist, and a declared file
// whose own contents are bad. Each is exit 2, the same posture as the rest of
// the selection chain.
func TestResolveDeclaredConfigRefusals(t *testing.T) {
	good := writeDeclared(t, "base = \"default\"\n")

	cases := []struct {
		name      string
		overrides harness.Overrides
		wantErr   string
	}{
		{
			name:      "both --harness and --harness-config",
			overrides: harness.Overrides{Harness: "naive-v1", HarnessConfig: good},
			wantErr:   "both choose the harness",
		},
		{
			name:      "a path that does not exist",
			overrides: harness.Overrides{HarnessConfig: filepath.Join(t.TempDir(), "nope.toml")},
			wantErr:   "reading declared config",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harness.Resolve(t.TempDir(), tc.overrides)
			if err == nil {
				t.Fatalf("Resolve succeeded, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if !harness.IsUsageError(err) {
				t.Errorf("a bad --harness-config invocation is a usage error (exit 2): %v", err)
			}
		})
	}
}

// TestResolveDeclaredConfigBadContents confirms an error from inside the
// declared file (an unknown base) surfaces from Resolve as a usage error rather
// than being swallowed.
func TestResolveDeclaredConfigBadContents(t *testing.T) {
	declared := writeDeclared(t, "base = \"no-such-base\"\n")

	_, err := harness.Resolve(t.TempDir(), harness.Overrides{HarnessConfig: declared})
	if err == nil {
		t.Fatal("Resolve succeeded, want an error for an unknown base")
	}
	if !strings.Contains(err.Error(), "unknown base harness configuration") {
		t.Fatalf("error = %v, want it to mention the unknown base", err)
	}
	if !harness.IsUsageError(err) {
		t.Errorf("a bad declared config is a usage error (exit 2): %v", err)
	}
}
