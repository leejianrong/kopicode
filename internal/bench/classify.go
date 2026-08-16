package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
	"github.com/leejianrong/kopicode/internal/verify"
)

// ErrNoRecord reports a failing task whose journal could not be read.
//
// It is an error and not a bucket, because every bucket is a claim about what
// went wrong and a record nobody could read supports none of them. The runner
// logs it and leaves the result [BucketUnclassified], which the report prints —
// so an unreadable record is visible as "nobody looked" rather than absorbed
// into whichever bucket happened to be the default.
var ErrNoRecord = errors.New("the session record could not be read")

// Attribution is SLICE-1 §9's three-bucket failure classifier, derived from
// journal events and never judged.
//
// # What it reads
//
// The session's journal at [TaskResult.JournalDir], plus the two facts about a
// task that the journal cannot hold because they happen outside a session: a
// panic the runner recovered, and the oracle's own execution. Nothing else. It
// asks no model, matches no prose, and infers nothing from an event's text
// fields — every rule below is a typed field of a typed event.
//
// # The buckets, and the precedence between them
//
// A session can trip several rules at once, and SLICE-1 §9 does not say which
// wins. The order is decided here and it is:
//
//  0. nothing to attribute — the oracle passed, or the run was cancelled
//  1. harness
//  2. unattributed
//  3. model
//
// **Nothing to attribute comes first** because attribution is *failure*
// attribution: ADR-0006 §3 taints "any failing session" and SLICE-1's
// acceptance bar counts "zero failures classified harness". A task whose oracle
// passed did not fail, and a cancelled task was abandoned rather than answered
// — charging either to a bucket would put rows into a tally that is supposed to
// count failures. Both come back [BucketUnclassified].
//
// **`harness` beats `unattributed`.** The two buckets make opposite-strength
// claims: `harness` says we know the failure was ours, `unattributed` says
// nobody can tell. Letting the fuzzy taint swallow a max-turns cap or a tool
// internal error would move a *known* harness failure out of the one bucket
// with an acceptance bar of zero, which is the flattering direction and the one
// direction ADR-0006 exists to forbid. So a session that used the fuzzy
// fallback *and* exhausted its repairs is `harness`, not `unattributed`.
//
// **`unattributed` beats `model`**, which is ADR-0006 §3 itself: the taint
// applies "at any point", regardless of how the session ended, precisely
// because a fuzzy match above the floor and in the wrong place produces a clean
// stop and no error at all. That is the case the bucket exists for, so a clean
// stop must not outrank it.
//
// # `harness`
//
// Any of, in the card's own order:
//
//   - repairs exhausted — a [journal.ToolCallFailed]. Every route to that event
//     is either the repair budget spent or a classification the repairer
//     refuses to spend a round trip on, and internal/parse's own doc calls the
//     unrepairable ones harness bugs;
//   - a tool internal error — a [journal.ToolResult] whose ErrorKind is
//     internal/tools' "internal". A task-level refusal and a cancellation are
//     deliberately not read here;
//   - a syntax-gate failure straight after an edit — a [journal.SyntaxGateRun]
//     that ran and exited non-zero, immediately following a
//     [journal.EditApplied]. "Immediately" is literal: the engine writes the two
//     adjacently with nothing between them, and a looser rule would catch a gate
//     failure the model's own later edit caused;
//   - the max-turns cap, and a provider error surviving retries — both read off
//     [TaskResult.Stop]. See stopIsHarness below for the whole table;
//   - a panic — [TaskResult.Panicked], which the runner recovers per task;
//   - a verification that could not run for a reason that is not the project's
//     shape — see verifyNotRun below;
//   - an oracle that produced no verdict — see oracleFailed below.
//
// # `unattributed`
//
// A [journal.EditApplied] or [journal.EditRejected] whose Mode is
// internal/tools' fuzzy mode. Applied *and* rejected, because the trigger is
// that the fallback was used at all: internal/tools/fuzzy.go records the mode on
// every result it produces for exactly this reason.
//
// # `model`
//
// Everything else.
type Attribution struct{}

var _ Classifier = Attribution{}

