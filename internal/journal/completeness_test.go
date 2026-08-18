package journal_test

import (
	"reflect"
	"testing"

	"github.com/leejianrong/kopicode/internal/journal"
)

// KAN-842: fixtures() gives every payload type one hand-built value, and
// TestRoundTripEveryKnownType proves that value survives Marshal/Unmarshal
// unchanged. Neither test notices a field that sat at its Go zero value the
// whole time: a round trip that drops a zero-valued field and one that
// carries it faithfully produce the identical assertion result. KAN-838 added
// ToolCallParsed.ArgEncoding and the fixture had to be hand-checked to confirm
// "json_string" was deliberately not "object" (ArgEncoding's own zero-look
// value) — this file makes that check automatic instead of something the next
// agent has to remember.
//
// The rule: walk every field fixtures() sets, and fail on any field still at
// its zero value unless a reasoned entry in zeroExemptions says otherwise.
// journal.Text is the one type treated as an atomic leaf rather than expanded
// field by field — see the doc comment on collectZeroFields for why.

// zeroExemption names one payload field whose zero value is meaningful and
// therefore not evidence of a forgotten fixture. The reason is mandatory and
// checked: an entry with an empty Reason is treated as not exempting anything,
// the same discipline CLAUDE.md's subprocess waivers apply to
// //kopicode:allow-nodir and //kopicode:allow-noenv comments with no reason.
type zeroExemption struct {
	Type   journal.Type
	Path   string // dotted field path, e.g. "Provider.AllowFallbacks"
	Reason string
}

// zeroExemptions is the complete exemption vocabulary. Add to it only when a
// field's zero value is itself the meaningful, intended state — never to
// silence a fixture that simply forgot to set something.
var zeroExemptions = []zeroExemption{
	{
		Type: journal.TypeSessionStarted,
		Path: "Provider.AllowFallbacks",
		Reason: "AllowFallbacks is false on every real benchmark request " +
			"(docs/adr/0005, CLAUDE.md 'Pin the provider on every benchmark " +
			"request'); true in a fixture would misrepresent production " +
			"traffic, so false is the correct value here, not an oversight.",
	},
	{
		Type: journal.TypeProviderRequest,
		Path: "Provider.AllowFallbacks",
		Reason: "Same field, same reason as SessionStarted.Provider." +
			"AllowFallbacks above: false is what every real request carries.",
	},
	{
		Type: journal.TypeVerificationRun,
		Path: "ExitCode",
		Reason: "0 is verification passing, which is a legitimate and " +
			"common outcome to fixture — internal/verify's Outcome docs make " +
			"NotRun, not Passed, the zero value at the Outcome level, but the " +
			"wire ExitCode itself is a plain int and 0 genuinely means " +
			"success here.",
	},
	{
		Type: journal.TypeSessionEnded,
		Path: "ExitCode",
		Reason: "0 is the process exit code for a session that completed " +
			"successfully — a legitimate, common value and not a forgotten " +
			"field. kopicode/run --print's exit codes document 0 as success.",
	},
}

// isExempt reports whether typ/path has a reasoned exemption. An entry with no
// Reason counts as absent: "a waiver with no reason waives nothing."
func isExempt(typ journal.Type, path string) (reason string, ok bool) {
	for _, e := range zeroExemptions {
		if e.Type == typ && e.Path == path {
			if e.Reason == "" {
				return "", false
			}
			return e.Reason, true
		}
	}
	return "", false
}

var textType = reflect.TypeOf(journal.Text{})

// collectZeroFields walks v — a payload struct value, or any struct value —
// and returns the dotted path of every field left at its Go zero value.
//
// journal.Text is deliberately not expanded into its own Inline/Blob/Size
// fields: exactly one of Inline and Blob is set on any real Text (InlineText
// sets the former, BlobText the latter), so one of the two is always "" by
// design, and treating that as a per-field zero would make every Text-typed
// field in the journal need its own exemption. Text's own invariant is tested
// elsewhere (TestTextCarriesTheFullSize); this walk only asks whether the
// field was populated at all, via the whole struct's IsZero — true only when
// every one of Text's fields (including Size) is unset.
//
// Pointer fields (ToolResult.ExitCode, Sampling.Seed) and slice fields
// (ProviderPin.Order, VerificationRun.Command) are also leaves: the check is
// whether the pointer/slice itself is nil, not whether the pointee or
// elements are individually non-zero. A *int pointing at 0 is a recorded
// value ("this tool reported exit code 0"), not a missing one, and unwrapping
// it would turn "recorded 0" and "not recorded" into the same finding.
//
// Any other struct-typed field (ProviderPin, Sampling, TokenCounts, BuildInfo)
// is expanded recursively, because those are exactly the shapes where a field
// added later — the KAN-838 scenario, one level down — would otherwise hide
// behind sibling fields that are set.
func collectZeroFields(v reflect.Value, prefix string) []string {
	var zeros []string
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		fv := v.Field(i)
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}

		if fv.Type() == textType {
			if fv.IsZero() {
				zeros = append(zeros, path)
			}
			continue
		}

		if fv.Kind() == reflect.Struct {
			zeros = append(zeros, collectZeroFields(fv, path)...)
			continue
		}

		if fv.IsZero() {
			zeros = append(zeros, path)
		}
	}
	return zeros
}

