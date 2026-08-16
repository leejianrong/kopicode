package bench_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/bench"
	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
)

// fakeAgent stands in for the engine loop.
//
// The runner's contract is about what happens *around* a session — a worktree
// per task off the frozen commit, a temp HOME, an oracle with a timeout,
// parallelism capped, and every worktree reclaimed on every path. Driving that
// with the real loop would make the assertions depend on a fixture's recorded
// traffic; driving it with an agent that panics, or one that blocks until its
// context is cancelled, turns the reclamation guarantees into assertions rather
// than claims.
type fakeAgent struct {
	// run is the behaviour. Nil means [fixTask].
	run func(ctx context.Context, spec bench.SessionSpec) (bench.SessionOutcome, error)

	mu      sync.Mutex
	specs   []bench.SessionSpec
	live    int
	peak    int
	started chan struct{}
}

func (a *fakeAgent) Run(ctx context.Context, spec bench.SessionSpec) (bench.SessionOutcome, error) {
	a.mu.Lock()
	a.specs = append(a.specs, spec)
	a.live++
	if a.live > a.peak {
		a.peak = a.live
	}
	started := a.started
	a.mu.Unlock()

	if started != nil {
		started <- struct{}{}
	}

	defer func() {
		a.mu.Lock()
		a.live--
		a.mu.Unlock()
	}()

	if a.run == nil {
		return fixTask(ctx, spec)
	}
	return a.run(ctx, spec)
}

func (a *fakeAgent) seen() []bench.SessionSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]bench.SessionSpec(nil), a.specs...)
}

func (a *fakeAgent) highWater() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.peak
}

// fixTask is the "the model solved it" behaviour: it writes the file the
// synthetic oracle looks for, into the worktree the runner prepared. That the
// oracle then passes is the end-to-end proof that the agent and the oracle see
// the same tree.
func fixTask(_ context.Context, spec bench.SessionSpec) (bench.SessionOutcome, error) {
	if err := os.WriteFile(filepath.Join(spec.Dir, "fixed"), []byte("ok\n"), 0o644); err != nil {
		return bench.SessionOutcome{}, err
	}
	return bench.SessionOutcome{
		Stop:   engine.StopCompleted.Reason(),
		Turns:  2,
		Tokens: journal.TokenCounts{Prompt: 100, Completion: 10, Total: 110},
	}, nil
}

