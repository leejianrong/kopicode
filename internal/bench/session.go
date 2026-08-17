package bench

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/lock"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/mock"
	"github.com/leejianrong/kopicode/internal/repo"
	"github.com/leejianrong/kopicode/internal/syntax"
	"github.com/leejianrong/kopicode/internal/tools"
)

// ProviderKind selects the traffic a run is driven by.
//
// It is resolved here rather than in cmd/kopibench because ADR-0003 decision 3
// allows a front end exactly three internal imports, and internal/provider is
// not one of them. A flag value crosses the boundary; a provider does not.
type ProviderKind string

const (
	// ProviderLive is the real OpenRouter client against the pinned endpoint.
	// It spends money.
	ProviderLive ProviderKind = "live"
	// ProviderMock is the replay provider serving recorded traffic at zero
	// token cost. It is what `make bench-smoke` runs and what gates a PR.
	ProviderMock ProviderKind = "mock"
)

// SmokeFixture is the recorded traffic a mock run replays for every task.
//
// One fixture for ten tasks is not a pretence that the model solved them: the
// recording reads a file that a corpus task does not have and then answers in
// prose, so every oracle fails and every failure is the model's. What the smoke
// run exercises is the runner — worktree per task off the frozen commit, temp
// HOME, oracle in a subprocess with a timeout, parallelism capped, and every
// worktree reclaimed — which is the half that has to be right before a real
// arm's numbers mean anything.
const SmokeFixture = "two_turn_native_tool_call"

// SessionSpec is one task's session, as the runner has prepared it.
type SessionSpec struct {
	// Task is the frozen manifest, read from inside the worktree.
	Task corpus.Task
	// Dir is the task's starting tree inside the worktree: the directory the
	// agent is rooted at and the only part of the checkout it can reach
	// through a tool.
	Dir string
	// Home is the task's temp HOME.
	Home string
	// OutDir is where the journal goes. It is outside the worktree on purpose
	// — the worktree is reclaimed and the record is what survives it.
	OutDir string
	// SessionID identifies the session in the journal.
	SessionID string
	// Selection is the arm.
	Selection engine.Selection
	// Build identifies the binary, which the harness config hash excludes.
	Build journal.BuildInfo
}

// SessionOutcome is what one task's session concluded. It is the runner's
// summary; the record is the journal.
type SessionOutcome struct {
	Stop   string
	Turns  int
	Tokens journal.TokenCounts
}

// Agent runs one task's session.
//
// It is the seam the runner is tested through, and it is one method because one
// is all the runner needs: everything about how a session is wired — provider,
// journal, tools, permission, syntax gate — is behind it. A test substitutes an
// agent that panics, or one that blocks until its context is cancelled, and the
// reclamation guarantees become assertions rather than claims.
type Agent interface {
	Run(ctx context.Context, spec SessionSpec) (SessionOutcome, error)
}

// EngineAgent drives the real engine loop.
type EngineAgent struct {
	// Provider decides where the traffic comes from.
	Provider ProviderKind
	// Fixture names the recording a mock run replays. Empty means
	// [SmokeFixture].
	Fixture string
}

// Run wires one session and runs it to a stop.
//
// Everything it builds is per task and closed on the way out, because ten of
// these run in one process: a leaked root handle or an unclosed journal is a
// file descriptor per task, and the run is capped on parallelism rather than on
// descriptors.
//
// # What is deliberately not wired
//
// **Snapshots.** engine.Config.Snapshots is nil here, so no TurnSnapshot events
// are written. A shadow ref per turn per task per arm is written into the ref
// store the whole repository shares and survives the worktree it describes —
// the ref half of exactly the accumulation this runner exists to prevent, for a
// tree that is deleted minutes later. A bench post-mortem reads the journal and
// the kept worktree; neither needs a ref.
func (a EngineAgent) Run(ctx context.Context, spec SessionSpec) (SessionOutcome, error) {
	prov, err := a.provider(spec)
	if err != nil {
		return SessionOutcome{}, err
	}
	return a.runSession(ctx, prov, spec)
}