// TestFixturesHaveNoUnexplainedZeroFields is the completeness gate. Every
// field fixtures() sets must either be non-zero or carry a reasoned
// zeroExemptions entry; anything else fails, naming the payload type and the
// exact field path so the next person adding a field sees precisely what to
// fix rather than a generic "round trip failed."
func TestFixturesHaveNoUnexplainedZeroFields(t *testing.T) {
	for typ, payload := range fixtures() {
		t.Run(string(typ), func(t *testing.T) {
			v := reflect.ValueOf(payload)
			if v.Kind() != reflect.Struct {
				t.Fatalf("fixture for %q is a %s, not a struct — collectZeroFields expects a payload value", typ, v.Kind())
			}
			for _, path := range collectZeroFields(v, "") {
				reason, ok := isExempt(typ, path)
				if !ok {
					t.Errorf("fixture for %q leaves field %q at its zero value with no exemption — "+
						"give it a distinguishing value in fixtures(), or add a reasoned entry to "+
						"zeroExemptions if the zero value is itself meaningful", typ, path)
					continue
				}
				if reason == "" {
					t.Errorf("exemption for %q %q has an empty reason and therefore exempts nothing", typ, path)
				}
			}
		})
	}
}

// TestZeroExemptionsAllCarryAReason is the exemption vocabulary's own
// discipline check, independent of any fixture: every entry in the table must
// have a non-empty Reason, full stop. This is what "an exemption with no
// reason must exempt nothing" means as a standing invariant rather than
// something only checked incidentally by whichever fixtures happen to hit it.
func TestZeroExemptionsAllCarryAReason(t *testing.T) {
	for _, e := range zeroExemptions {
		if e.Reason == "" {
			t.Errorf("zeroExemptions entry for %q %q has no reason", e.Type, e.Path)
		}
	}
}

// TestCollectZeroFieldsWalksNestedStructsAndLeaves is the positive control:
// proof the reflection walk itself can find zero fields at all, including one
// level of nesting and the Text/pointer/slice leaf cases, so a broken walk —
// say, a version that stops descending into nested structs, or that
// mishandles Text and starts flagging Inline/Blob individually — fails this
// test rather than silently degrading into a guard that never reports
// anything (or reports everything). It uses a synthetic struct built for this
// test alone, not a real journal payload, so it cannot be satisfied by
// accidentally-correct production fixture data.
func TestCollectZeroFieldsWalksNestedStructsAndLeaves(t *testing.T) {
	type inner struct {
		Set    string
		Unset  string
		Number int
	}
	type synthetic struct {
		TopSet    string
		TopUnset  string
		Nested    inner
		WholeText journal.Text // entirely zero
		HalfText  journal.Text // Inline set, Blob deliberately ""
		NilPtr    *int
		SetPtr    *int // points at 0 on purpose — the pointer itself is what's checked
		NilSlice  []string
		SetSlice  []string
	}

	zero := 0
	s := synthetic{
		TopSet:    "x",
		TopUnset:  "",
		Nested:    inner{Set: "y", Unset: "", Number: 0},
		WholeText: journal.Text{},
		HalfText:  journal.InlineText("hi"),
		NilPtr:    nil,
		SetPtr:    &zero,
		NilSlice:  nil,
		SetSlice:  []string{"a"},
	}

	got := collectZeroFields(reflect.ValueOf(s), "")

	want := map[string]bool{
		"TopUnset":      true,
		"Nested.Unset":  true,
		"Nested.Number": true,
		"WholeText":     true,
		"NilPtr":        true,
		"NilSlice":      true,
	}
	dontWant := []string{"TopSet", "Nested.Set", "HalfText", "SetPtr", "SetSlice"}

	if len(got) != len(want) {
		t.Fatalf("collectZeroFields returned %d paths, want %d: got %v", len(got), len(want), got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("collectZeroFields reported %q, which should not be zero", p)
		}
	}
	for _, p := range dontWant {
		for _, g := range got {
			if g == p {
				t.Errorf("collectZeroFields reported %q as zero, but it is set", p)
			}
		}
	}
	if len(got) == 0 {
		t.Fatal("collectZeroFields found nothing at all — a walk that never descends " +
			"into nested structs would also produce this, and would make " +
			"TestFixturesHaveNoUnexplainedZeroFields pass vacuously")
	}
}

// TestCompletenessGuardCatchesAnUnexemptedZeroField exercises the gate's
// failure path directly, without depending on any real payload's shape —
// mirroring how KAN-848 tested its AST walk's failure path against a fake
// input rather than mutating key.go. This stands in for the manual proof (see
// the PR description) that adding a field to a real payload — e.g.
// SessionStarted — and leaving it at zero in fixtures() makes
// TestFixturesHaveNoUnexplainedZeroFields fail by naming the new field.
func TestCompletenessGuardCatchesAnUnexemptedZeroField(t *testing.T) {
	type addedFieldLater struct {
		Existing string
		Added    int // a hypothetical new field, left unset — the KAN-842 scenario
	}
	fixture := addedFieldLater{Existing: "set"}

	zeros := collectZeroFields(reflect.ValueOf(fixture), "")
	if len(zeros) != 1 || zeros[0] != "Added" {
		t.Fatalf("collectZeroFields(fixture) = %v, want [\"Added\"]", zeros)
	}

	const fakeType = journal.Type("SyntheticPayloadForTesting")
	if _, ok := isExempt(fakeType, "Added"); ok {
		t.Fatal("an unlisted (type, path) pair must never report as exempt — " +
			"that is exactly the gap that would let a forgotten field through")
	}
	// This is the same shape TestFixturesHaveNoUnexplainedZeroFields checks for
	// every real payload: a zero field with no exemption is a failure.
}
