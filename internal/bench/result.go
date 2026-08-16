package bench

import (
	"context"
	"time"

	"github.com/leejianrong/kopicode/internal/journal"
)

// Bucket is SLICE-1 §9's three-bucket failure attribution (ADR-0006 §3).
//
// The values are declared here, and nothing in this file assigns one. The rule
// that decides between them is derived from journal events and is KAN-797's
// card; what this card owes it is a place to plug in and a result that carries
// everything the derivation needs — which is the journal path, the stop reason
// and the oracle's verdict, all below.
type Bucket string

const (
	// BucketUnclassified is the zero value: nothing has attributed this
	// failure yet. It is deliberately not "model". A classifier that has not
	// run must never be readable as a classifier that found nothing wrong with
	// the harness, because that is the direction that quietly flatters the
	// number the project exists to measure.
	BucketUnclassified Bucket = ""
	// BucketHarness is a failure the harness caused: repairs exhausted, a tool
	// internal error, a syntax-gate failure straight after an edit, the
	// max-turns cap, a provider error surviving retries, a panic.
	BucketHarness Bucket = "harness"
	// BucketUnattributed is a session that used the fuzzy edit fallback at any
	// point. It does not detect a misapplication; it refuses to launder one.
	BucketUnattributed Bucket = "unattributed"
	// BucketModel is everything else: a clean stop, every edit hash-anchored,
	// the syntax gate passed, and the oracle still failed.
	BucketModel Bucket = "model"
)

// Classifier assigns the three-bucket attribution to a finished task.
//
// It is an interface declared where it is consumed, and it is one method, so
// KAN-797 can satisfy it without this file changing. [Runner.Classifier] is
// nil by default and a nil classifier leaves every [TaskResult.Bucket] as
// [BucketUnclassified] — which the report prints as "unclassified" rather than
// omitting, so a run whose classifier is not wired up says so.
//
// The result is passed by value and the classifier returns a bucket rather than
// mutating anything: attribution is derived from the journal, and a classifier
// that could edit the result could also edit the evidence.
type Classifier interface {
	Classify(ctx context.Context, r TaskResult) (Bucket, error)
}

// TaskResult is one task's outcome in one arm.
//
// It is the runner's report and not the record. The record is the journal at
// [TaskResult.JournalDir]; everything here is either derived from it or is a
// fact about the run the journal has no event for — how long the oracle took,
// which worktree the task ran in, whether that worktree was reclaimed.
type TaskResult struct {
	// TaskID identifies the task in the frozen corpus. It is what [Compare]
	// pairs on.
	TaskID string
	// Passed is the oracle's verdict, and the only bit the paired test reads.
	Passed bool

	// SessionID is the journal session this task's run recorded under.
	SessionID string
	// JournalDir is the directory holding that session's record. It outlives
	// the worktree deliberately: the worktree is reclaimed and the record is
	// the thing a classifier and a post-mortem both read.
	JournalDir string

	// Stop is the loop's own reason for stopping — "completed", "max_turns",
	// "cancelled", "error" and so on. It is read off the engine's result
	// rather than re-derived.
	Stop string
	// Turns is how many turns the session used.
	Turns int
	// Tokens is the session's reported usage. Zero on the mock provider is a
	// real zero: replayed traffic carries the recorded counts.
	Tokens journal.TokenCounts
	// SessionErr is what the session failed with, empty when it did not. A
	// session error does not stop the run: the task fails, the worktree is
	// still reclaimed, and the remaining tasks still execute.
	SessionErr string
	// Panicked reports that the task's session panicked and was recovered.
	// SLICE-1 §9 lists a panic as a harness signal outright.
	Panicked bool

	// Oracle is what the task's own suite said.
	Oracle OracleResult

	// Bucket is the three-bucket attribution, [BucketUnclassified] until a
	// [Classifier] is wired up.
	Bucket Bucket

	// Worktree is the path the task ran in.
	Worktree string
	// WorktreeKept reports that it was deliberately left behind, which is what
	// --keep-worktrees asks for.
	WorktreeKept bool

	// Duration is the whole task: worktree creation, session, oracle and
	// reclamation.
	Duration time.Duration
}

// Outcome is the scorer's view of this result: the task id and the one bit
// ADR-0005's paired test is defined over.
func (r TaskResult) Outcome() TaskOutcome {
	return TaskOutcome{TaskID: r.TaskID, Passed: r.Passed}
}

// RunResult is one arm's whole run over the corpus.
type RunResult struct {
	// RunID names the run and is the prefix of every session id in it.
	RunID string
	// Arm is the (model x harness configuration x provider pin) this run
	// measured, per ADR-0007.
	Arm ArmIdentity
	// CorpusVersion and CorpusDigest are the experiment-series boundary
	// ADR-0005 makes results comparable within. They come from the corpus as
	// checked out at Commit, not from the working tree.
	CorpusVersion string
	CorpusDigest  string
	// Commit is the frozen commit every worktree was created from.
	Commit string
	// Tasks are the per-task results, in the corpus's canonical run order.
	Tasks []TaskResult
	// Reclamation is the worktree account. It is part of the result rather
	// than a log line because a run that accumulated and a run that cleaned up
	// are indistinguishable from outside unless the numbers are reported.
	Reclamation Reclamation
	// Jobs is the parallelism the run was capped at.
	Jobs int
	// Started and Duration bound the run.
	Started  time.Time
	Duration time.Duration
	// OutDir is where the journals and the temp homes were written.
	OutDir string
}

// ArmIdentity is the arm a run measured, in the three parts ADR-0007 decision 7
// says identify one, plus the build the numbers came out of.
//
// The build is here because the harness config hash deliberately excludes the
// code: two results from two different binaries would otherwise pool as one
// arm. A TreeState that is not "clean" means the run is not poolable with
// anything at all.
type ArmIdentity struct {
	ModelID           string
	HarnessConfigHash string
	HarnessConfigName string
	ProviderPin       string
	Build             journal.BuildInfo
}

// Passed is how many tasks passed their oracle.
func (r RunResult) Passed() int {
	n := 0
	for _, t := range r.Tasks {
		if t.Passed {
			n++
		}
	}
	return n
}

// Errored is how many tasks failed for a reason that is not the model's answer
// being wrong: a session error, a panic, or an oracle that could not be run.
//
// It is the runner's own health, and it is what the front end's exit code is
// decided from. A task the model simply did not solve is data; a task the
// harness could not put a question to is not.
func (r RunResult) Errored() int {
	n := 0
	for _, t := range r.Tasks {
		if t.SessionErr != "" || t.Panicked || t.Oracle.Err != nil {
			n++
		}
	}
	return n
}

// Scored turns the run into the scorer's input, so a paired comparison is
// [Compare] over two of these and nothing in between.
func (r RunResult) Scored() Arm {
	outcomes := make([]TaskOutcome, 0, len(r.Tasks))
	for _, t := range r.Tasks {
		outcomes = append(outcomes, t.Outcome())
	}
	return Arm{Name: r.Arm.ModelID + "/" + r.Arm.HarnessConfigName, Outcomes: outcomes}
}
