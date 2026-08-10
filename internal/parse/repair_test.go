package parse_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/parse"
)

// malformed is a reply that fails on every route and every attempt: single
// quotes inside a tool fence. It is the driver for the bound.
const malformed = "```tool\n{'tool': 'read_file', 'arguments': {'path': 'main.go'}}\n```"

// wellFormed is a reply that parses and validates against the fixture
// catalogue.
const wellFormed = "```tool\n{\"name\": \"read_file\", \"arguments\": {\"path\": \"main.go\"}}\n```"

func reply(content string) parse.Message { return parse.Message{Content: content} }

// TestRepairCorpus scores the classification and the message it produces. It is
// the card's first criterion — every classification produces a distinct,
// specific message — checked case by case; the pairwise-distinctness property
// over the whole taxonomy lives in message_internal_test.go.
func TestRepairCorpus(t *testing.T) {
	for _, tc := range repairCorpus {
		t.Run(tc.name, func(t *testing.T) {
			r := parse.NewRepairer(tools, parse.DefaultBudget)
			out := r.Step(tc.msg)

			if out.Event() != tc.wantEvent {
				t.Fatalf("event = %s, want %s (%s); feedback:\n%s", out.Event(), tc.wantEvent, tc.why, out.Feedback)
			}
			assertOutcomeInvariants(t, out)

			if tc.wantEvent == parse.EventToolCallParsed || tc.wantEvent == parse.EventNone {
				return
			}

			if out.Failure.Kind != tc.wantKind {
				t.Errorf("classification = %s, want %s", out.Failure.Kind, tc.wantKind)
			}
			for _, frag := range tc.says {
				if !strings.Contains(out.Feedback, frag) {
					t.Errorf("the repair message does not say %q; it said:\n%s", frag, out.Feedback)
				}
			}
			for _, frag := range tc.omits {
				if strings.Contains(out.Feedback, frag) {
					t.Errorf("the repair message mentions %q, which is not the failing tool; it said:\n%s", frag, out.Feedback)
				}
			}
		})
	}
}

// TestEveryClassificationInTheCorpusSaysSomethingDifferent is the corpus-level
// half of the card's scoring criterion: two classifications the model is told
// the same thing about are one classification wearing two names.
func TestEveryClassificationInTheCorpusSaysSomethingDifferent(t *testing.T) {
	byKind := map[parse.Kind]string{}

	for _, tc := range repairCorpus {
		if tc.wantEvent != parse.EventToolCallRepaired {
			continue
		}
		r := parse.NewRepairer(tools, parse.DefaultBudget)
		out := r.Step(tc.msg)
		if out.Event() != parse.EventToolCallRepaired {
			continue
		}
		byKind[out.Failure.Kind] = out.Feedback
	}

	seen := map[string]parse.Kind{}
	for kind, msg := range byKind {
		if other, dup := seen[msg]; dup {
			t.Errorf("kinds %s and %s produced the same repair message:\n%s", kind, other, msg)
		}
		seen[msg] = kind
	}
	if len(byKind) < 2 {
		t.Fatalf("the corpus reached only %d repairable classification(s); it is not testing distinctness", len(byKind))
	}
}

// TestRepairBoundStopsAtBudget is the guard this card turns on: at most two
// repair attempts, then the call fails. The table varies the budget because a
// bound that is only ever exercised at its default is a literal in a loop
// wearing a parameter's name.
func TestRepairBoundStopsAtBudget(t *testing.T) {
	budgets := []int{0, 1, parse.DefaultBudget, 5}

	for _, budget := range budgets {
		t.Run(budgetName(budget), func(t *testing.T) {
			r := parse.NewRepairer(tools, budget)

			// A model that never gets it right: exactly budget repair requests,
			// then one failure, and nothing after.
			for want := 1; want <= budget; want++ {
				out := r.Step(reply(malformed))
				if out.Event() != parse.EventToolCallRepaired {
					t.Fatalf("step %d: event = %s, want tool_call_repaired", want, out.Event())
				}
				if out.Attempt != want {
					t.Errorf("step %d: attempt = %d, want %d", want, out.Attempt, want)
				}
				if out.Terminal() {
					t.Errorf("step %d: a repair request reported itself terminal", want)
				}
			}

			out := r.Step(reply(malformed))
			if out.Event() != parse.EventToolCallFailed {
				t.Fatalf("after %d repairs the loop produced %s, not tool_call_failed — the bound is not being enforced", budget, out.Event())
			}
			if out.Attempt != budget {
				t.Errorf("the failure reports %d repairs spent, want %d", out.Attempt, budget)
			}
			if r.Repairs() != budget {
				t.Errorf("the loop spent %d repairs against a budget of %d", r.Repairs(), budget)
			}
			if !out.Terminal() {
				t.Error("a failed call site did not report itself terminal")
			}
		})
	}
}

