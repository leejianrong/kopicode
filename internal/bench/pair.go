package bench

import (
	"fmt"
	"slices"
)

// TaskOutcome is one task's result within one arm.
//
// Pass/fail and nothing else, because ADR-0005 decision 5 makes the oracle a
// unit test suite: the outcome is the exit status of a command, not a judgement
// with a grade. Turns, tokens and the three-bucket failure classification are
// the runner's report, not the scorer's input — the scorer reads the one bit
// the paired test is defined over.
type TaskOutcome struct {
	// TaskID identifies the task in the frozen corpus. Pairing is by this
	// field, so it must be the same id in both arms.
	TaskID string
	// Passed is whether the task's oracle command succeeded.
	Passed bool
}

// Arm is one side of a paired comparison: an arm, per ADR-0007, being one
// (model x harness configuration x provider pin) run over the corpus.
//
// The scorer takes the outcomes as data rather than resolving them from a
// runner. Slice 1 registers a single harness configuration, so there is no
// second arm to build one of these from yet (docs/SLICE-1.md affordance B3),
// and a scorer that could only be exercised through a runner that does not
// exist could not be tested at all.
type Arm struct {
	// Name identifies the arm in reports and error messages. It is not
	// interpreted.
	Name string
	// Outcomes is this arm's result for every task in the corpus, in any
	// order.
	Outcomes []TaskOutcome
}

// Compare pairs two arms by task id and scores the result with [McNemar].
//
// Every task must appear exactly once in each arm. A task missing from one arm,
// a duplicated id, or an unnamed result is a typed error and never a dropped
// row: the discordant counts are the entire statistic, so a silently skipped
// task does not weaken the comparison, it changes the answer.
func Compare(armA, armB Arm) (Result, error) {
	a, err := index(armA)
	if err != nil {
		return Result{}, err
	}
	b, err := index(armB)
	if err != nil {
		return Result{}, err
	}

	if unpaired := symmetricDifference(a, b); len(unpaired) > 0 {
		return Result{}, &Error{
			Op:     "compare",
			Tasks:  unpaired,
			Detail: fmt.Sprintf("the arms must run the identical task set; %d task(s) appear in only one", len(unpaired)),
			err:    ErrUnpaired,
		}
	}

	var t Table
	for id, passedA := range a {
		switch passedB := b[id]; {
		case passedA && passedB:
			t.BothPassed++
		case passedA:
			t.AOnly++
		case passedB:
			t.BOnly++
		default:
			t.BothFailed++
		}
	}

	r, err := McNemar(t)
	if err != nil {
		return Result{}, err
	}
	r.ArmA, r.ArmB = armA.Name, armB.Name
	return r, nil
}

// index turns an arm's outcomes into a lookup, refusing anything that cannot be
// paired by id.
func index(arm Arm) (map[string]bool, error) {
	seen := make(map[string]bool, len(arm.Outcomes))
	var unnamed, duplicated []string

	for i, o := range arm.Outcomes {
		if o.TaskID == "" {
			unnamed = append(unnamed, fmt.Sprintf("#%d", i))
			continue
		}
		if _, ok := seen[o.TaskID]; ok {
			duplicated = append(duplicated, o.TaskID)
			continue
		}
		seen[o.TaskID] = o.Passed
	}

	if len(unnamed) > 0 {
		return nil, &Error{
			Op:     "compare",
			Arm:    arm.Name,
			Tasks:  unnamed,
			Detail: "pairing is by task id, so a result with no id cannot be paired (positions listed)",
			err:    ErrUnnamedTask,
		}
	}
	if len(duplicated) > 0 {
		slices.Sort(duplicated)
		return nil, &Error{
			Op:     "compare",
			Arm:    arm.Name,
			Tasks:  slices.Compact(duplicated),
			Detail: "a task may hold only one outcome per arm",
			err:    ErrDuplicateTask,
		}
	}
	return seen, nil
}

// symmetricDifference returns every task id present in exactly one arm, sorted
// so the error message is stable across runs.
func symmetricDifference(a, b map[string]bool) []string {
	var only []string
	for id := range a {
		if _, ok := b[id]; !ok {
			only = append(only, id)
		}
	}
	for id := range b {
		if _, ok := a[id]; !ok {
			only = append(only, id)
		}
	}
	slices.Sort(only)
	return only
}
