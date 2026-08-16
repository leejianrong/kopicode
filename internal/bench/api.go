package bench

import (
	"context"
	"fmt"

	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// BuildIdentity is the binary's own identity, in the four plain strings
// internal/build produces.
//
// It is declared here, in plain strings, because a front end may import
// internal/build and internal/bench and nothing else (ADR-0003 decision 3):
// internal/journal's BuildInfo is out of its reach, and internal/build's Info
// carries a defined TreeState type that no struct conversion will cross. So the
// surface reads its identity and hands over four strings, and the mapping onto
// the journal's wire contract happens on this side of the boundary — where the
// journal is importable and where a test can hold the two shapes to each other.
type BuildIdentity struct {
	Version   string
	Commit    string
	TreeState string
	Source    string
}

func (b BuildIdentity) journalInfo() journal.BuildInfo {
	return journal.BuildInfo{
		Version:   b.Version,
		Commit:    b.Commit,
		TreeState: b.TreeState,
		Source:    b.Source,
	}
}

// Options are one benchmark invocation, as a front end describes it.
//
// It is a struct rather than a parameter list because every field is a decision
// a report has to be able to name: which corpus, which arm, which commit, how
// much parallelism, whether the worktrees were kept.
type Options struct {
	// CorpusDir is the frozen corpus. Required.
	CorpusDir string
	// Selection is the resolved arm (ADR-0007), produced by
	// engine.ResolveSelection. Required.
	Selection engine.Selection
	// Build identifies the binary the numbers came out of.
	Build BuildIdentity
	// Provider is where model traffic comes from.
	Provider ProviderKind
	// Fixture names the recording a mock run replays. Empty means
	// [SmokeFixture].
	Fixture string
	// Commit is the frozen commit worktrees are created from. Empty means HEAD.
	Commit string
	// Jobs caps parallelism. Zero means [DefaultJobs].
	Jobs int
	// KeepWorktrees leaves every worktree behind for a post-mortem.
	KeepWorktrees bool
}

// RunCorpus loads the corpus and runs it against one arm.
//
// It is the whole surface a front end needs, and it exists so that cmd/kopibench
// holds flag parsing, printing and an exit code and nothing else. Everything it
// assembles — the corpus loader, the provider, the engine, the worktrees — is
// behind ADR-0003's boundary on purpose.
//
// The corpus is loaded before anything else and its failure is returned before a
// worktree exists or a request is billed: corpus.Load refuses a corpus whose
// contents no longer match its recorded digest, which is ADR-0005's
// experiment-series boundary made checkable, and discovering it after ten tasks
// have run would have cost money to learn.
//
// A non-nil result with a non-nil error is normal and deliberate: a run that
// failed part way still has results for the tasks that finished, and the report
// is written from them.
func RunCorpus(ctx context.Context, opts Options) (*RunResult, error) {
	c, err := corpus.Load(opts.CorpusDir)
	if err != nil {
		return nil, fmt.Errorf("bench: %w", err)
	}

	r := &Runner{
		Corpus:        c,
		Selection:     opts.Selection,
		Build:         opts.Build.journalInfo(),
		Agent:         EngineAgent{Provider: opts.Provider, Fixture: opts.Fixture},
		Commit:        opts.Commit,
		Jobs:          opts.Jobs,
		KeepWorktrees: opts.KeepWorktrees,
	}
	return r.Run(ctx)
}