// TestNegativeBudgetIsNoBudget keeps a misconfigured arm from becoming an
// unbounded one.
func TestNegativeBudgetIsNoBudget(t *testing.T) {
	r := parse.NewRepairer(tools, -3)
	if r.Budget() != 0 {
		t.Errorf("budget = %d, want 0", r.Budget())
	}
	if out := r.Step(reply(malformed)); out.Event() != parse.EventToolCallFailed {
		t.Errorf("event = %s, want tool_call_failed", out.Event())
	}
}

// TestTurnContinuesAfterTheBudgetIsSpent is the behaviour docs/SLICE-1.md §3
// asks for in place of aborting: the failure becomes an observation the turn
// carries on with, so it has to be something the model can be shown.
func TestTurnContinuesAfterTheBudgetIsSpent(t *testing.T) {
	r := parse.NewRepairer(tools, parse.DefaultBudget)
	r.Step(reply(malformed))
	r.Step(reply(malformed))
	out := r.Step(reply(malformed))

	if out.Event() != parse.EventToolCallFailed {
		t.Fatalf("event = %s, want tool_call_failed", out.Event())
	}
	if out.Feedback == "" {
		t.Fatal("the failure carries no observation; the turn has nothing to continue with")
	}
	if !strings.Contains(out.Feedback, "No repair attempts remain") {
		t.Errorf("the observation does not tell the model the budget is spent:\n%s", out.Feedback)
	}
	if strings.Contains(out.Feedback, "Send it again") {
		t.Errorf("the observation still asks for a repair after the budget is spent:\n%s", out.Feedback)
	}
	if len(out.Extraction.Calls()) != 0 {
		t.Error("a failed call site returned dispatchable calls")
	}
	if out.Failure == nil || out.Failure.Kind != parse.KindInvalidJSON {
		t.Errorf("the failure lost its classification: %+v", out.Failure)
	}
}

// TestRepairRecovers is the repair-recovery rate's numerator: a call that
// failed, was told what was wrong, and came back right.
func TestRepairRecovers(t *testing.T) {
	tests := []struct {
		name        string
		before      []string
		wantAttempt int
	}{
		{name: "right on the first try", wantAttempt: 0},
		{name: "right after one repair", before: []string{malformed}, wantAttempt: 1},
		{name: "right on the last allowed attempt", before: []string{malformed, malformed}, wantAttempt: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := parse.NewRepairer(tools, parse.DefaultBudget)
			for _, content := range tt.before {
				if out := r.Step(reply(content)); out.Event() != parse.EventToolCallRepaired {
					t.Fatalf("setup step produced %s", out.Event())
				}
			}

			out := r.Step(reply(wellFormed))
			if out.Event() != parse.EventToolCallParsed {
				t.Fatalf("event = %s, want tool_call_parsed", out.Event())
			}
			if out.Attempt != tt.wantAttempt {
				t.Errorf("attempt = %d, want %d", out.Attempt, tt.wantAttempt)
			}
			if got, want := out.Recovered(), tt.wantAttempt > 0; got != want {
				t.Errorf("Recovered() = %v, want %v", got, want)
			}
			if calls := out.Extraction.Calls(); len(calls) != 1 || calls[0].Name != "read_file" {
				t.Errorf("the recovered outcome carries %+v", calls)
			}
			if out.Feedback != "" || out.Failure != nil {
				t.Error("a successful parse carried a failure or a repair message")
			}
		})
	}
}

// TestProseIsNotRepaired holds the one benign outcome benign. A model that
// answered rather than called has done nothing wrong, and spending a repair
// attempt telling it otherwise would burn the budget on the ordinary case.
func TestProseIsNotRepaired(t *testing.T) {
	r := parse.NewRepairer(tools, parse.DefaultBudget)
	out := r.Step(reply("The bug is in main.go: the loop never terminates."))

	if out.Event() != parse.EventNone {
		t.Fatalf("event = %s, want none", out.Event())
	}
	if r.Repairs() != 0 {
		t.Errorf("prose spent %d repair attempts", r.Repairs())
	}
	if out.Feedback != "" || out.Failure != nil {
		t.Error("prose produced a repair message")
	}
	if !out.Terminal() {
		t.Error("a prose reply did not settle the call site")
	}
}

