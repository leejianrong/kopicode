package permission

import (
	"context"
	"errors"
	"testing"
)

// identityResolver is enough for the guards below, which are about the
// classifier's coverage rather than about containment.
type identityResolver struct{}

func (identityResolver) Resolve(path string) (string, error) { return path, nil }

// allowAll is the least interesting policy there is; these guards care about
// what reaches a policy at all.
type allowAll struct{}

func (allowAll) Decide(context.Context, Request) (Decision, error) {
	return Decision{Verdict: VerdictAllow, Source: SourcePolicy}, nil
}

// TestEveryOperationIsClassified is the guard the card asks for: a new
// operation that nobody classified fails the suite rather than defaulting
// silently.
//
// It walks the declared operations rather than a list the test keeps, so
// declaring OperationDelete and forgetting the rule in classify turns this
// red — [ErrUnknownOperation] is the classifier's default branch, and reaching
// it is the failure.
func TestEveryOperationIsClassified(t *testing.T) {
	g, err := New("/repo", identityResolver{}, allowAll{})
	if err != nil {
		t.Fatalf("building gate: %v", err)
	}

	for op := range operationText {
		if op == OperationUnspecified {
			continue // the zero value is unclassified on purpose
		}
		t.Run(op.String(), func(t *testing.T) {
			// Every field an operation might need is populated, so a failure
			// here is about the missing rule and not about a missing argument.
			a := Action{
				ID:        "1",
				Tool:      "tool",
				Operation: op,
				Path:      "/repo/main.go",
				Command:   []string{"go", "test"},
				Dir:       "/repo",
			}
			if _, _, err := g.classify(a); errors.Is(err, ErrUnknownOperation) {
				t.Fatalf("operation %s reaches the classifier's default branch: it is declared but no rule says whether it needs consent", op)
			}
		})
	}
}

// TestWireVocabulary pins the strings the engine copies into
// journal.PermissionRequested and journal.PermissionDecided.
//
// This package does not import internal/journal — the engine journals, this
// package decides — so nothing but this test stops the two vocabularies
// drifting. If a value here changes, the event's documented values change with
// it, and every recorded session before the change is reading a different
// language.
func TestWireVocabulary(t *testing.T) {
	kinds := map[Kind]string{
		KindRunShell:         "run_shell",
		KindWriteOutsideRoot: "write_outside_root",
	}
	for k, want := range kinds {
		if got := k.String(); got != want {
			t.Errorf("Kind(%d) = %q, want %q", uint8(k), got, want)
		}
	}

	verdicts := map[Verdict]string{
		VerdictAllow:        "allow",
		VerdictDeny:         "deny",
		VerdictAllowSession: "allow_session",
	}
	for v, want := range verdicts {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d) = %q, want %q", uint8(v), got, want)
		}
	}

	sources := map[Source]string{
		SourceUser:   "user",
		SourcePolicy: "policy",
	}
	for s, want := range sources {
		if got := s.String(); got != want {
			t.Errorf("Source(%d) = %q, want %q", uint8(s), got, want)
		}
	}
}

// TestStringOfAnUndeclaredValue: a value outside the declared set must still
// print something that names it, because it will appear in a refusal message
// and "unspecified" would be a lie.
func TestStringOfAnUndeclaredValue(t *testing.T) {
	if got := Operation(200).String(); got != "operation(200)" {
		t.Errorf("Operation(200).String() = %q", got)
	}
	if got := Kind(200).String(); got != "kind(200)" {
		t.Errorf("Kind(200).String() = %q", got)
	}
	if got := Verdict(200).String(); got != "verdict(200)" {
		t.Errorf("Verdict(200).String() = %q", got)
	}
	if got := Source(200).String(); got != "source(200)" {
		t.Errorf("Source(200).String() = %q", got)
	}
}

// TestContains covers the containment predicate directly, including the two
// inputs a string-prefix implementation gets wrong.
func TestContains(t *testing.T) {
	tests := []struct {
		root, path string
		want       bool
	}{
		{"/repo", "/repo", true},
		{"/repo", "/repo/main.go", true},
		{"/repo", "/repo/a/b/c.go", true},
		{"/repo", "/repo-backup/main.go", false}, // shares the prefix, is not inside
		{"/repo", "/", false},
		{"/repo", "/etc/hosts", false},
		{"/", "/etc/hosts", true}, // a root of "/" contains everything
		{"/repo/a", "/repo", false},
	}
	for _, tc := range tests {
		if got := contains(tc.root, tc.path); got != tc.want {
			t.Errorf("contains(%q, %q) = %t, want %t", tc.root, tc.path, got, tc.want)
		}
	}
}