// Classify assigns r's bucket. See [Attribution] for the rules and the
// precedence between them.
//
// The error is [ErrNoRecord] when a failing task's journal could not be read.
// It is never returned for a task there is nothing to attribute — those need no
// record, and a passing task whose journal a later cleanup removed must not
// start reporting an error.
func (Attribution) Classify(ctx context.Context, r TaskResult) (Bucket, error) {
	// Rule 0. Nothing failed, or nothing was answered.
	if r.Passed || r.Stop == engine.StopCancelled.Reason() {
		return BucketUnclassified, nil
	}

	// The result-level harness signals are decided without the record, because
	// a task that panicked or never reached a session may have no record at
	// all. They are not returned early: `harness` outranks `unattributed`, so
	// finding one here saves a read but finding none proves nothing yet.
	harness := r.Panicked || stopIsHarness(r.Stop) || oracleFailed(r.Oracle)

	sig, err := readSignals(ctx, r.JournalDir)
	if err != nil {
		if harness {
			// The record is unreadable and the bucket is already decided. Say
			// so from what is known rather than refusing to attribute a
			// failure the runner itself observed.
			return BucketHarness, nil
		}
		return BucketUnclassified, err
	}

	switch {
	case harness || sig.harness:
		return BucketHarness, nil
	case sig.fuzzy:
		return BucketUnattributed, nil
	default:
		return BucketModel, nil
	}
}

// stopIsHarness reads [TaskResult.Stop], which is internal/engine's Stop
// rendered as journal.SessionEnded.Reason.
//
// It is written as the complement — the reasons that are *not* the harness's —
// so that a stop this build does not recognise fails closed. That matches
// engine.Stop.ExitCode, which exits 4 for an unmapped stop for the same reason:
// an unrecognised outcome is a harness defect, and reading it as "everything
// else" would charge it to the model.
//
//	completed            not harness — the clean stop
//	budget_exhausted     not harness — internal/engine/stop.go argues this one
//	                     into "everything else" explicitly, and says why it may
//	                     be wrong; changing it is a card, not a line here
//	verification_failed  not harness — the suite ran, the model was shown the
//	                     failure, and it stopped anyway (KAN-787)
//	cancelled            handled above: neither bucket
//	max_turns            harness — named outright in SLICE-1 §9
//	error                harness — internal/engine renders both a provider error
//	                     surviving retries and its own breakage as "error", and
//	                     §9 charges both to the harness, so nothing is lost by
//	                     them sharing a reason here
func stopIsHarness(stop string) bool {
	switch stop {
	case engine.StopCompleted.Reason(),
		engine.StopBudgetExhausted.Reason(),
		engine.StopVerificationFailed.Reason(),
		engine.StopCancelled.Reason():
		return false
	default:
		return true
	}
}

// oracleFailed reports that the task's suite produced no verdict.
//
// This rule is not in SLICE-1 §9's list and is here on the same argument the
// list is built from. An oracle that could not be started, that outlived its
// timeout, or that was killed by a signal leaves [TaskResult.Passed] false with
// a session that may be spotless — so without this rule the harness's own
// inability to ask the question lands in `model`, which is precisely the
// laundering ADR-0006 §3 exists to prevent. [RunResult.Errored] already counts
// these; that count is the runner's health and this is the attribution, and a
// number in one place cannot be read from the other.
func oracleFailed(o OracleResult) bool {
	return o.Err != nil || o.TimedOut || o.Signal != ""
}

// signals are the facts the rules read out of one session's record.
//
// harness is a single bit rather than one per rule because the bucket is a
// single value and nothing downstream asks which rule fired. What fired is in
// the journal, which is where a post-mortem reads it.
type signals struct {
	harness bool
	fuzzy   bool
}

