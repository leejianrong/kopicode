// Package bench is the headless benchmark: the runner that executes the frozen
// corpus against one arm, and the paired scorer that compares two arms.
//
// # The two halves
//
// The scorer (pair.go, mcnemar.go) takes outcomes as data and knows nothing
// about how they were produced. The runner (this file, worktree.go, oracle.go,
// session.go) produces them. They meet at [TaskOutcome] and nowhere else, which
// is what lets the statistics be tested against known contingency tables
// without a provider, a corpus or a git repository in sight.
//
// # What a run is
//
// One arm — (model x harness configuration x provider pin), per ADR-0007 — over
// the frozen corpus. Every task gets a git worktree of its own, checked out
// detached at the frozen commit, plus a temp HOME. The agent is rooted at the
// task's starting tree inside that worktree; when it stops, the task's own test
// suite runs as a subprocess with the manifest's timeout, and its exit status
// is the whole verdict (ADR-0005 decision 5).
//
// # Worktrees are reclaimed, and the reclamation is reported
//
// Ten checkouts per arm, and an A/B series is many arms. Left alone that fills
// a disk quietly, and an agent harness that auto-creates worktrees is the
// textbook offender. So:
//
//   - removal is deferred on every path — normal, error, panic and
//     cancellation. The panic path works because a deferred call runs while the
//     panic unwinds, and the runner recovers per task so the other nine tasks
//     are reclaimed too. The cancellation path works because
//     [Worktrees.Release] detaches from the run's context before it runs a
//     single git command;
//   - [Worktrees.Reclaim] runs at the start of every run and takes back what a
//     previous crash orphaned, in both the forms an orphan takes;
//   - --keep-worktrees leaves them for a post-mortem;
//   - and the counts are in [RunResult.Reclamation], because silent cleanup and
//     silent accumulation look identical from outside.
//
// # Failure attribution, and the order the rules are applied in
//
// A failing task is charged to one of SLICE-1 §9's three buckets by
// [Attribution], derived from journal events and never judged. A session can
// trip several rules at once and §9 does not say which wins, so the order is
// decided here:
//
//  0. nothing to attribute  — the oracle passed, or the task was cancelled
//  1. harness               — a defect this project owns
//  2. unattributed          — the fuzzy edit fallback was used at any point
//  3. model                 — everything else
//
// `harness` outranks `unattributed` because the two make opposite-strength
// claims: one says we know the failure was ours, the other says nobody can
// tell. Letting the taint swallow a known harness failure would move it out of
// the only bucket with an acceptance bar of zero, which is the flattering
// direction. `unattributed` outranks `model` because ADR-0006 §3 says so — the
// taint applies at any point, whatever the session's stop, precisely because
// the misapplication it exists for produces a clean one. [Attribution] carries
// the full argument and [BucketCounts] the tally the report prints.
//
// # This is not a security boundary
//
// Model-authored shell runs in a worktree, not a container. The temp HOME
// contains accidents, not intent. Do not point this at untrusted tasks or run
// it on a machine you care about; isolation is slice 3 (docs/SLICE-1.md §9).
package bench

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/repo"
)

// DefaultJobs is the parallelism cap when none is given.
//
// Capped, and capped low. Each job holds a git worktree, an agent session and,
// while the oracle runs, a compiler — so the binding constraint is memory and
// disk rather than cores, and an uncapped run over a ten-task corpus is ten
// checkouts and ten toolchains at once. Four is a cap that keeps a laptop
// usable and a CI runner alive; --jobs raises it deliberately.
const DefaultJobs = 4

// RunsSubdir is where a run's durable output goes, relative to the
// repository's kopicode state directory: one directory per run, one per task
// inside it, holding the journal and the temp HOME.
//
// It is separate from [WorktreeSubdir] on purpose. The worktree is reclaimed
// and the record is not: a journal written inside the checkout would be deleted
// by the cleanup that this card exists to guarantee, which would leave the run
// with nothing to classify and nothing to review.
const RunsSubdir = "bench/runs"