// newRunner builds a runner over the fixture with everything injected.
func newRunner(t *testing.T, f *fixture, agent bench.Agent, opts ...func(*bench.Runner)) *bench.Runner {
	t.Helper()
	c, err := corpus.Load(f.CorpusDir)
	if err != nil {
		t.Fatalf("loading the fixture corpus: %v", err)
	}
	r := &bench.Runner{
		Corpus:    c,
		Selection: testSelection(t),
		Build: journal.BuildInfo{
			Version: "test", Commit: "unknown", TreeState: "clean", Source: "unknown",
		},
		Agent: agent,
		Jobs:  2,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// testSelection resolves the arm through the real registry, so the fields the
// runner records are the fields ADR-0007's resolution populates.
func testSelection(t *testing.T) engine.Selection {
	t.Helper()
	sel, err := engine.ResolveSelection(t.TempDir(), engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the default selection: %v", err)
	}
	return sel
}

// TestRunReclaimsEveryWorktreeOnTheHappyPath is the base case, and it asserts
// the account as well as the state: ten created, ten removed, none kept, none
// left registered.
func TestRunReclaimsEveryWorktreeOnTheHappyPath(t *testing.T) {
	f := newFixture(t)
	agent := &fakeAgent{}
	r := newRunner(t, f, agent)

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got, want := len(res.Tasks), corpus.MinTasks; got != want {
		t.Fatalf("results = %d, want %d", got, want)
	}
	if got := res.Passed(); got != corpus.MinTasks {
		t.Errorf("passed = %d, want %d: the agent wrote the file every oracle looks for", got, corpus.MinTasks)
	}
	if got := res.Errored(); got != 0 {
		t.Errorf("errored = %d, want 0", got)
	}

	c := res.Reclamation
	if c.Created != corpus.MinTasks || c.Removed != corpus.MinTasks || c.Kept != 0 || len(c.Failed) != 0 {
		t.Errorf("reclamation = %+v, want created and removed %d, kept 0, none failed", c, corpus.MinTasks)
	}
	if paths := worktreePaths(t, f.Root); len(paths) != 1 {
		t.Errorf("worktrees left registered: %v", paths)
	}
	if entries, err := os.ReadDir(filepath.Join(f.Root, ".kopicode", "bench", "worktrees")); err == nil && len(entries) > 0 {
		t.Errorf("worktree directory is not empty: %v", entries)
	}
}

// TestRunRecordsTasksInCorpusOrder holds the ordering the parallelism could
// otherwise destroy. ADR-0005 §4's sequential early stopping runs tasks in a
// fixed order, and a report whose rows arrived in whichever order the workers
// finished would make two runs of one corpus look different.
func TestRunRecordsTasksInCorpusOrder(t *testing.T) {
	f := newFixture(t)
	r := newRunner(t, f, &fakeAgent{})

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, task := range r.Corpus.Tasks {
		if res.Tasks[i].TaskID != task.ID {
			t.Fatalf("result %d is %q, want %q — the corpus order was not restored",
				i, res.Tasks[i].TaskID, task.ID)
		}
	}
}

// TestWorktreeIsReclaimedWhenTheSessionPanics is one of the two clauses the
// card names outright.
//
// A panic in a task must not take the process, because that would leave the
// other nine tasks' worktrees on disk with nothing left running to remove them.
// So it is recovered per task, recorded as a harness signal (SLICE-1 §9 lists a
// panic outright), and the deferred reclamation runs on the way out.
func TestWorktreeIsReclaimedWhenTheSessionPanics(t *testing.T) {
	f := newFixture(t)
	agent := &fakeAgent{run: func(context.Context, bench.SessionSpec) (bench.SessionOutcome, error) {
		panic("the loop exploded")
	}}
	r := newRunner(t, f, agent)

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	c := res.Reclamation
	if c.Created != corpus.MinTasks || c.Removed != corpus.MinTasks || len(c.Failed) != 0 {
		t.Errorf("reclamation = %+v, want every worktree created and removed", c)
	}
	if paths := worktreePaths(t, f.Root); len(paths) != 1 {
		t.Errorf("a panicking run left worktrees registered: %v", paths)
	}

	for _, tr := range res.Tasks {
		if !tr.Panicked {
			t.Errorf("%s: Panicked = false, want true", tr.TaskID)
		}
		if !strings.Contains(tr.SessionErr, "the loop exploded") {
			t.Errorf("%s: SessionErr = %q, want the panic value in it", tr.TaskID, tr.SessionErr)
		}
		if tr.Passed {
			t.Errorf("%s: passed, but its session panicked", tr.TaskID)
		}
	}
	if res.Errored() != corpus.MinTasks {
		t.Errorf("errored = %d, want %d", res.Errored(), corpus.MinTasks)
	}
}

// TestWorktreesAreReclaimedWhenTheRunIsCancelled is the other clause.
//
// Ctrl-C mid-run must leave nothing behind. The agent here blocks until its
// context is done, so every task that started is in flight when the run is
// cancelled — which is the state a naive cleanup fails in, because its own git
// commands inherit the cancelled context and refuse to run.
func TestWorktreesAreReclaimedWhenTheRunIsCancelled(t *testing.T) {
	f := newFixture(t)
	started := make(chan struct{}, corpus.MinTasks)
	agent := &fakeAgent{
		started: started,
		run: func(ctx context.Context, _ bench.SessionSpec) (bench.SessionOutcome, error) {
			<-ctx.Done()
			return bench.SessionOutcome{Stop: engine.StopCancelled.Reason()}, ctx.Err()
		},
	}
	r := newRunner(t, f, agent, func(r *bench.Runner) { r.Jobs = 3 })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan *bench.RunResult, 1)
	go func() {
		res, _ := r.Run(ctx)
		done <- res
	}()

	// Cancel only once the cap is saturated, so the assertion is about
	// worktrees that exist and are in use rather than about a race won early.
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Fatal("no task started within 30s")
		}
	}
	cancel()

	var res *bench.RunResult
	select {
	case res = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the run did not return within 60s of cancellation")
	}

	c := res.Reclamation
	if c.Created == 0 {
		t.Fatal("no worktree was created, so this test proves nothing")
	}
	if c.Removed != c.Created || len(c.Failed) != 0 {
		t.Errorf("reclamation = %+v, want every created worktree removed", c)
	}
	if paths := worktreePaths(t, f.Root); len(paths) != 1 {
		t.Errorf("a cancelled run left worktrees registered: %v", paths)
	}
	if len(res.Tasks) != corpus.MinTasks {
		t.Errorf("results = %d, want %d: a cancelled run still reports every task",
			len(res.Tasks), corpus.MinTasks)
	}
}