// readSignals walks a session's journal once and reports what the rules found.
//
// It reads the JSONL directly rather than through journal.Open, which opens the
// record for *appending*: creating and fsyncing a session directory is not
// something a classifier may do to the record it is describing, and a journal
// created as a side effect of reading would make "the session wrote nothing"
// and "the session never ran" the same bytes. Every field the rules read —
// a mode, an error kind, an exit code, a source — is a scalar on the line, so
// nothing here needs the blob rehydration that journal.Read performs.
func readSignals(ctx context.Context, dir string) (signals, error) {
	var sig signals
	if dir == "" {
		return sig, fmt.Errorf("bench: no journal directory on the result: %w", ErrNoRecord)
	}

	path := filepath.Join(dir, journal.EventsFile)
	f, err := os.Open(path)
	if err != nil {
		return sig, fmt.Errorf("bench: opening %s: %w: %w", path, err, ErrNoRecord)
	}
	defer func() { _ = f.Close() }()

	// bufio.Reader and not bufio.Scanner: an event line carries tool output
	// inline up to the journal's blob threshold, which is larger than
	// Scanner's default limit, and a Scanner that gave up on a long line would
	// turn an oversized tool result into an unclassifiable session.
	br := bufio.NewReader(f)
	var prev journal.Type

	for line := 1; ; line++ {
		if err := ctx.Err(); err != nil {
			return sig, fmt.Errorf("bench: reading %s: %w", path, err)
		}

		chunk, rerr := br.ReadBytes('\n')
		if len(chunk) > 0 && rerr == nil {
			var ev journal.Event
			if err := json.Unmarshal(chunk, &ev); err != nil {
				return sig, fmt.Errorf("bench: %s line %d: %w: %w", path, line, err, ErrNoRecord)
			}
			apply(&sig, &prev, ev.Payload)
			continue
		}
		if errors.Is(rerr, io.EOF) {
			if len(chunk) > 0 {
				// A final line with no terminator is a write a crash caught in
				// flight. internal/journal refuses to parse it and so does
				// this: a partial line that happens to be valid JSON parses
				// perfectly and is silently wrong.
				return sig, fmt.Errorf(
					"bench: %s line %d has no newline terminator, so the record ends mid-write: %w",
					path, line, ErrNoRecord)
			}
			return sig, nil
		}
		return sig, fmt.Errorf("bench: reading %s: %w: %w", path, rerr, ErrNoRecord)
	}
}

// apply folds one event into the signals, and remembers its type so the next
// event can ask what preceded it.
func apply(sig *signals, prev *journal.Type, payload journal.Payload) {
	if payload == nil {
		return
	}
	switch p := payload.(type) {
	case journal.ToolCallFailed:
		sig.harness = true
	case journal.ToolResult:
		if p.ErrorKind == tools.FaultInternal.String() {
			sig.harness = true
		}
	case journal.SyntaxGateRun:
		// Only a gate that *ran* and rejected the file counts. A gate with no
		// checker for the language records Ran false and ExitCode 0, and
		// reading that as a pass or as a failure would both be inventions.
		if *prev == journal.TypeEditApplied && p.Ran && p.ExitCode != 0 {
			sig.harness = true
		}
	case journal.VerificationRun:
		if verifyNotRun(p) {
			sig.harness = true
		}
	case journal.EditApplied:
		if p.Mode == tools.ModeFuzzy {
			sig.fuzzy = true
		}
	case journal.EditRejected:
		if p.Mode == tools.ModeFuzzy {
			sig.fuzzy = true
		}
	}
	*prev = payload.Type()
}

// verifyNotRun reports a forced verification that reached no verdict for a
// reason the harness owns.
//
// KAN-787 landed forced verification with a deliberate asymmetry: a command
// that ran and exited non-zero blocks a success report and is charged to the
// *model*, while a verification that could not run does not block at all. That
// second case is not one thing, and the split matters more than it looks:
//
//   - no command configured and none discovered (verify.SourceNone) is a
//     legitimate project shape, not a defect. Not harness;
//   - a missing toolchain, a command that could not be started, a signal, or a
//     timeout is the harness proceeding with no evidence, and SLICE-1's
//     acceptance bar of zero `harness` failures would be flattered by every one
//     of them. Harness.
//
// **What the record can and cannot say.** internal/verify distinguishes all of
// these on verify.Result.Skip. journal.VerificationRun has no field for it, so
// what survives to here is Source plus an ExitCode that internal/verify
// guarantees is -1, never 0, for anything that did not conclude. That is enough
// to separate the legitimate shape from the rest and not enough to separate the
// rest from each other, so this rule is the conservative one the missing field
// forces: any not-run with a command behind it is charged to the harness. A
// cancelled verification would land here too and does not, because a cancelled
// session is filtered before any of this runs.
func verifyNotRun(v journal.VerificationRun) bool {
	return v.Source != string(verify.SourceNone) && v.ExitCode < 0
}
