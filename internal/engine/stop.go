package engine

import (
	"fmt"

	"github.com/leejianrong/kopicode/internal/journal"
)

// Stop says why a bounded exchange stopped.
//
// It is the loop's own vocabulary and it is finer than the journal's, on
// purpose. journal.SessionEnded.Reason has five documented values and two
// distinct stops map onto "error"; what separates them on the record is the
// exit code, which is SLICE-1 §Build Plan step 14's enumeration and is what a
// surface reports and a classifier reads.
//
// The mapping in full:
//
//	Stop                     Reason               ExitCode  SLICE-1 §9 bucket
//	StopCompleted            completed            0         model (everything else)
//	StopMaxTurns             max_turns            4         harness — named outright
//	StopBudgetExhausted      budget_exhausted     1         model; see the note below
//	StopVerificationFailed   verification_failed  1         model
//	StopCancelled            cancelled            1         neither
//	StopProviderError        error                3         harness — "a provider error surviving retries"
//	StopHarnessError         error                4         harness
//
// **The budget is the one that is not settled here.** SLICE-1 §9 lists the
// `harness` triggers explicitly and the token budget is not among them, so a
// budget-exhausted session falls into "everything else" and is charged to the
// model. That may well be wrong — a task the model could have finished in one
// more turn is not a model failure — but the classifier is KAN-797's and
// inventing a fourth bucket here would be worse. What this package owes is that
// the reason is on the record and distinct from every other, which it is.
type Stop uint8

const (
	// StopUnspecified is the zero value and means the loop has not concluded.
	// It is never returned by [Engine.Run].
	StopUnspecified Stop = iota

	// StopCompleted is the clean stop: the model replied in prose, asking for
	// no tool, so there is nothing left to do.
	StopCompleted

	// StopMaxTurns is the turn cap. It is a harness failure by name in
	// docs/SLICE-1.md §9.
	StopMaxTurns

	// StopBudgetExhausted is the token budget, decided from reported usage.
	StopBudgetExhausted

	// StopVerificationFailed is the model saying it is finished over a tree the
	// project's own verification command has rejected (docs/SLICE-1.md §5).
	//
	// It exists because the alternative is [StopCompleted], and "completed" is
	// exactly the word forced verification exists to withhold. It is exit 1 —
	// task not completed — and it is charged to the *model*: the harness ran the
	// suite, showed the model the failure, and the model stopped anyway.
	//
	// Only a command that ran and exited non-zero produces this. A verification
	// that could not run — no command found, the toolchain missing, a timeout —
	// is recorded honestly and does not block, for the reason internal/verify's
	// package doc gives.
	StopVerificationFailed

	// StopCancelled is a cancelled context — Ctrl-C, or a bench runner
	// abandoning a task. Neither bucket.
	StopCancelled

	// StopProviderError is a provider call that produced no usable reply.
	// Retries and backoff belong to the client (docs/SLICE-1.md §Build Plan
	// step 8), so what reaches the loop has already survived them.
	StopProviderError

	// StopHarnessError is the harness breaking: the journal refused a write,
	// the dispatcher and the catalogue disagreed, a tool result could not be
	// placed in the conversation.
	StopHarnessError
)

// stopReason is the journal.SessionEnded.Reason each stop is recorded under.
var stopReason = map[Stop]string{
	StopUnspecified:        "unspecified",
	StopCompleted:          "completed",
	StopMaxTurns:           "max_turns",
	StopBudgetExhausted:    "budget_exhausted",
	StopVerificationFailed: "verification_failed",
	StopCancelled:          "cancelled",
	StopProviderError:      "error",
	StopHarnessError:       "error",
}

// stopExit is the process exit code each stop maps to, from docs/SLICE-1.md
// §Build Plan step 14: 0 success, 1 task not completed, 2 usage error,
// 3 provider error, 4 harness error. 2 is not reachable from here — an unknown
// model id is refused before the engine is built (ADR-0007 decision 4).
var stopExit = map[Stop]int{
	StopUnspecified:        4,
	StopCompleted:          0,
	StopMaxTurns:           4,
	StopBudgetExhausted:    1,
	StopVerificationFailed: 1,
	StopCancelled:          1,
	StopProviderError:      3,
	StopHarnessError:       4,
}

// Reason returns the journal.SessionEnded.Reason for this stop.
func (s Stop) Reason() string {
	if r, ok := stopReason[s]; ok {
		return r
	}
	return fmt.Sprintf("stop(%d)", uint8(s))
}

// ExitCode returns the process exit code for this stop.
//
// A stop nobody mapped exits 4. That is the fail-closed direction: an
// unrecognised outcome is a harness defect, and reporting success for it is how
// a broken loop passes a smoke test.
func (s Stop) ExitCode() int {
	if c, ok := stopExit[s]; ok {
		return c
	}
	return 4
}

// String renders the stop for a message.
func (s Stop) String() string { return s.Reason() }

// Result is what one call to [Engine.Run] concluded.
//
// It is a summary for the caller, never the record: everything here is derived
// from events the loop already wrote, and a surface printing a session prints
// the journal.
type Result struct {
	// Stop is why the exchange stopped.
	Stop Stop
	// Turns is how many turns this exchange used. Repair round trips are
	// **not** turns: they are further attempts within one, bounded by the
	// repair budget, which is why a repaired turn cannot spend the cap.
	Turns int
	// Tokens is the session's cumulative reported usage, not this exchange's.
	Tokens journal.TokenCounts
}