// TestEventMappingIsTotal is the "measurable from the journal" criterion
// checked rather than hoped for: every outcome the loop can produce maps onto
// exactly one journal event, and carries the fields that event needs.
//
// This package does not import internal/journal and must not — the engine
// journals. What it owes is that the engine never has to guess, and this is
// where that is proven.
func TestEventMappingIsTotal(t *testing.T) {
	// Every path the loop has, driven through the public API.
	drivers := []struct {
		name  string
		steps []string
		tools parse.Tools
		want  parse.Event
	}{
		{name: "first-try parse", steps: []string{wellFormed}, tools: tools, want: parse.EventToolCallParsed},
		{name: "prose", steps: []string{"just thinking out loud"}, tools: tools, want: parse.EventNone},
		{name: "extraction failure", steps: []string{malformed}, tools: tools, want: parse.EventToolCallRepaired},
		{name: "semantic failure", steps: []string{"```tool\n{\"name\":\"read\",\"arguments\":{}}\n```"}, tools: tools, want: parse.EventToolCallRepaired},
		{name: "budget spent", steps: []string{malformed, malformed, malformed}, tools: tools, want: parse.EventToolCallFailed},
		{name: "recovery", steps: []string{malformed, wellFormed}, tools: tools, want: parse.EventToolCallParsed},
		{name: "no catalogue, so no semantic check", steps: []string{"```tool\n{\"name\":\"read\",\"arguments\":{}}\n```"}, tools: nil, want: parse.EventToolCallParsed},
	}

	seen := map[parse.Event]bool{}
	for _, d := range drivers {
		t.Run(d.name, func(t *testing.T) {
			r := parse.NewRepairer(d.tools, parse.DefaultBudget)
			var out parse.Outcome
			for _, content := range d.steps {
				out = r.Step(reply(content))
			}
			if out.Event() != d.want {
				t.Fatalf("event = %s, want %s", out.Event(), d.want)
			}
			assertOutcomeInvariants(t, out)
			seen[out.Event()] = true
		})
	}

	for _, ev := range []parse.Event{parse.EventNone, parse.EventToolCallParsed, parse.EventToolCallRepaired, parse.EventToolCallFailed} {
		if !seen[ev] {
			t.Errorf("no driver produced %s; the mapping is not proven total", ev)
		}
	}
	if seen[parse.EventUnspecified] {
		t.Error("the loop produced an unspecified event")
	}
}

// assertOutcomeInvariants checks that an outcome carries exactly what its event
// needs and nothing that contradicts it. An engine reading these fields must
// not have to guess which are meaningful.
func assertOutcomeInvariants(t *testing.T, out parse.Outcome) {
	t.Helper()

	switch out.Event() {
	case parse.EventToolCallParsed:
		if len(out.Extraction.Calls()) == 0 {
			t.Error("tool_call_parsed carries no calls")
		}
		if !out.Extraction.Route().Valid() {
			t.Error("tool_call_parsed carries no route; ToolCallParsed.Route has nothing to record")
		}
		if out.Failure != nil || out.Feedback != "" {
			t.Error("tool_call_parsed carries a failure")
		}
	case parse.EventNone:
		if out.Failure != nil || out.Feedback != "" || len(out.Extraction.Calls()) != 0 {
			t.Error("none carries a call or a failure; it means the model replied in prose")
		}
	case parse.EventToolCallRepaired:
		if out.Failure == nil {
			t.Fatal("tool_call_repaired carries no classification")
		}
		if out.Failure.Kind == parse.KindUnspecified {
			t.Error("tool_call_repaired asked the model to repair an unclassified failure")
		}
		if out.Feedback == "" {
			t.Error("tool_call_repaired carries no message; ToolCallRepaired.Error has nothing to record")
		}
		if out.Attempt < 1 {
			t.Errorf("tool_call_repaired reports attempt %d; the journal's attempt is 1-based", out.Attempt)
		}
		if len(out.Extraction.Calls()) != 0 {
			t.Error("tool_call_repaired carries dispatchable calls")
		}
	case parse.EventToolCallFailed:
		if out.Failure == nil {
			t.Fatal("tool_call_failed carries no reason")
		}
		if out.Feedback == "" {
			t.Error("tool_call_failed carries no observation for the turn to continue with")
		}
		if len(out.Extraction.Calls()) != 0 {
			t.Error("tool_call_failed carries dispatchable calls")
		}
	case parse.EventUnspecified:
		t.Error("the loop produced an unspecified event")
	default:
		t.Errorf("outcome maps to no journal event: %s", out.Event())
	}
}

