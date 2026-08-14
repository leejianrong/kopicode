package harness

import (
	"strings"
	"testing"
)

// TestSystemPromptFitsItsBudget keeps the prompt's growth a decision.
//
// Slice 1 re-sends the whole prompt on every turn (docs/SLICE-1.md affordance
// E2, naive full history), so length is paid per turn and not per session. The
// ceiling is arbitrary in the way a ceiling has to be; what it is not is
// decorative, because raising it is a diff somebody signs rather than an
// accumulation nobody notices.
func TestSystemPromptFitsItsBudget(t *testing.T) {
	if n := len(DefaultSystemPrompt); n > promptBudget {
		t.Errorf("the system prompt is %d bytes and the budget is %d\n"+
			"it is re-sent on every turn of every task; if the extra prose earns its place, "+
			"raise promptBudget in the same change and say what it buys", n, promptBudget)
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
	if DefaultSystemPrompt == "" {
		t.Fatal("the system prompt is empty")
	}
	if strings.Contains(DefaultSystemPrompt, "\r") {
		t.Error("the system prompt contains a carriage return; it is hashed byte for byte, " +
			"so a CRLF checkout would give this binary a different arm from every other one")
	}
	if strings.Contains(DefaultSystemPrompt, "\t") {
		t.Error("the system prompt contains a tab; use spaces, so that what a reviewer sees in " +
			"the diff is what is hashed")
	}
	if !strings.HasSuffix(DefaultSystemPrompt, "\n") || strings.HasSuffix(DefaultSystemPrompt, "\n\n") {
		t.Error("the system prompt must end with exactly one newline")
	}

	for i, line := range strings.Split(DefaultSystemPrompt, "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line %d of the system prompt has trailing whitespace: %q", i+1, line)
		}
	}
}
