package harness_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/harness"
)

// writeDeclared writes content to a file in a fresh temp dir and returns its
// path, the way LoadDeclaredConfig is called in practice.
func writeDeclared(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing declared config: %v", err)
	}
	return path
}

// TestLoadDeclaredConfig drives the declared-config reader and resolver.
//
// The refusals are the point. This file is kopicode's own, so — unlike
// .kopicode/config.toml's reader — a key it does not understand is an error
// naming the file and line, because a silently-ignored `max_turns` is the exact
// bad experience ADR-0010 exists to fix. Every failure is a usage error (exit
// 2), the same posture as the rest of the selection chain.
func TestLoadDeclaredConfig(t *testing.T) {
	cases := []struct {
		name    string
		content string
		// check runs on the resolved config when no error is expected.
		check   func(t *testing.T, cfg harness.Config)
		wantErr string
	}{
		{
			name:    "base only keeps every base field",
			content: "base = \"default\"\n",
			check: func(t *testing.T, cfg harness.Config) {
				base, _ := harness.ConfigByName("default")
				if cfg.MaxTurns != base.MaxTurns {
					t.Errorf("max_turns = %d, want the base's %d", cfg.MaxTurns, base.MaxTurns)
				}
				if cfg.TokenBudget != base.TokenBudget {
					t.Errorf("token_budget = %d, want the base's %d", cfg.TokenBudget, base.TokenBudget)
				}
			},
		},
		{
			name:    "the resolved name is prefixed",
			content: "base = 'default'\n",
			check: func(t *testing.T, cfg harness.Config) {
				if cfg.Name != harness.DeclaredConfigNamePrefix+"default" {
					t.Errorf("name = %q, want %q", cfg.Name, harness.DeclaredConfigNamePrefix+"default")
				}
			},
		},
		{
			name:    "max_turns override",
			content: "base = \"default\"\nmax_turns = 40\n",
			check: func(t *testing.T, cfg harness.Config) {
				if cfg.MaxTurns != 40 {
					t.Errorf("max_turns = %d, want 40", cfg.MaxTurns)
				}
			},
		},
		{
			name:    "all four numeric overrides",
			content: "base = \"default\"\nmax_turns = 30\ntoken_budget = 4_000_000\nrepair_budget = 3\nmax_tokens = 16384\n",
			check: func(t *testing.T, cfg harness.Config) {
				if cfg.MaxTurns != 30 || cfg.TokenBudget != 4_000_000 || cfg.RepairBudget != 3 || cfg.Sampling.MaxTokens != 16384 {
					t.Errorf("overrides not all applied: turns=%d budget=%d repair=%d maxtok=%d",
						cfg.MaxTurns, cfg.TokenBudget, cfg.RepairBudget, cfg.Sampling.MaxTokens)
				}
			},
		},
		{
			name:    "a non-default base",
			content: "base = \"naive-v1\"\n",
			check: func(t *testing.T, cfg harness.Config) {
				if cfg.Name != harness.DeclaredConfigNamePrefix+"naive-v1" {
					t.Errorf("name = %q, want a naive-v1 declared name", cfg.Name)
				}
			},
		},
		{
			name:    "a trailing comment on an int",
			content: "base = \"default\"\nmax_turns = 50  # roomy\n",
			check: func(t *testing.T, cfg harness.Config) {
				if cfg.MaxTurns != 50 {
					t.Errorf("max_turns = %d, want 50", cfg.MaxTurns)
				}
			},
		},
		{
			name:    "repair_budget of zero is the no-repair arm, not an error",
			content: "base = \"default\"\nrepair_budget = 0\n",
			check: func(t *testing.T, cfg harness.Config) {
				if cfg.RepairBudget != 0 {
					t.Errorf("repair_budget = %d, want 0", cfg.RepairBudget)
				}
			},
		},
		{
			name:    "token_budget of zero is unbounded, not an error",
			content: "base = \"default\"\ntoken_budget = 0\n",
			check: func(t *testing.T, cfg harness.Config) {
				if cfg.TokenBudget != 0 {
					t.Errorf("token_budget = %d, want 0", cfg.TokenBudget)
				}
			},
		},

		// Refusals.
		{
			name:    "no base",
			content: "max_turns = 40\n",
			wantErr: "must name a `base`",
		},
		{
			name:    "unknown base",
			content: "base = \"defalt\"\n",
			wantErr: "unknown base harness configuration",
		},
		{
			name:    "an unknown override key is refused, not skipped",
			content: "base = \"default\"\nmax_turn = 40\n",
			wantErr: "is not a field a declared harness config can override",
		},
		{
			name:    "a field kopicode does not tune here",
			content: "base = \"default\"\ntemperature = 0.5\n",
			wantErr: "is not a field a declared harness config can override",
		},
		{
			name:    "a table header",
			content: "base = \"default\"\n[sampling]\nmax_tokens = 100\n",
			wantErr: "table header",
		},
		{
			name:    "a duplicate key",
			content: "base = \"default\"\nmax_turns = 10\nmax_turns = 20\n",
			wantErr: "set twice",
		},
		{
			name:    "a non-integer int value",
			content: "base = \"default\"\nmax_turns = \"lots\"\n",
			wantErr: "is not an integer",
		},
		{
			name:    "max_turns of zero",
			content: "base = \"default\"\nmax_turns = 0\n",
			wantErr: "max_turns resolves to 0",
		},
		{
			name:    "a negative max_turns",
			content: "base = \"default\"\nmax_turns = -5\n",
			wantErr: "must be greater than 0",
		},
		{
			name:    "max_tokens of zero",
			content: "base = \"default\"\nmax_tokens = 0\n",
			wantErr: "max_tokens resolves to 0",
		},
		{
			name:    "a negative token_budget",
			content: "base = \"default\"\ntoken_budget = -1\n",
			wantErr: "token_budget resolves to -1",
		},
		{
			name:    "a bare unquoted base",
			content: "base = default\n",
			wantErr: "not a quoted string",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := harness.LoadDeclaredConfig(writeDeclared(t, tc.content))

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("LoadDeclaredConfig succeeded, want an error containing %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				if !harness.IsUsageError(err) {
					t.Errorf("a malformed declared config is a usage error (exit 2), not a harness "+
						"error: %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("LoadDeclaredConfig: %v", err)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

// TestDeclaredConfigNeverPoolsWithItsBase is the guardrail ADR-0010 decision 3
// asks for, turned into a test: a declared configuration must not share a hash
// with the built-in it is based on, even when it overrides nothing — otherwise
// a locally-tuned run could pool with, and silently stand in for, a published
// built-in number. The mechanism is the [DeclaredConfigNamePrefix] on the
// resolved name, and the name is in the hash preimage.
func TestDeclaredConfigNeverPoolsWithItsBase(t *testing.T) {
	for _, base := range harness.ConfigNames() {
		t.Run(base, func(t *testing.T) {
			builtin, ok := harness.ConfigByName(base)
			if !ok {
				t.Fatalf("ConfigByName(%q) not found", base)
			}

			declared, err := harness.LoadDeclaredConfig(writeDeclared(t, "base = \""+base+"\"\n"))
			if err != nil {
				t.Fatalf("LoadDeclaredConfig: %v", err)
			}

			if !strings.HasPrefix(declared.Name, harness.DeclaredConfigNamePrefix) {
				t.Errorf("declared name %q lacks the %q prefix that keeps it from pooling with a "+
					"built-in", declared.Name, harness.DeclaredConfigNamePrefix)
			}
			if declared.Hash() == builtin.Hash() {
				t.Errorf("declared config from base %q hashes identically to the built-in (%s); it "+
					"would pool with a published number, which ADR-0010 decision 3 forbids",
					base, builtin.Hash())
			}
		})
	}
}
