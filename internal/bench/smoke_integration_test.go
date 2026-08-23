//go:build integration

package bench_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/bench"
	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/journal"
)

// TestSmokeRunOverTheRealCorpus is `make bench-smoke` as a test: the frozen
// corpus, the real engine loop, the replay provider, zero tokens.
//
// It is what makes the smoke target's *acceptance* checkable rather than
// observed by eye — the target itself runs the shipped binary against the
// repository's own corpus, and this runs the same code against a copy of that
// corpus inside t.TempDir(), because a test that created worktrees in
// kopicode's own repository is precisely the escape this package's fixtures
// exist to prevent.
//
// What the mock run proves and what it does not: the recorded traffic reads a
// file no corpus task has and then answers in prose, so every oracle fails and
// every failure is the model's. Nothing here measures capability. What it does
// measure is the runner — a worktree per task off the frozen commit, a temp
// HOME, an oracle in a subprocess with a timeout, parallelism capped, a journal
// that outlives the checkout, and every worktree reclaimed — which is the half
// that has to be right before an arm's numbers mean anything.
//
// Behind the integration tag because it compiles every task's small project
// and runs its suite.
func TestSmokeRunOverTheRealCorpus(t *testing.T) {
	f := newCorpusCopyFixture(t)

	res, err := bench.RunCorpus(t.Context(), bench.Options{
		CorpusDir: f.CorpusDir,
		Selection: testSelection(t),
		Build: bench.BuildIdentity{
			Version: "test", Commit: "unknown", TreeState: "clean", Source: "unknown",
		},
		Provider: bench.ProviderMock,
		Jobs:     2,
	})
	if err != nil {
		t.Fatalf("RunCorpus: %v", err)
	}

	if got := len(res.Tasks); got != f.TaskCount {
		t.Fatalf("results = %d, want %d", got, f.TaskCount)
	}

	// The bar for the smoke run is that every task was *put a question*, not
	// that any was answered. A task that errored is a runner that could not
	// ask, and that is what would make `make bench-smoke` a broken gate.
	if got := res.Errored(); got != 0 {
		for _, tr := range res.Tasks {
			if tr.SessionErr != "" || tr.Panicked || tr.Oracle.Err != nil {
				t.Errorf("%s: session %q oracle %v panicked=%v",
					tr.TaskID, tr.SessionErr, tr.Oracle.Err, tr.Panicked)
			}
		}
		t.Fatalf("errored = %d, want 0", got)
	}

	c := res.Reclamation
	if c.Created != f.TaskCount || c.Removed != f.TaskCount || len(c.Failed) != 0 {
		t.Errorf("reclamation = %+v, want created and removed %d", c, f.TaskCount)
	}
	if paths := worktreePaths(t, f.Root); len(paths) != 1 {
		t.Errorf("the smoke run left worktrees registered: %v", paths)
	}

	for _, tr := range res.Tasks {
		if tr.Stop != "completed" {
			t.Errorf("%s: stop = %q, want completed — the recorded session ends in prose", tr.TaskID, tr.Stop)
		}
		if tr.Turns != 2 {
			t.Errorf("%s: turns = %d, want 2 — the fixture records two", tr.TaskID, tr.Turns)
		}
		if tr.Tokens.Total == 0 {
			t.Errorf("%s: no token usage recorded; the replay carries the recorded counts", tr.TaskID)
		}
		// The recorded traffic reads a file no task has and then answers in
		// prose, so every session stops cleanly, no edit is attempted, and the
		// oracle fails. That is `model` under the mechanical rule, and it is
		// the end-to-end proof that RunCorpus wires a classifier at all: an
		// unclassified row here would mean the front end's own path produces
		// unattributed failures.
		if tr.Bucket != bench.BucketModel {
			t.Errorf("%s: bucket = %q, want %q — the fixture stops cleanly and the oracle fails",
				tr.TaskID, tr.Bucket, bench.BucketModel)
		}
		assertSessionRecorded(t, tr)
		assertOracleLogged(t, res.OutDir, tr)
	}

	var report strings.Builder
	if err := bench.WriteReport(&report, res); err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if !strings.Contains(report.String(), fmt.Sprintf("created %d, removed %d, kept 0",
		f.TaskCount, f.TaskCount)) {
		t.Errorf("the report does not account for the worktrees:\n%s", report.String())
	}
	// The whole tally, and the `unattributed` line ADR-0006 §3 asks for by
	// name, on a run where that bucket is empty.
	for _, want := range []string{
		fmt.Sprintf("attribution over %d failed task(s): harness 0, unattributed 0, model %d, "+
			"unclassified 0", f.TaskCount, f.TaskCount),
		"unattributed = 0",
		// KAN-905: this is `make bench-smoke`'s actual run, so the footer
		// explaining a 0-pass mock run must appear on the real thing and not
		// only in a hand-built unit test.
		"SMOKE BASELINE",
		fmt.Sprintf("0/%d passing is EXPECTED", f.TaskCount),
	} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, report.String())
		}
	}
	t.Logf("smoke report:\n%s", report.String())
}

