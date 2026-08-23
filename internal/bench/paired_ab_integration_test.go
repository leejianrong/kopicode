//go:build integration

package bench_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/bench"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/harness"
)

// TestPairedRunSaveCompareOverTheRealCorpus is KAN-943's whole story end to
// end, at zero cost: two real [bench.RunCorpus] invocations against the mock
// provider — the two `kopibench run --provider mock --save …` calls the PR
// describes — each result taken through [bench.SaveResult] and
// [bench.LoadResult] exactly as a file on disk would, and then
// [bench.Compare] and [bench.WriteCompareReport] over the loaded pair exactly
// as `kopibench compare` runs them.
//
// It is what makes "fully validate the plumbing against the mock provider
// before spending anything on the live one" a checked claim rather than a
// description of manual steps nobody re-runs. Behind the integration tag for
// the same reason [TestSmokeRunOverTheRealCorpus] is: it compiles the real
// corpus's suites, twice.
//
// # Why this pairs two harness configurations rather than two models
//
// The two arms KAN-943 actually cares about — qwen/qwen3-coder-next and
// minimax/minimax-m2 — differ by model id, and that is exactly what the mock
// provider refuses to replay: every shipped fixture in
// internal/provider/fixture was hand-authored against one recorded model
// ("qwen/qwen3-coder-next", see [bench.SmokeFixture]'s own doc comment), and
// [mock.ErrModelMismatch] fires the instant a request's model does not match
// it — by design, not a bug this test tripped over by accident (verified by
// hand: running the corpus with --model minimax/minimax-m2 against the mock
// provider errors every task with exactly that message). So the model axis
// cannot be exercised against a mock fixture at all without a second
// hand-authored fixture recorded for minimax/minimax-m2, which is not this
// card's job to add.
//
// What *can* be exercised for real, at zero cost, is the harness-config axis:
// this test keeps the model at the default (matching the fixture) and pairs
// [harness.DefaultConfigName] against [harness.MinimaxM2ConfigName] via
// --harness, which is a genuinely different [harness.Config] (different
// Sampling, different hash) flowing through the exact same RunCorpus ->
// SaveResult -> LoadResult -> Compare -> WriteCompareReport pipeline the live
// model-vs-model comparison uses. That is the plumbing this card adds; which
// two arms it is pointed at is a flag value the live run supplies.
func TestPairedRunSaveCompareOverTheRealCorpus(t *testing.T) {
	fA := newCorpusCopyFixture(t)
	fB := newCorpusCopyFixture(t)

	selA := testSelection(t)
	selB := selectionWithHarness(t, harness.MinimaxM2ConfigName)
	if selA.HarnessConfigHash == selB.HarnessConfigHash {
		t.Fatalf("the two arms resolved to the same harness config hash; the comparison below "+
			"would not be exercising two different arms: %s", selA.HarnessConfigHash)
	}

	runA, err := bench.RunCorpus(t.Context(), bench.Options{
		CorpusDir: fA.CorpusDir,
		Selection: selA,
		Build:     bench.BuildIdentity{Version: "test", Commit: "a", TreeState: "clean", Source: "unknown"},
		Provider:  bench.ProviderMock,
		Jobs:      2,
	})
	if err != nil {
		t.Fatalf("RunCorpus (arm A): %v", err)
	}
	runB, err := bench.RunCorpus(t.Context(), bench.Options{
		CorpusDir: fB.CorpusDir,
		Selection: selB,
		Build:     bench.BuildIdentity{Version: "test", Commit: "b", TreeState: "clean", Source: "unknown"},
		Provider:  bench.ProviderMock,
		Jobs:      2,
	})
	if err != nil {
		t.Fatalf("RunCorpus (arm B): %v", err)
	}
	if got := runA.Errored(); got != 0 {
		t.Fatalf("arm A errored on %d task(s); the mock replay should answer every task", got)
	}
	if got := runB.Errored(); got != 0 {
		t.Fatalf("arm B errored on %d task(s); the mock replay should answer every task", got)
	}

	// Round-trip through exactly the encoding `run --save` and `compare` use —
	// a bytes.Buffer standing in for the file, since SaveResult/LoadResult
	// know nothing about files, only io.Writer/io.Reader.
	var bufA, bufB bytes.Buffer
	if err := bench.SaveResult(&bufA, runA); err != nil {
		t.Fatalf("SaveResult (arm A): %v", err)
	}
	if err := bench.SaveResult(&bufB, runB); err != nil {
		t.Fatalf("SaveResult (arm B): %v", err)
	}
	loadedA, err := bench.LoadResult(&bufA)
	if err != nil {
		t.Fatalf("LoadResult (arm A): %v", err)
	}
	loadedB, err := bench.LoadResult(&bufB)
	if err != nil {
		t.Fatalf("LoadResult (arm B): %v", err)
	}

	if got, want := len(loadedA.Tasks), fA.TaskCount; got != want {
		t.Fatalf("loaded arm A has %d tasks, want %d", got, want)
	}
	if got, want := len(loadedB.Tasks), fB.TaskCount; got != want {
		t.Fatalf("loaded arm B has %d tasks, want %d", got, want)
	}
	if loadedA.Arm.HarnessConfigName != harness.DefaultConfigName {
		t.Errorf("loaded arm A's harness config = %s, want %s", loadedA.Arm.HarnessConfigName, harness.DefaultConfigName)
	}
	if loadedB.Arm.HarnessConfigName != harness.MinimaxM2ConfigName {
		t.Errorf("loaded arm B's harness config = %s, want %s", loadedB.Arm.HarnessConfigName, harness.MinimaxM2ConfigName)
	}

	res, err := bench.Compare(loadedA.Scored(), loadedB.Scored())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	// Both arms replay the same fixture regardless of which harness
	// configuration asked for it, so every task's outcome is identical across
	// arms: MethodNone, no discordant pairs. That degenerate case is itself
	// part of the point — a paired comparison over two runs that agreed on
	// everything is a real, scoreable result and not an error, exactly as
	// TestCompareScoresTheZeroDiscordantCase asserts at the unit level; this
	// test is the same claim end to end through the runner and the files.
	if res.Discordant() != 0 {
		t.Errorf("Discordant() = %d, want 0 — both arms replay the same fixture", res.Discordant())
	}
	if res.Method != bench.MethodNone {
		t.Errorf("Method = %v, want %v", res.Method, bench.MethodNone)
	}

	var report strings.Builder
	if err := bench.WriteCompareReport(&report, res, loadedA, loadedB); err != nil {
		t.Fatalf("WriteCompareReport: %v", err)
	}
	t.Logf("compare report:\n%s", report.String())

	for _, want := range []string{
		harness.DefaultConfigName, harness.MinimaxM2ConfigName,
		"both runs replayed the mock provider",
		"the arms agreed on every task",
	} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("compare report does not contain %q:\n%s", want, report.String())
		}
	}
}

// selectionWithHarness is [testSelection] with a --harness override, so this
// file can resolve a named harness configuration through the real registry
// while leaving the model at its default — which is what keeps the request
// inside the model the shipped mock fixture was recorded against. See this
// test's own doc comment for why the model axis itself cannot be driven
// through the mock provider.
func selectionWithHarness(t *testing.T, harnessName string) engine.Selection {
	t.Helper()
	sel, err := engine.ResolveSelection(t.TempDir(), engine.SelectionOverrides{Harness: harnessName})
	if err != nil {
		t.Fatalf("resolving the selection for harness %s: %v", harnessName, err)
	}
	if sel.Config.Name != harnessName {
		t.Fatalf("resolved harness config = %s, want %s", sel.Config.Name, harnessName)
	}
	return sel
}