// TestSettledLoopIsNotReused catches a harness bug rather than a model one: a
// Repairer shared across two call sites shares the budget, which is the bound
// quietly ceasing to be one. It is reported as a failure, never as a repair
// request, because asking the model to fix our bookkeeping is how a harness
// failure gets laundered into a model number.
func TestSettledLoopIsNotReused(t *testing.T) {
	r := parse.NewRepairer(tools, parse.DefaultBudget)
	if out := r.Step(reply(wellFormed)); out.Event() != parse.EventToolCallParsed {
		t.Fatalf("setup: event = %s", out.Event())
	}

	out := r.Step(reply(wellFormed))
	if out.Event() != parse.EventToolCallFailed {
		t.Fatalf("event = %s, want tool_call_failed", out.Event())
	}
	if out.Failure.Kind != parse.KindUnspecified {
		t.Errorf("classification = %s, want unspecified — reuse is a harness bug", out.Failure.Kind)
	}
}

// TestFailureCarriesTheToolWhenItIsKnown feeds ToolCallFailed.Tool, which the
// bench classifier needs to tell a call that named nothing from one that named
// something wrong.
func TestFailureCarriesTheToolWhenItIsKnown(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "a semantic failure knows its tool", content: "```tool\n{\"name\":\"read_file\",\"arguments\":{}}\n```", want: "read_file"},
		{name: "an unknown tool knows what was asked for", content: "```tool\n{\"name\":\"reed_file\",\"arguments\":{}}\n```", want: "reed_file"},
		{name: "a block that never parsed names nothing", content: malformed, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := parse.NewRepairer(tools, 0)
			out := r.Step(reply(tt.content))
			if out.Event() != parse.EventToolCallFailed {
				t.Fatalf("event = %s, want tool_call_failed", out.Event())
			}
			if out.Failure.Tool != tt.want {
				t.Errorf("failure tool = %q, want %q", out.Failure.Tool, tt.want)
			}
		})
	}
}

// TestFailuresAreWrappedNotFlattened keeps errors.Is/errors.As working through
// the repair path, so the cause of a bad parse survives to whoever debugs it.
func TestFailuresAreWrappedNotFlattened(t *testing.T) {
	r := parse.NewRepairer(tools, parse.DefaultBudget)
	out := r.Step(reply(malformed))

	if out.Failure == nil {
		t.Fatal("the outcome carries no failure")
	}
	if errors.Unwrap(out.Failure) == nil {
		t.Error("the JSON decode error was not wrapped; %w is how the cause survives")
	}
	if errors.Is(out.Failure, parse.ErrNoToolCall) {
		t.Error("a malformed call reported itself as no call at all")
	}
	if !strings.Contains(out.Feedback, "invalid character") {
		t.Errorf("the repair message drops the decoder's own explanation:\n%s", out.Feedback)
	}
}

// TestUnknownEnvelopeRepairIsSpecificNotASchemaDump is the failing half this
// card exists to avoid: handing the model the whole catalogue back on every
// failure. The unknown-tool message is the only one allowed to name other
// tools, and even then only their names.
func TestUnknownEnvelopeRepairIsSpecificNotASchemaDump(t *testing.T) {
	r := parse.NewRepairer(tools, parse.DefaultBudget)
	out := r.Step(reply("```tool\n{\"name\":\"read_file\",\"arguments\":{}}\n```"))

	if out.Event() != parse.EventToolCallRepaired {
		t.Fatalf("event = %s", out.Event())
	}
	for _, other := range []string{"write_file", "grep", "list_dir"} {
		if strings.Contains(out.Feedback, other) {
			t.Errorf("the message names %q, a tool the call had nothing to do with:\n%s", other, out.Feedback)
		}
	}
	if n := len(strings.Split(strings.TrimSpace(out.Feedback), "\n")); n > 6 {
		t.Errorf("the repair message is %d lines; it is drifting towards a schema dump:\n%s", n, out.Feedback)
	}
}

func budgetName(n int) string {
	if n == 0 {
		return "no repairs"
	}
	return "budget of " + strconv.Itoa(n)
}
