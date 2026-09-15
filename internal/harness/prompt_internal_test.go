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

// TestDefaultPromptDocumentsTheSkillsConvention pins ADR-0014's skills
// mechanism to the prose that is its whole implementation: shape (a) adds no
// tool and no dispatch entry, so the one sentence pointing the model at
// .agents/skills/<name>/SKILL.md is the feature. If it is ever dropped or the
// convention path is retyped, the mechanism silently stops existing, which this
// catches.
//
// It is scoped to DefaultSystemPrompt (which minimax-m2-v1 inherits), not every
// configuration: the three naive configs are deliberately stripped-down A/B
// baselines with their own prompt files, and adding a capability to them would
// change what they measure — so this test also asserts they do *not* carry the
// pointer, keeping that scoping decision honest rather than incidental.
func TestDefaultPromptDocumentsTheSkillsConvention(t *testing.T) {
	for _, want := range []string{".agents/skills/", "SKILL.md"} {
		if !strings.Contains(DefaultSystemPrompt, want) {
			t.Errorf("the default system prompt does not mention %q; ADR-0014's skills mechanism is that "+
				"pointer sentence and nothing else, so losing it removes the feature", want)
		}
	}
	for name, prompt := range map[string]string{
		"naive-v1":      NaiveSystemPrompt,
		"naive-v2":      NaiveVerifyOnlySystemPrompt,
		"naive-toolset": NaiveToolsetSystemPrompt,
	} {
		if strings.Contains(prompt, ".agents/skills/") {
			t.Errorf("the %q baseline prompt mentions the skills convention; the naive configs are "+
				"deliberately minimal A/B baselines and must not gain a capability the default has", name)
		}
	}
}

// TestDefaultPromptTriggersSkillDiscovery pins KAN-1385: the skills mechanism
// must be discoverable regardless of the model's first move.
//
// The KAN-1384 dogfood found discovery was incidental — the model found a
// relevant skill only when its opening move happened to be a `list_dir` of the
// repo root (which surfaces `.agents/`), and missed an equally relevant skill on
// a task whose natural first move was `grep`, because it never listed the root.
// The fix keeps ADR-0014 shape (a) — no new tool, no auto-injected skill bodies
// — and lives entirely in this prose: the `## Skills` section now tells the
// model to `list_dir` the skills directory up front, so a grep-first task shape
// no longer hides it.
//
// This asserts that imperative. A passive "read_file it when it looks relevant"
// sentence cannot fire until the model already knows the skill exists, so it
// does not name `list_dir` and fails here — which is the state this card found
// and the regression it must not slide back into. Scoped to DefaultSystemPrompt
// for the same reason TestDefaultPromptDocumentsTheSkillsConvention is: the
// naive baselines deliberately carry no skills capability at all.
func TestDefaultPromptTriggersSkillDiscovery(t *testing.T) {
	section, ok := markdownSection(DefaultSystemPrompt, "## Skills")
	if !ok {
		t.Fatal("the default system prompt has no `## Skills` section; ADR-0014's mechanism is that " +
			"section and nothing else, so its absence removes the feature")
	}
	if !strings.Contains(section, "list_dir") {
		t.Errorf("the `## Skills` section does not tell the model to `list_dir` the skills directory:\n%s\n"+
			"KAN-1384 showed that discovery which waits for the model to judge a skill \"relevant\" only "+
			"fires when its first move happens to list the root; the trigger must be an imperative to "+
			"`list_dir` `.agents/skills` up front so discovery is independent of task shape (KAN-1385)",
			section)
	}
}

// markdownSection returns the body of the `## <title>` section of a prompt —
// the lines from just after the heading to the next second-level heading, or
// the end of the prompt — and whether the heading was found.
//
// It lets a test assert about one section's prose without a substring match
// leaking in from the rest of the prompt: `list_dir` appears in several other
// places (the `### list_dir` tool section, the `## Working` steps), so a bare
// strings.Contains over the whole prompt would pass even if the `## Skills`
// section never mentioned it.
func markdownSection(prompt, heading string) (string, bool) {
	lines := strings.Split(prompt, "\n")
	start := -1
	for i, line := range lines {
		if line == heading {
			start = i
			break
		}
	}
	if start == -1 {
		return "", false
	}
	var b strings.Builder
	for _, line := range lines[start+1:] {
		if strings.HasPrefix(line, "## ") {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), true
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