// Runner executes the corpus against one arm.
//
// Everything variable is a field rather than a constant: the parallelism cap,
// the clock, the agent and the classifier are all injected, so the reclamation
// guarantees can be driven by an agent that panics or blocks rather than
// asserted about one that does not.
type Runner struct {
	// Corpus is the loaded, digest-verified corpus. Required.
	Corpus *corpus.Corpus

	// Selection is the resolved arm (ADR-0007), produced by
	// engine.ResolveSelection before the runner is built. Required.
	Selection engine.Selection

	// Build identifies the binary the numbers came out of.
	Build journal.BuildInfo

	// Agent runs one task's session. Required.
	Agent Agent

	// Commit is the frozen commit worktrees are created from. Empty means
	// HEAD of the repository holding the corpus.
	Commit string

	// Jobs caps parallelism. Zero means [DefaultJobs].
	Jobs int

	// KeepWorktrees leaves every worktree behind for a post-mortem, and says
	// so in the report. The next run reclaims them: the worktree directory is
	// per task and not per run, so inspect before you re-run.
	KeepWorktrees bool

	// Classifier assigns the three-bucket attribution. Nil leaves every result
	// [BucketUnclassified], which the report prints rather than hides.
	Classifier Classifier

	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// Log receives engine-internal diagnostics — worktree lifecycle, task
	// start and finish. Never session content: the journal owns that. Nil
	// means slog.Default.
	Log *slog.Logger

	// caches are resolved once per run so a temp HOME does not force a cold
	// toolchain cache in every task. See [resolveGoCaches].
	caches goCaches
}

// Run executes the whole corpus and returns the arm's result.
//
// The error is non-nil only when the run could not be carried out: a corpus
// that drifted from the frozen commit, a repository that cannot be resolved, a
// worktree directory that cannot be reclaimed. A task that failed — its session
// errored, its oracle said no, it panicked — is *data*, and it comes back on
// [RunResult] with the other nine. A runner that aborted the corpus on the first
// failing task would make every partial run unpairable, which is the one thing
// ADR-0005 §1 cannot tolerate.
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	now := r.now()
	started := now()

	src, err := repo.Open(ctx, r.Corpus.Root)
	if err != nil {
		return nil, fmt.Errorf("bench: the corpus at %s is not in a git repository, so there is "+
			"no frozen commit to check tasks out from: %w", r.Corpus.Root, err)
	}
	// The throwaway index, the run output and the worktrees all live under
	// .kopicode/, and git must not see any of them. In a linked worktree only
	// $GIT_COMMON_DIR/info/exclude is read, which is why this is repo's call to
	// make and not a file this package writes.
	if err := src.ExcludeStateDir(); err != nil {
		return nil, fmt.Errorf("bench: excluding the state directory: %w", err)
	}

	commit, err := r.resolveCommit(ctx, src.Root())
	if err != nil {
		return nil, err
	}
	corpusRel, err := filepath.Rel(src.Root(), r.Corpus.Root)
	if err != nil {
		return nil, fmt.Errorf("bench: locating the corpus inside %s: %w", src.Root(), err)
	}

	runID := runID(started)
	outDir := filepath.Join(src.StatePath(), filepath.FromSlash(RunsSubdir), runID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("bench: creating the run directory: %w", err)
	}

	trees := NewWorktrees(src.Root(), src.StatePath(), r.KeepWorktrees)
	// Prune before anything is created, not after: what a previous crash left
	// is sitting at the paths this run is about to ask for.
	if err := trees.Reclaim(ctx); err != nil {
		return nil, err
	}
	if caches, err := resolveGoCaches(ctx, src.Root()); err != nil {
		r.log().Debug("could not resolve the go build cache; oracles will use the temp home", "error", err)
	} else {
		r.caches = caches
	}

	result := &RunResult{
		RunID: runID,
		Arm: ArmIdentity{
			ModelID:           r.Selection.ModelID,
			HarnessConfigHash: r.Selection.HarnessConfigHash,
			HarnessConfigName: r.Selection.Config.Name,
			ProviderPin:       r.Selection.Pin.String(),
			Build:             r.Build,
		},
		CorpusVersion: r.Corpus.Version,
		CorpusDigest:  r.Corpus.Digest,
		Commit:        commit,
		Jobs:          r.jobs(),
		Started:       started,
		OutDir:        outDir,
	}

	result.Tasks = r.runTasks(ctx, taskEnv{
		trees:     trees,
		repoRoot:  src.Root(),
		corpusRel: filepath.ToSlash(corpusRel),
		commit:    commit,
		runID:     runID,
		outDir:    outDir,
	})
	result.Reclamation = trees.Counts()
	result.Duration = now().Sub(started)

	if failed := result.Reclamation.Failed; len(failed) > 0 {
		return result, fmt.Errorf("bench: %d worktree(s) were left behind: %s: %w",
			len(failed), strings.Join(failed, ", "), ErrReclaim)
	}
	return result, nil
}