// TestKeepWorktreesLeavesThemAndSaysSo is the post-mortem flag, end to end.
func TestKeepWorktreesLeavesThemAndSaysSo(t *testing.T) {
	f := newFixture(t)
	r := newRunner(t, f, &fakeAgent{}, func(r *bench.Runner) { r.KeepWorktrees = true })

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	c := res.Reclamation
	if c.Kept != corpus.MinTasks || c.Removed != 0 {
		t.Errorf("reclamation = %+v, want kept %d and removed 0", c, corpus.MinTasks)
	}
	if paths := worktreePaths(t, f.Root); len(paths) != corpus.MinTasks+1 {
		t.Errorf("registered worktrees = %d, want %d (the main one plus every task's)",
			len(paths), corpus.MinTasks+1)
	}
	for _, tr := range res.Tasks {
		if !tr.WorktreeKept {
			t.Errorf("%s: WorktreeKept = false under --keep-worktrees", tr.TaskID)
		}
		if _, err := os.Stat(tr.Worktree); err != nil {
			t.Errorf("%s: the kept worktree is gone: %v", tr.TaskID, err)
		}
	}

	// The next run reclaims what this one kept, which is the documented
	// contract and the reason the worktree directory is per task rather than
	// per run.
	next := newRunner(t, f, &fakeAgent{})
	nextRes, err := next.Run(t.Context())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if nextRes.Reclamation.PrunedStale != corpus.MinTasks {
		t.Errorf("PrunedStale = %d, want %d: the next run reclaims what --keep-worktrees left",
			nextRes.Reclamation.PrunedStale, corpus.MinTasks)
	}
}

// TestParallelismIsCapped holds the third clause of the card. The cap is what
// stands between a ten-task corpus and ten checkouts, ten agent sessions and ten
// compilers at once.
func TestParallelismIsCapped(t *testing.T) {
	// Each session dawdles, so with ten tasks and a cap of two the high-water
	// mark reaches the cap and must not pass it. The sleep is what makes the
	// observation possible at all: an instantaneous session would never overlap
	// and the assertion would hold for a runner with no cap.
	const limit = 2
	agent := &fakeAgent{run: func(ctx context.Context, spec bench.SessionSpec) (bench.SessionOutcome, error) {
		time.Sleep(20 * time.Millisecond)
		return fixTask(ctx, spec)
	}}

	f := newFixture(t)
	r := newRunner(t, f, agent, func(r *bench.Runner) { r.Jobs = limit })
	if _, err := r.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := agent.highWater(); got > limit {
		t.Errorf("%d sessions ran at once, want at most %d", got, limit)
	}
	if agent.highWater() < limit {
		t.Logf("high-water mark was %d; the cap held, but the run may have been effectively "+
			"sequential, so this run is weaker evidence than it looks", agent.highWater())
	}
}

// TestSessionsRunInTheirOwnWorktreeAndTempHome holds the isolation the card
// asks for: a worktree per task off the frozen commit, and a HOME that is not
// the developer's.
func TestSessionsRunInTheirOwnWorktreeAndTempHome(t *testing.T) {
	f := newFixture(t)
	agent := &fakeAgent{}
	r := newRunner(t, f, agent)

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	seen := map[string]bool{}
	homes := map[string]bool{}
	realHome, _ := os.UserHomeDir()
	for _, spec := range agent.seen() {
		if seen[spec.Dir] {
			t.Errorf("two tasks were given the same directory %s", spec.Dir)
		}
		seen[spec.Dir] = true
		if homes[spec.Home] {
			t.Errorf("two tasks were given the same HOME %s", spec.Home)
		}
		homes[spec.Home] = true

		if realHome != "" && spec.Home == realHome {
			t.Errorf("%s was given the developer's own HOME", spec.Task.ID)
		}
		if !strings.Contains(spec.Dir, filepath.Join(".kopicode", "bench", "worktrees")) {
			t.Errorf("%s ran in %s, which is not a bench worktree", spec.Task.ID, spec.Dir)
		}
		// The record must outlive the checkout it describes.
		if strings.HasPrefix(spec.OutDir, spec.Dir) {
			t.Errorf("%s writes its journal inside the worktree that is about to be reclaimed",
				spec.Task.ID)
		}
	}

	// And the run's own output survived the reclamation that removed every
	// worktree. (That the *journal* lands there is the smoke test's assertion:
	// this agent is a stub and opens none.)
	for _, tr := range res.Tasks {
		if _, err := os.Stat(filepath.Join(res.OutDir, tr.TaskID)); err != nil {
			t.Errorf("%s: the run output did not survive reclamation: %v", tr.TaskID, err)
		}
		if !strings.HasPrefix(tr.JournalDir, res.OutDir) {
			t.Errorf("%s: JournalDir %q is not under the run's output directory %q, so a "+
				"classifier would be pointed at a reclaimed worktree", tr.TaskID, tr.JournalDir, res.OutDir)
		}
	}
}

