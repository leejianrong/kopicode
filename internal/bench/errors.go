package bench

import (
	"errors"
	"strings"
)

// Sentinels a caller can branch on. Compare with errors.Is; the detail is on
// *[Error], reachable with errors.As.
//
// Every one of these is a refusal to score rather than a warning about a score.
// ADR-0005's whole argument is that an unpaired or unpinned comparison is not
// evidence, so an input the scorer cannot pair is an error and never a quietly
// dropped row: a task silently missing from one arm shifts the discordant
// counts, which is the only thing the test reads.
var (
	// ErrUnpaired reports a task present in one arm and absent from the
	// other. The arms must run the identical task set (ADR-0005 decision 1).
	ErrUnpaired = errors.New("task is not present in both arms")

	// ErrDuplicateTask reports the same task id twice within one arm. A
	// second result for a task is either a re-run that should have replaced
	// the first or a corpus with two tasks sharing an id; both need a human,
	// and picking one silently would double-count it.
	ErrDuplicateTask = errors.New("task appears more than once in one arm")

	// ErrUnnamedTask reports a result with an empty task id. Pairing is by
	// id, so an unnamed result cannot be paired with anything.
	ErrUnnamedTask = errors.New("task has no id")

	// ErrNoPairs reports a comparison over no tasks at all.
	ErrNoPairs = errors.New("no paired tasks to compare")

	// ErrNegativeCount reports a contingency table cell below zero.
	ErrNegativeCount = errors.New("contingency table cell is negative")
)

// Sentinels the runner refuses on. They are separate from the scoring ones
// above because they answer a different question: the scorer refuses to *score*
// an input it cannot pair, and these refuse to *run* at all.
//
// Every one of them is a refusal before any provider request is made. A
// benchmark that discovers its corpus has drifted, or that it cannot create a
// worktree, after N tasks have been billed has spent money to learn something
// it could have checked for free.
var (
	// ErrNoWorkingDir reports a subprocess with no working directory named.
	// Git walks upward until it finds a repository, so a command with no
	// directory operates on one nobody chose.
	ErrNoWorkingDir = errors.New("no working directory")

	// ErrReclaim reports a worktree that could not be removed. It is an error
	// rather than a warning because the disk it holds is the failure this card
	// exists to prevent, and a run that quietly accumulates looks exactly like
	// a run that cleaned up.
	ErrReclaim = errors.New("worktree could not be reclaimed")

	// ErrWorktreeCreate reports a task that never got a worktree, so nothing
	// about it was measured.
	//
	// It is a refusal rather than a note on one row because of how it presents:
	// a run that created nine checkouts for a ten-task corpus finishes, prints a
	// report, and looks healthy — the counts are symmetric, the reclamation is
	// clean, and the missing task is one line among ten. The corpus is the unit
	// of comparison (ADR-0005 decision 1), so a run that measured nine tasks is
	// not a run with one bad row, it is a run of a different corpus.
	ErrWorktreeCreate = errors.New("worktree could not be created")

	// ErrCorpusDrift reports a corpus whose contents at the frozen commit do
	// not match the corpus the run was pointed at. Results are only comparable
	// within one frozen corpus (ADR-0005 §Consequences), so this is a refusal
	// rather than a warning.
	ErrCorpusDrift = errors.New("the corpus at the frozen commit is not the corpus that was loaded")

	// ErrNotConfigured reports a Runner missing something it cannot invent.
	ErrNotConfigured = errors.New("runner is not configured")

	// ErrTaskPanic reports a task whose session panicked. The panic is
	// recovered per task so the remaining tasks still run and every worktree is
	// still reclaimed; the fact travels on the result, where SLICE-1 §9 charges
	// it to the harness.
	ErrTaskPanic = errors.New("task panicked")
)

// Error is a scoring failure, with enough detail to fix the input.
//
// Tasks lists every offending task id rather than the first one found, and the
// message prints all of them. A pairing failure is usually systematic — a whole
// arm cut short by a crash, a corpus version mismatch — and reporting one id
// out of thirty turns one fix into thirty runs of the same fix.
type Error struct {
	// Op is the operation that refused: "compare" or "mcnemar".
	Op string
	// Arm names the arm the problem was found in, "" when it belongs to
	// neither or to both.
	Arm string
	// Tasks lists every task id involved, sorted, empty when the failure is
	// not about particular tasks.
	Tasks []string
	// Detail says what was wrong.
	Detail string

	err error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	if e.err != nil {
		b.WriteString(e.err.Error())
	}
	if e.Arm != "" {
		b.WriteString(": arm ")
		b.WriteString(quote(e.Arm))
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if len(e.Tasks) > 0 {
		b.WriteString(": ")
		b.WriteString(strings.Join(e.Tasks, ", "))
	}
	if b.Len() == 0 {
		return "bench: unspecified scoring failure"
	}
	return b.String()
}

// Unwrap exposes the cause, one of this package's sentinels.
func (e *Error) Unwrap() error { return e.err }

func quote(s string) string { return `"` + s + `"` }