// taskEnv is what every task in a run shares.
type taskEnv struct {
	trees     *Worktrees
	repoRoot  string
	corpusRel string
	commit    string
	runID     string
	outDir    string
}

// runTasks executes the corpus with parallelism capped, and returns the results
// in the corpus's canonical run order.
//
// The order matters and is restored rather than relied on: ADR-0005 §4's
// sequential early stopping runs tasks in a fixed order, and a report whose rows
// arrived in whatever order the workers finished would make two runs of the same
// corpus look different.
func (r *Runner) runTasks(ctx context.Context, env taskEnv) []TaskResult {
	tasks := r.Corpus.Tasks
	results := make([]TaskResult, len(tasks))

	sem := make(chan struct{}, r.jobs())
	var wg sync.WaitGroup

	for i, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// A cancelled run still records every task, so a partial run is
			// legible rather than a slice of zero values. It does not start
			// work it cannot finish.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = cancelledResult(task.ID, ctx.Err())
				return
			}
			if ctx.Err() != nil {
				results[i] = cancelledResult(task.ID, ctx.Err())
				return
			}
			results[i] = r.runTask(ctx, env, task)
		}()
	}
	wg.Wait()
	return results
}

func cancelledResult(id string, err error) TaskResult {
	return TaskResult{
		TaskID:     id,
		Stop:       engine.StopCancelled.Reason(),
		SessionErr: err.Error(),
		Oracle:     OracleResult{ExitCode: -1},
	}
}

