package bench_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/internal/bench"
)

// outcomes builds an arm from a compact "task:pass" spelling, so a test case
// reads as the corpus result it is meant to be.
func outcomes(name string, passed map[string]bool, order ...string) bench.Arm {
	arm := bench.Arm{Name: name}
	for _, id := range order {
		arm.Outcomes = append(arm.Outcomes, bench.TaskOutcome{TaskID: id, Passed: passed[id]})
	}
	return arm
}

// TestComparePairsByTaskID is the happy path: the same five tasks in both arms,
// listed in different orders, producing the contingency table the flips imply.
func TestComparePairsByTaskID(t *testing.T) {
	armA := outcomes("baseline", map[string]bool{
		"t1": true, "t2": true, "t3": false, "t4": true, "t5": false,
	}, "t1", "t2", "t3", "t4", "t5")

	// Deliberately a different order, to prove pairing is by id and not by
	// position. A positional pairing would silently produce a different
	// table from the same data, which is the failure this asserts against.
	armB := outcomes("candidate", map[string]bool{
		"t1": true, "t2": false, "t3": true, "t4": false, "t5": false,
	}, "t5", "t3", "t1", "t4", "t2")

	got, err := bench.Compare(armA, armB)
	if err != nil {
		t.Fatalf("Compare: unexpected error: %v", err)
	}

	want := bench.Table{
		BothPassed: 1, // t1
		AOnly:      2, // t2, t4
		BOnly:      1, // t3
		BothFailed: 1, // t5
	}
	if diff := cmp.Diff(want, got.Table); diff != "" {
		t.Errorf("contingency table mismatch (-want +got):\n%s", diff)
	}
	if got.ArmA != "baseline" || got.ArmB != "candidate" {
		t.Errorf("arm names = %q/%q, want %q/%q", got.ArmA, got.ArmB, "baseline", "candidate")
	}
	if got.Direction != bench.FavoursA {
		t.Errorf("Direction = %v, want %v", got.Direction, bench.FavoursA)
	}
	if got.Method != bench.MethodExactBinomial {
		t.Errorf("Method = %v, want %v", got.Method, bench.MethodExactBinomial)
	}
	// 2 * P(X <= 1), X ~ Bin(3, 1/2) = 2 * (1+3)/8 = 1.
	if got.P != 1 {
		t.Errorf("P = %v, want 1: three flips two-to-one is no evidence", got.P)
	}
	if got.Table.Pairs() != 5 {
		t.Errorf("Pairs() = %d, want 5", got.Table.Pairs())
	}
}

// TestCompareIsOrderIndependent states the pairing property directly rather
// than relying on one shuffled fixture.
func TestCompareIsOrderIndependent(t *testing.T) {
	passedA := map[string]bool{"a": true, "b": false, "c": true, "d": false}
	passedB := map[string]bool{"a": false, "b": false, "c": true, "d": true}

	forward, err := bench.Compare(
		outcomes("A", passedA, "a", "b", "c", "d"),
		outcomes("B", passedB, "a", "b", "c", "d"),
	)
	if err != nil {
		t.Fatalf("Compare: unexpected error: %v", err)
	}
	reversed, err := bench.Compare(
		outcomes("A", passedA, "d", "c", "b", "a"),
		outcomes("B", passedB, "b", "d", "a", "c"),
	)
	if err != nil {
		t.Fatalf("Compare: unexpected error: %v", err)
	}
	if diff := cmp.Diff(forward, reversed); diff != "" {
		t.Errorf("result depends on outcome order (-forward +reversed):\n%s", diff)
	}
}