// TestOracleTimeoutIsReportedRatherThanCountedAsAFailure. A suite that was
// killed is not a suite that failed: nothing was measured, and a classifier
// reads the difference.
func TestOracleTimeoutIsReportedRatherThanCountedAsAFailure(t *testing.T) {
	f := newFixture(t, taskSpec{ID: "task-01", Oracle: oracleSleeps, TimeoutSeconds: 1})
	r := newRunner(t, f, &fakeAgent{})

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var slow bench.TaskResult
	for _, tr := range res.Tasks {
		if tr.TaskID == "task-01" {
			slow = tr
		}
	}
	if !slow.Oracle.TimedOut {
		t.Fatalf("task-01 oracle: %+v, want TimedOut", slow.Oracle)
	}
	if slow.Passed {
		t.Error("a timed-out oracle reported a pass")
	}
	if slow.Oracle.Duration > 30*time.Second {
		t.Errorf("the oracle ran for %s, so the timeout did not bound it", slow.Oracle.Duration)
	}
	if res.Errored() == 0 {
		t.Error("a timed-out oracle is not counted as a task that could not be run")
	}
	// And every worktree is still gone.
	if res.Reclamation.Removed != corpus.MinTasks {
		t.Errorf("reclamation = %+v, want every worktree removed", res.Reclamation)
	}
}

// TestOracleOutputIsKeptWhole holds CLAUDE.md's rule where it bites hardest: the
// output justifying a "fail" verdict lives in the tree that is about to be
// deleted, so it is written out before reclamation and never clipped.
func TestOracleOutputIsKeptWhole(t *testing.T) {
	noisy := `import sys

for i in range(500):
    print("line %d of oracle output" % i)
sys.exit(1)
`
	f := newFixture(t, taskSpec{ID: "task-01", Oracle: noisy})
	// No agent fix, so the oracle runs on the starting tree and fails loudly.
	r := newRunner(t, f, &fakeAgent{run: func(context.Context, bench.SessionSpec) (bench.SessionOutcome, error) {
		return bench.SessionOutcome{Stop: engine.StopCompleted.Reason()}, nil
	}})

	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	log, err := os.ReadFile(filepath.Join(res.OutDir, "task-01", bench.OracleLogName))
	if err != nil {
		t.Fatalf("reading the oracle log: %v", err)
	}
	if got := strings.Count(string(log), "of oracle output"); got != 500 {
		t.Errorf("the oracle log holds %d of 500 output lines: it was clipped", got)
	}
	if !strings.Contains(string(log), "exit 1") {
		t.Errorf("the oracle log does not record the exit status:\n%s", first(string(log), 200))
	}
}

// TestRunRefusesACorpusThatDriftedFromTheFrozenCommit is ADR-0005's
// experiment-series boundary, enforced where it can actually be checked.
//
// corpus.Load refuses a corpus that does not match its own recorded digest.
// That leaves the other half: a working tree whose corpus has been edited and
// re-digested is internally consistent and is *not* the corpus at the commit
// every worktree is checked out from. Running it would produce a report labelled
// with one corpus and scored against another.
func TestRunRefusesACorpusThatDriftedFromTheFrozenCommit(t *testing.T) {
	f := newFixture(t)

	// Edit a task in the working tree and re-record the digest, so the corpus
	// on disk is valid and the corpus at HEAD is a different one.
	writeFile(t, filepath.Join(f.CorpusDir, "task-01", corpus.RepoDirName, "oracle.py"),
		"import sys\n\nsys.exit(0)\n")
	ids := make([]string, 0, corpus.MinTasks)
	for i := 1; i <= corpus.MinTasks; i++ {
		ids = append(ids, fmt.Sprintf("task-%02d", i))
	}
	writeCorpusManifest(t, f.CorpusDir, ids)

	r := newRunner(t, f, &fakeAgent{})
	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	drifted := 0
	for _, tr := range res.Tasks {
		if strings.Contains(tr.SessionErr, bench.ErrCorpusDrift.Error()) {
			drifted++
		}
		if tr.Passed {
			t.Errorf("%s: passed on a corpus the run refused to trust", tr.TaskID)
		}
	}
	if drifted != corpus.MinTasks {
		t.Errorf("%d task(s) reported the drift, want %d; the report would otherwise be "+
			"labelled with one corpus and scored against another\nfirst error: %q",
			drifted, corpus.MinTasks, res.Tasks[0].SessionErr)
	}
	// Refusing is not an excuse to leak: the worktrees still go.
	if res.Reclamation.Removed != corpus.MinTasks {
		t.Errorf("reclamation = %+v, want every worktree removed", res.Reclamation)
	}
}