// runTask is one task, end to end: worktree, session, oracle, reclamation.
//
// The reclamation is deferred first, immediately after the worktree exists, so
// there is no statement between creation and the guarantee of removal. Every
// later failure — including a panic, which is recovered here rather than allowed
// to take the process and the other nine tasks' worktrees with it — unwinds
// through it.
func (r *Runner) runTask(ctx context.Context, env taskEnv, task corpus.Task) (res TaskResult) {
	now := r.now()
	started := now()
	log := r.log().With("task", task.ID, "run", env.runID)

	res = TaskResult{
		TaskID:    task.ID,
		SessionID: env.runID + "-" + task.ID,
		Oracle:    OracleResult{ExitCode: -1},
	}
	defer func() {
		res.Duration = now().Sub(started)
		// Only a worktree that exists can have been kept. A task that failed
		// before one was created must not report that one is waiting to be
		// inspected.
		res.WorktreeKept = r.KeepWorktrees && res.Worktree != ""
		if p := recover(); p != nil {
			// A panic is a harness signal (SLICE-1 §9) and is recorded as one
			// rather than crashing the run. The stack goes to the log because
			// it is a diagnostic, not session content.
			res.Panicked = true
			res.SessionErr = fmt.Sprintf("%v: %v", ErrTaskPanic, p)
			res.Stop = engine.StopHarnessError.Reason()
			log.Error("task panicked", "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
		res.Bucket = r.classify(ctx, res, log)
	}()

	worktree, err := env.trees.Add(ctx, task.ID, env.commit)
	if err != nil {
		res.SessionErr = err.Error()
		res.Stop = engine.StopHarnessError.Reason()
		return res
	}
	res.Worktree = worktree
	// Deferred immediately, and before anything can fail: this is the whole
	// point of the card. Release detaches from ctx, so a cancelled run reclaims
	// exactly as a finished one does.
	defer func() {
		if err := env.trees.Release(ctx, worktree); err != nil {
			log.Error("worktree not reclaimed", "path", worktree, "error", err)
		}
	}()
	log.Debug("worktree created", "path", worktree, "commit", env.commit)

	// The task that runs is the one at the frozen commit, read from inside the
	// worktree — not the one in the working tree the run was launched from.
	// Checking the digest here rather than trusting HEAD is what makes "off the
	// frozen commit" a fact rather than an intention.
	frozen, err := r.frozenTask(worktree, env.corpusRel, task.ID)
	if err != nil {
		res.SessionErr = err.Error()
		res.Stop = engine.StopHarnessError.Reason()
		return res
	}

	taskOut := filepath.Join(env.outDir, task.ID)
	home := filepath.Join(taskOut, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		res.SessionErr = fmt.Errorf("bench: creating the temp home for %s: %w", task.ID, err).Error()
		res.Stop = engine.StopHarnessError.Reason()
		return res
	}
	res.JournalDir = journal.SessionDir(taskOut, res.SessionID)

	dir := taskRepoDir(worktree, env.corpusRel, task.ID)
	out, err := r.Agent.Run(ctx, SessionSpec{
		Task:      frozen,
		Dir:       dir,
		Home:      home,
		OutDir:    taskOut,
		SessionID: res.SessionID,
		Selection: r.Selection,
		Build:     r.Build,
	})
	res.Stop, res.Turns, res.Tokens = out.Stop, out.Turns, out.Tokens
	if err != nil {
		res.SessionErr = err.Error()
		log.Debug("session failed", "error", err)
	}

	// The oracle runs whatever the session did. A session that errored may
	// still have landed the fix before it failed, and a session that stopped
	// cleanly may have changed nothing; the suite is the only thing that knows,
	// and asking it costs one subprocess.
	//
	// A cancelled run is the exception: there is no verdict to be had from a
	// tree the agent was interrupted half way through, and running the suite
	// would spend a minute of a cancellation the user asked to be immediate.
	if ctx.Err() == nil {
		res.Oracle = runOracle(ctx, frozen, dir, home, r.caches, now)
		res.Passed = res.Oracle.Passed
		// Written before the worktree is reclaimed, and written whole. The
		// oracle's output is the diagnostic that justifies calling a task
		// failed, and it lives in the tree that is about to be deleted; a run
		// that reported "fail" with nothing to read behind it would be exactly
		// the clipped-diagnostic failure this project designs out.
		if err := writeOracleLog(taskOut, res.Oracle); err != nil {
			log.Error("oracle output not saved", "error", err)
		}
	}
	log.Debug("task finished", "passed", res.Passed, "stop", res.Stop)
	return res
}

// OracleLogName is the file a task's oracle output is kept in, inside the
// task's run directory.
const OracleLogName = "oracle.log"

// writeOracleLog keeps the oracle's whole output next to the journal, with a
// header naming what ran and what it said. Never truncated: CLAUDE.md's rule
// about tool output is a rule about the diagnostic that justifies a verdict,
// and this is that diagnostic for the only verdict a bench run produces.
func writeOracleLog(dir string, o OracleResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", strings.Join(o.Argv, " "))
	fmt.Fprintf(&b, "exit %d", o.ExitCode)
	if o.Signal != "" {
		fmt.Fprintf(&b, " (killed by %s)", o.Signal)
	}
	if o.TimedOut {
		b.WriteString(" (timed out)")
	}
	fmt.Fprintf(&b, " after %s\n\n", o.Duration)
	b.WriteString(o.Output)

	path := filepath.Join(dir, OracleLogName)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("bench: writing %s: %w", path, err)
	}
	return nil
}

// frozenTask loads the corpus from inside the worktree and returns the named
// task, refusing a corpus whose digest is not the one this run declared.
//
// corpus.Load already refuses a corpus that does not match its own recorded
// digest, so this adds the second half: that the frozen commit's corpus is the
// same corpus the run was pointed at. Without it a working tree with an edited
// task would produce a report labelled with the working tree's version and
// scored against the committed one, and the two would be silently different
// experiments.
func (r *Runner) frozenTask(worktree, corpusRel, id string) (corpus.Task, error) {
	root := filepath.Join(worktree, filepath.FromSlash(corpusRel))
	c, err := corpus.Load(root)
	if err != nil {
		return corpus.Task{}, fmt.Errorf("bench: loading the corpus at the frozen commit: %w", err)
	}
	if c.Digest != r.Corpus.Digest || c.Version != r.Corpus.Version {
		return corpus.Task{}, fmt.Errorf(
			"bench: the corpus at the frozen commit is %s %s but this run was pointed at %s %s; "+
				"commit the corpus, or name the commit it is frozen at, before running: %w",
			c.Version, c.Digest, r.Corpus.Version, r.Corpus.Digest, ErrCorpusDrift)
	}
	for _, t := range c.Tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return corpus.Task{}, fmt.Errorf("bench: task %s is not in the corpus at the frozen commit: %w",
		id, ErrCorpusDrift)
}

// classify runs the classifier if there is one. A classifier that fails is
// logged and leaves the bucket unclassified rather than failing the task: the
// attribution is a reading of the record, and a reading that could destroy the
// result it describes would be worse than an absent one.
func (r *Runner) classify(ctx context.Context, res TaskResult, log *slog.Logger) Bucket {
	if r.Classifier == nil {
		return BucketUnclassified
	}
	b, err := r.Classifier.Classify(context.WithoutCancel(ctx), res)
	if err != nil {
		log.Error("classification failed", "error", err)
		return BucketUnclassified
	}
	return b
}

// resolveCommit turns the configured revision into a commit sha.
//
// A sha rather than a name, because "HEAD" resolved once per worktree could
// resolve differently if the user moved it half way through a run, and a report
// that recorded the name would not show it.
func (r *Runner) resolveCommit(ctx context.Context, repoRoot string) (string, error) {
	rev := r.Commit
	if rev == "" {
		rev = "HEAD"
	}
	out, err := runGit(ctx, repoRoot, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("bench: resolving %s in %s: %w", rev, repoRoot, err)
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return "", fmt.Errorf("bench: %s did not resolve to a commit in %s: %w",
			rev, repoRoot, ErrNotConfigured)
	}
	return sha, nil
}

func (r *Runner) check() error {
	var missing []string
	if r.Corpus == nil {
		missing = append(missing, "Corpus")
	}
	if r.Agent == nil {
		missing = append(missing, "Agent")
	}
	if r.Selection.ModelID == "" {
		missing = append(missing, "Selection")
	}
	if len(missing) > 0 {
		return fmt.Errorf("bench: %s: %w", strings.Join(missing, ", "), ErrNotConfigured)
	}
	if r.Jobs < 0 {
		return fmt.Errorf("bench: jobs is %d, want 1 or more: %w", r.Jobs, ErrNotConfigured)
	}
	return nil
}

func (r *Runner) jobs() int {
	if r.Jobs > 0 {
		return r.Jobs
	}
	if n := runtime.NumCPU(); n < DefaultJobs {
		return n
	}
	return DefaultJobs
}

func (r *Runner) now() func() time.Time {
	if r.Now != nil {
		return r.Now
	}
	return time.Now
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// runID is the run's name and the prefix of every session id in it. UTC and
// second resolution: it ends up in a directory name and a journal session id,
// both of which stay boring on three operating systems.
func runID(at time.Time) string {
	return "run-" + at.UTC().Format("20060102t150405z")
}