// TestCompareRefusesUnpairedInput is the guard ADR-0005 decision 1 rests on.
// The discordant counts are the entire statistic, so a task quietly dropped
// from one arm does not weaken the comparison, it changes the answer.
func TestCompareRefusesUnpairedInput(t *testing.T) {
	tests := []struct {
		name      string
		armA      bench.Arm
		armB      bench.Arm
		want      error
		wantArm   string
		wantTasks []string
	}{
		{
			name:      "task missing from arm B",
			armA:      outcomes("A", map[string]bool{"t1": true, "t2": false}, "t1", "t2"),
			armB:      outcomes("B", map[string]bool{"t1": true}, "t1"),
			want:      bench.ErrUnpaired,
			wantTasks: []string{"t2"},
		},
		{
			name:      "task missing from arm A",
			armA:      outcomes("A", map[string]bool{"t1": true}, "t1"),
			armB:      outcomes("B", map[string]bool{"t1": true, "t9": false}, "t1", "t9"),
			want:      bench.ErrUnpaired,
			wantTasks: []string{"t9"},
		},
		{
			name: "different corpora entirely, every id reported",
			armA: outcomes("A", map[string]bool{"a1": true, "a2": true}, "a1", "a2"),
			armB: outcomes("B", map[string]bool{"b1": true, "b2": true}, "b1", "b2"),
			want: bench.ErrUnpaired,
			// Sorted, and all four, because a pairing failure is
			// usually systematic and naming one of them turns one fix
			// into four runs of the same fix.
			wantTasks: []string{"a1", "a2", "b1", "b2"},
		},
		{
			name:      "duplicate task in arm A",
			armA:      outcomes("A", map[string]bool{"t1": true}, "t1", "t1"),
			armB:      outcomes("B", map[string]bool{"t1": true}, "t1"),
			want:      bench.ErrDuplicateTask,
			wantArm:   "A",
			wantTasks: []string{"t1"},
		},
		{
			name:      "duplicate task in arm B",
			armA:      outcomes("A", map[string]bool{"t1": true}, "t1"),
			armB:      outcomes("B", map[string]bool{"t1": true}, "t1", "t1", "t1"),
			want:      bench.ErrDuplicateTask,
			wantArm:   "B",
			wantTasks: []string{"t1"},
		},
		{
			name: "unnamed task",
			armA: bench.Arm{Name: "A", Outcomes: []bench.TaskOutcome{
				{TaskID: "t1", Passed: true},
				{TaskID: "", Passed: false},
			}},
			armB:      outcomes("B", map[string]bool{"t1": true}, "t1"),
			want:      bench.ErrUnnamedTask,
			wantArm:   "A",
			wantTasks: []string{"#1"},
		},
		{
			name: "both arms empty",
			armA: bench.Arm{Name: "A"},
			armB: bench.Arm{Name: "B"},
			want: bench.ErrNoPairs,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bench.Compare(tc.armA, tc.armB)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Compare error = %v, want %v", err, tc.want)
			}
			if got != (bench.Result{}) {
				t.Errorf("Compare returned %+v alongside the error; a refused comparison must score nothing", got)
			}

			var e *bench.Error
			if !errors.As(err, &e) {
				t.Fatalf("error %v is not a *bench.Error", err)
			}
			if tc.wantArm != "" && e.Arm != tc.wantArm {
				t.Errorf("Arm = %q, want %q", e.Arm, tc.wantArm)
			}
			if diff := cmp.Diff(tc.wantTasks, e.Tasks); diff != "" {
				t.Errorf("Tasks mismatch (-want +got):\n%s", diff)
			}
			// The message must carry every offending id, since that is
			// what a human reads before the struct.
			for _, id := range tc.wantTasks {
				if !strings.Contains(err.Error(), id) {
					t.Errorf("error message %q does not name task %q", err.Error(), id)
				}
			}
		})
	}
}

// TestCompareScoresTheZeroDiscordantCase holds the degenerate case end to end:
// two arms that agreed on every task are a real, reportable result and not an
// error, and not a division by zero either.
func TestCompareScoresTheZeroDiscordantCase(t *testing.T) {
	passed := map[string]bool{"t1": true, "t2": false, "t3": true}
	got, err := bench.Compare(
		outcomes("A", passed, "t1", "t2", "t3"),
		outcomes("B", passed, "t1", "t2", "t3"),
	)
	if err != nil {
		t.Fatalf("Compare: unexpected error: %v", err)
	}
	if got.Discordant() != 0 {
		t.Fatalf("Discordant() = %d, want 0", got.Discordant())
	}
	if got.Method != bench.MethodNone {
		t.Errorf("Method = %v, want %v", got.Method, bench.MethodNone)
	}
	if got.P != 1 {
		t.Errorf("P = %v, want exactly 1", got.P)
	}
	if got.Direction != bench.NoDifference {
		t.Errorf("Direction = %v, want %v", got.Direction, bench.NoDifference)
	}
	if got.Method.String() != "none" {
		t.Errorf("Method.String() = %q, want %q", got.Method.String(), "none")
	}
}