// TestRunRefusesToReportSuccessWithFewerWorktreesThanTasks is KAN-875's defect
// stated as a property.
//
// The sighting was a run that created nine worktrees for a ten-task corpus,
// removed all nine, printed symmetric counts and exited zero. Nothing in that
// output says a task never ran, and KAN-800's bar is zero failures classified
// `harness` — so a run that quietly measures 90% of the corpus is the expensive
// way to find a bug.
//
// Creation is broken here by a mechanism that has nothing to do with the race
// behind the original sighting: an ordinary file sitting where the worktree
// directory has to be. That is deliberate. The guard must not depend on knowing
// why creation failed, because the cause of the sighting was never pinned to a
// single mechanism.
func TestRunRefusesToReportSuccessWithFewerWorktreesThanTasks(t *testing.T) {
	f := newFixture(t)

	base := filepath.Join(f.Root, ".kopicode", "bench", "worktrees")
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, base, "a file where the worktree directory has to be\n")

	r := newRunner(t, f, &fakeAgent{})
	res, err := r.Run(t.Context())
	if !errors.Is(err, bench.ErrWorktreeCreate) {
		t.Fatalf("Run: %v, want ErrWorktreeCreate; a run that created no worktrees "+
			"must not be reportable as a run over the corpus", err)
	}
	if res == nil {
		t.Fatal("Run returned no result; the tasks that did run are still worth reading")
	}

	if got := res.Reclamation.Created; got != 0 {
		t.Errorf("Created = %d, want 0", got)
	}
	if got := len(res.Reclamation.CreateFailed); got != corpus.MinTasks {
		t.Errorf("CreateFailed = %v, want all %d tasks named",
			res.Reclamation.CreateFailed, corpus.MinTasks)
	}
	// Named, not counted: "9 of 10" does not say which task went unmeasured.
	for _, id := range []string{"task-01", "task-10"} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the error does not name %s: %v", id, err)
		}
	}
	if !strings.Contains(res.Tasks[0].SessionErr, "creating a worktree") {
		t.Errorf("task-01's own result does not say its worktree failed: %q", res.Tasks[0].SessionErr)
	}

	var report strings.Builder
	if err := bench.WriteReport(&report, res); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if !strings.Contains(report.String(), "NOT CREATED") {
		t.Errorf("the report does not say a worktree was missing:\n%s", report.String())
	}
}

// TestRunRefusesAnIncompleteConfiguration keeps the runner from being started
// with a piece missing and discovering it after a worktree exists.
func TestRunRefusesAnIncompleteConfiguration(t *testing.T) {
	for name, r := range map[string]*bench.Runner{
		"no corpus":    {Agent: &fakeAgent{}, Selection: testSelection(t)},
		"no agent":     {Corpus: &corpus.Corpus{}, Selection: testSelection(t)},
		"no selection": {Corpus: &corpus.Corpus{}, Agent: &fakeAgent{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Run(t.Context()); !errors.Is(err, bench.ErrNotConfigured) {
				t.Fatalf("Run: %v, want ErrNotConfigured", err)
			}
		})
	}
}

// TestReportNamesTheReclamation is the card's last clause: silent cleanup and
// silent accumulation look identical from outside, so the numbers are in the
// report rather than in a debug log.
func TestReportNamesTheReclamation(t *testing.T) {
	f := newFixture(t)
	r := newRunner(t, f, &fakeAgent{})
	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var b strings.Builder
	if err := bench.WriteReport(&b, res); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	report := b.String()

	for _, want := range []string{
		"worktrees: created 10, removed 10, kept 0",
		"unclassified",
		res.CorpusDigest,
		res.OutDir,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not mention %q:\n%s", want, report)
		}
	}
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