// runSession is Run's body once the provider has been chosen. Split out so a
// test in this package can drive it with a hand-built mock.Provider — a native
// run_shell tool call rather than one of the two shipped fixtures — without
// teaching EngineAgent.provider a third ProviderKind for one test. See
// session_internal_test.go's TestEngineAgentRunShellSeesTheTaskTempHOME
// (KAN-874).
func (a EngineAgent) runSession(ctx context.Context, prov engine.Provider, spec SessionSpec) (SessionOutcome, error) {
	// The working tree's session lock (docs/SLICE-1.md §8), first and released
	// last, exactly as engine.Open takes it for the REPL. Two things about it
	// are worth stating here rather than leaving to be inferred.
	//
	// **It is keyed on the task's worktree, and that is what stops it
	// serialising this runner.** Each task gets its own `git worktree`, so ten
	// concurrent tasks resolve ten different work tree roots and take ten
	// different locks. Keying on the git *directory* would have been the other
	// reading of "one session per repo", and the ten worktrees share theirs —
	// that version deadlocks the corpus at the first task. spec.Dir is inside
	// the worktree (the task's starting tree), so the root is resolved rather
	// than assumed, which also keeps .kopicode/ out of bench/tasks/ and away
	// from the corpus digest.
	//
	// **kopibench does not lock the tree it was launched from.** It never edits
	// it — every session runs in a worktree — and locking it would make a paired
	// A/B, which ADR-0005 defines as two invocations, impossible to run side by
	// side from one checkout.
	held, err := lock.Acquire(repo.WorkTreeRoot(ctx, spec.Dir), lock.Holder{
		Session: spec.SessionID,
		Record:  journal.SessionDir(spec.OutDir, spec.SessionID),
	})
	if err != nil {
		return SessionOutcome{}, fmt.Errorf("bench: locking the working tree for %s: %w", spec.Task.ID, err)
	}
	// Deferred rather than released at the end of the body: runTask recovers a
	// panic to keep one task from taking the other nine down with it, and a
	// lock released only on the happy path would survive that recovery and be
	// held for the rest of the process.
	defer func() { _ = held.Release() }()

	set, err := tools.NewSet(spec.Dir)
	if err != nil {
		return SessionOutcome{}, fmt.Errorf("bench: opening the tool set for %s: %w", spec.Task.ID, err)
	}
	defer func() { _ = set.Close() }()
	// The temp HOME goes on the tool set too, and not only on the oracle
	// (oracleEnv in oracle.go): without it, model-authored run_shell sees the
	// operator's real HOME while the suite that judges the task runs against a
	// clean one, and a task can "pass" only because its shell reached the
	// operator's ~/.gitconfig or toolchain caches — a result that will not
	// reproduce elsewhere (KAN-874). See tools.Set.Home and childEnv in
	// internal/tools/shell.go.
	set.Home = spec.Home

	// The bench policy auto-approves inside the task's worktree and denies
	// outside it. It exists in internal/permission precisely so the headless
	// runner does not inherit an interactive dependency it cannot satisfy, and
	// so the usual fix for that — a mode flag that approves everything — is not
	// the thing standing between one task's run and the next task's tree.
	pol, err := permission.NewBench(set.Root.Path(), set.Root.Resolver())
	if err != nil {
		return SessionOutcome{}, fmt.Errorf("bench: building the permission policy for %s: %w", spec.Task.ID, err)
	}
	gate, err := permission.New(set.Root.Path(), set.Root.Resolver(), pol)
	if err != nil {
		return SessionOutcome{}, fmt.Errorf("bench: building the permission gate for %s: %w", spec.Task.ID, err)
	}

	jrn, err := journal.Open(spec.OutDir, spec.SessionID)
	if err != nil {
		return SessionOutcome{}, fmt.Errorf("bench: opening the journal for %s: %w", spec.Task.ID, err)
	}
	defer func() { _ = jrn.Close() }()

	eng, err := engine.New(engine.Config{
		SessionID:   spec.SessionID,
		Selection:   spec.Selection,
		Build:       spec.Build,
		CWD:         spec.Dir,
		Provider:    prov,
		Journal:     jrn,
		Tools:       set,
		Permissions: gate,
		Syntax:      &syntax.Gate{Root: set.Root.Path()},
	})
	if err != nil {
		return SessionOutcome{}, fmt.Errorf("bench: building the engine for %s: %w", spec.Task.ID, err)
	}

	if err := eng.Start(ctx); err != nil {
		return SessionOutcome{}, fmt.Errorf("bench: starting the session for %s: %w", spec.Task.ID, err)
	}

	res, runErr := eng.Run(ctx, spec.Task.Statement)

	// Close writes SessionEnded whatever Run said, and the journal ignores a
	// cancelled context on Append, so a task killed by Ctrl-C still records why
	// it ended. A close failure is reported alongside the run's own error
	// rather than in place of it.
	closeErr := eng.Close(ctx)

	out := SessionOutcome{Stop: res.Stop.Reason(), Turns: res.Turns, Tokens: res.Tokens}
	return out, errors.Join(runErr, closeErr)
}

// provider builds this task's provider. A mock run gets its own replay per
// task: the cursor is per fixture, and ten tasks sharing one would exhaust it
// on the second.
func (a EngineAgent) provider(spec SessionSpec) (engine.Provider, error) {
	switch a.Provider {
	case ProviderMock:
		name := a.Fixture
		if name == "" {
			name = SmokeFixture
		}
		p, err := mock.Load(name)
		if err != nil {
			return nil, fmt.Errorf("bench: loading the replay fixture %q: %w", name, err)
		}
		return p, nil
	case ProviderLive:
		// The credential is read here rather than held on the runner: an
		// APIKey that travelled through a config struct is an APIKey that can
		// be printed by a %v on that struct, which is the incidental leak the
		// type exists to prevent.
		key, err := provider.APIKeyFromEnv()
		if err != nil {
			return nil, fmt.Errorf("bench: %w", err)
		}
		c, err := provider.NewClient(key)
		if err != nil {
			return nil, fmt.Errorf("bench: building the provider client: %w", err)
		}
		return c, nil
	default:
		return nil, fmt.Errorf("bench: unknown provider %q, want %q or %q: %w",
			a.Provider, ProviderLive, ProviderMock, ErrNotConfigured)
	}
}

// taskRepoDir is the task's starting tree inside a worktree: the worktree root,
// then the corpus's path relative to the repository, then the task and its
// repo/ directory.
//
// The agent is rooted here and not at the worktree root, which is what keeps
// the reference solutions in bench/_solutions out of reach of every tool: they
// are outside the corpus tree for that reason, and a root one level up would
// have made the precaution decorative.
func taskRepoDir(worktree, corpusRel, taskID string) string {
	return filepath.Join(worktree, filepath.FromSlash(corpusRel), taskID, corpus.RepoDirName)
}
