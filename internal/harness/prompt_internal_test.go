package harness

import (
	"strings"
	"testing"
)

// registeredSystemPrompts returns every registered configuration's system
// prompt, by configuration name.
//
// Reading it out of [configs] rather than testing [DefaultSystemPrompt]
// alone is what holds a second configuration to this file's two checks from
// the day it is registered: every configuration happens to carry
// DefaultSystemPrompt today ([MinimaxM2ConfigName], added by KAN-942, varies
// only the sampling seed policy — see its own doc comment in harness.go),
// but nothing enforces that staying true, and a configuration with its own
// prompt deserves this budget and this whitespace check from the moment it
// exists rather than from whenever someone notices the gap.
func registeredSystemPrompts(t *testing.T) map[string]string {
	t.Helper()
	names := ConfigNames()
	if len(names) == 0 {
		t.Fatal("positive control failed: ConfigNames() lists no configuration at all")
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		cfg, ok := ConfigByName(name)
		if !ok {
			t.Fatalf("ConfigByName(%q) found nothing right after ConfigNames() listed it", name)
		}
		out[name] = cfg.SystemPrompt
	}
	return out
}

// TestSystemPromptFitsItsBudget keeps the prompt's growth a decision.
//
// Slice 1 re-sends the whole prompt on every turn (docs/SLICE-1.md affordance
// E2, naive full history), so length is paid per turn and not per session. The
// ceiling is arbitrary in the way a ceiling has to be; what it is not is
// decorative, because raising it is a diff somebody signs rather than an
// accumulation nobody notices.
func TestSystemPromptFitsItsBudget(t *testing.T) {
	for name, prompt := range registeredSystemPrompts(t) {
		t.Run(name, func(t *testing.T) {
			if n := len(prompt); n > promptBudget {
				t.Errorf("the %q configuration's system prompt is %d bytes and the budget is %d\n"+
					"it is re-sent on every turn of every task; if the extra prose earns its place, "+
					"raise promptBudget in the same change and say what it buys", name, n, promptBudget)
			}
		})
	}
}

// TestSystemPromptIsWhitespaceClean stops an invisible edit from opening a new
// arm.
//
// The prompt is hashed by digest, so a trailing space added by an editor moves
// the harness config hash and un-pools every result taken before it, with
// nothing in the diff a reviewer can see. Rejecting the characters that do that
// is cheaper than trying to spot them: the prompt is hashed exactly as it is
// sent, and normalising it at load time would mean the file and the bytes on
// the wire were two different things.
func TestSystemPromptIsWhitespaceClean(t *testing.T) {
	for name, prompt := range registeredSystemPrompts(t) {
		t.Run(name, func(t *testing.T) {
			if prompt == "" {
				t.Fatalf("the %q configuration's system prompt is empty", name)
			}
			if strings.Contains(prompt, "\r") {
				t.Error("the system prompt contains a carriage return; it is hashed byte for byte, " +
					"so a CRLF checkout would give this binary a different arm from every other one")
			}
			if strings.Contains(prompt, "\t") {
				t.Error("the system prompt contains a tab; use spaces, so that what a reviewer sees in " +
					"the diff is what is hashed")
			}
			if !strings.HasSuffix(prompt, "\n") || strings.HasSuffix(prompt, "\n\n") {
				t.Error("the system prompt must end with exactly one newline")
			}

			for i, line := range strings.Split(prompt, "\n") {
				if line != strings.TrimRight(line, " ") {
					t.Errorf("line %d of the system prompt has trailing whitespace: %q", i+1, line)
				}
			}
		})
	}
}
