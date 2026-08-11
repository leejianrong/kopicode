package parse

import "testing"

// notRepairable names the two classifications that are deliberately owed no
// repair message, and why. Anything else missing from adviceByKind is a Kind
// that went quietly unhandled, which is the failure this file exists to catch.
var notRepairable = map[Kind]string{
	KindUnspecified: "it names a bug in this package, and asking the model to repair a harness bug launders a harness failure into a model number",
	KindNoCall:      "the model replied in prose; ErrNoToolCall is the one benign outcome and no repair is owed",
}

// TestEveryKindIsClassifiedOrExempt is the card's scoring criterion as a
// property rather than as ten assertions: a Kind added to the taxonomy without
// a repair message fails the suite instead of falling back on something
// generic.
//
// It is an internal test on purpose. The advice table is not exported — the
// model-facing message is reached through [Repairer.Step] — and exporting it
// only so a test could read it would widen the package's surface for the
// convenience of the test.
func TestEveryKindIsClassifiedOrExempt(t *testing.T) {
	for kind := range kindText {
		_, hasAdvice := adviceByKind[kind]
		why, exempt := notRepairable[kind]

		switch {
		case exempt && hasAdvice:
			t.Errorf("kind %s has a repair message but is documented as not repairable: %s", kind, why)
		case !exempt && !hasAdvice:
			t.Errorf(
				"kind %s has no repair message.\n"+
					"Every classification owes the model a distinct, actionable sentence "+
					"(docs/SLICE-1.md §3); add one to adviceByKind, or add the kind to "+
					"notRepairable with the reason it is owed none.",
				kind,
			)
		}
	}

	for kind := range adviceByKind {
		if _, known := kindText[kind]; !known {
			t.Errorf("adviceByKind holds %v, which is not in the kind taxonomy", kind)
		}
	}
}

// TestRepairAdviceIsPairwiseDistinct is the other half of the criterion. Two
// classifications rendering the same sentence means one of them is not
// classified — the model is told the same thing about two different mistakes,
// and the repair-recovery rate stops being attributable to the wording that
// produced it.
func TestRepairAdviceIsPairwiseDistinct(t *testing.T) {
	fields := []struct {
		name string
		get  func(advice) string
	}{
		{"problem", func(a advice) string { return a.problem }},
		{"expected", func(a advice) string { return a.expected }},
	}

	for _, f := range fields {
		t.Run(f.name, func(t *testing.T) {
			seen := map[string]Kind{}
			for kind, a := range adviceByKind {
				v := f.get(a)
				if v == "" {
					t.Errorf("kind %s has an empty %s", kind, f.name)
					continue
				}
				if other, dup := seen[v]; dup {
					t.Errorf("kinds %s and %s share a %s: %q", kind, other, f.name, v)
				}
				seen[v] = kind
			}
		})
	}
}

// TestRenderedMessagesArePairwiseDistinct checks the whole message, not just
// its parts, and checks it in both the repair-request and budget-spent
// renderings — the second drops the corrective sentence, so distinctness there
// rests on the problem alone.
func TestRenderedMessagesArePairwiseDistinct(t *testing.T) {
	for _, final := range []bool{false, true} {
		name := "repair request"
		if final {
			name = "budget spent"
		}
		t.Run(name, func(t *testing.T) {
			seen := map[string]Kind{}
			for kind := range adviceByKind {
				// nil tools on purpose: with no catalogue to quote, the
				// message is only its classification, which is exactly what
				// has to differ.
				msg := repairMessage(nil, &Error{Kind: kind}, final)
				if other, dup := seen[msg]; dup {
					t.Errorf("kinds %s and %s render the same message:\n%s", kind, other, msg)
				}
				seen[msg] = kind
			}
		})
	}
}

// TestNearestSuggestsButNeverSubstitutes pins the one place this package looks
// at a near miss. A suggestion is advice; resolving it would be the silent
// misapplication refused everywhere else.
func TestNearestSuggestsButNeverSubstitutes(t *testing.T) {
	tools := []string{"read_file", "write_file", "list_dir", "grep", "run_shell"}

	tests := []struct {
		name  string
		got   string
		want  string
		found bool
	}{
		{name: "a dropped suffix", got: "read", want: "read_file", found: true},
		{name: "a transposition", got: "raed_file", want: "read_file", found: true},
		{name: "a missing underscore", got: "readfile", want: "read_file", found: true},
		{name: "a plural", got: "greps", want: "grep", found: true},
		{name: "a name invented wholesale", got: "summon_the_compiler", found: false},
		{name: "an empty name", got: "", found: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := nearest(tt.got, tools)
			if ok != tt.found {
				t.Fatalf("nearest(%q) found = %v, want %v (got %q)", tt.got, ok, tt.found, got)
			}
			if ok && got != tt.want {
				t.Errorf("nearest(%q) = %q, want %q", tt.got, got, tt.want)
			}
		})
	}
}