// assertSessionRecorded reads the task's journal back. It is the point of
// putting the record outside the worktree: the checkout it describes has been
// deleted by the time this runs, and the record has not.
func assertSessionRecorded(t *testing.T, tr bench.TaskResult) {
	t.Helper()

	path := filepath.Join(tr.JournalDir, "events.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%s: the journal did not survive the worktree: %v", tr.TaskID, err)
		return
	}
	text := string(b)
	for _, want := range []journal.Type{
		journal.TypeSessionStarted,
		journal.TypeProviderRequest,
		journal.TypeToolCallParsed,
		journal.TypeSessionEnded,
	} {
		if !strings.Contains(text, `"`+string(want)+`"`) {
			t.Errorf("%s: the journal holds no %s event", tr.TaskID, want)
		}
	}
	// The credential must be nowhere, and the mock run never has one — so this
	// is a cheap standing check rather than a proof (internal/engine's
	// keyleak_test.go owns that).
	if strings.Contains(text, "sk-or-") {
		t.Errorf("%s: something that looks like a credential is in the journal", tr.TaskID)
	}
}

func assertOracleLogged(t *testing.T, outDir string, tr bench.TaskResult) {
	t.Helper()

	path := filepath.Join(outDir, tr.TaskID, bench.OracleLogName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("%s: the oracle output was not kept: %v", tr.TaskID, err)
		return
	}
	if !strings.Contains(string(b), "exit ") {
		t.Errorf("%s: the oracle log does not record an exit status", tr.TaskID)
	}
}

// newCorpusCopyFixture builds a repository under t.TempDir() holding a copy of
// the repository's real corpus, committed.
//
// A copy rather than the corpus in place: `git worktree add` registers against
// the repository the corpus lives in, and for the corpus in place that is
// kopicode's own. The digest is over the corpus's contents, so a copy loads as
// the same corpus and the frozen-commit check is exercised for real.
func newCorpusCopyFixture(t *testing.T) *fixture {
	t.Helper()

	src, err := filepath.Abs(filepath.Join("..", "..", "bench", "tasks"))
	if err != nil {
		t.Fatalf("resolving the corpus: %v", err)
	}
	frozen, err := corpus.Load(src)
	if err != nil {
		t.Fatalf("loading the real corpus: %v", err)
	}

	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q", "-b", "main")
	assertIsolated(t, root)

	dst := filepath.Join(root, "bench", "tasks")
	copyTree(t, src, dst)

	copied, err := corpus.Load(dst)
	if err != nil {
		t.Fatalf("the copied corpus does not load: %v", err)
	}
	if copied.Digest != frozen.Digest {
		t.Fatalf("the copy digests as %s, the original as %s: the copy is not the same corpus",
			copied.Digest, frozen.Digest)
	}

	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "corpus")
	return &fixture{
		Root:      root,
		CorpusDir: dst,
		Commit:    git(t, root, "rev-parse", "HEAD"),
		TaskCount: len(copied.Tasks),
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatalf("copying %s to %s: %v", src, dst, err)
	}
}
