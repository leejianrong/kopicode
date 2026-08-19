package bench_test

import (
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/bench"
	"github.com/leejianrong/kopicode/internal/journal"
)

// twoRuns builds two RunResults over the same five-task corpus, differing in
// exactly the two tasks the caller's outcomes describe, so a test can drive a
// specific contingency table through the real [bench.RunResult.Scored] path
// rather than building a [bench.Arm] by hand.
func twoRuns(t *testing.T, passedA, passedB map[string]bool, corpusDigest string) (*bench.RunResult, *bench.RunResult) {
	t.Helper()
	ids := []string{"t1", "t2", "t3", "t4", "t5"}

	build := func(modelID, hash, name, pin, digest string, passed map[string]bool) *bench.RunResult {
		r := &bench.RunResult{
			RunID: "run-" + modelID,
			Arm: bench.ArmIdentity{
				ModelID:           modelID,
				HarnessConfigHash: hash,
				HarnessConfigName: name,
				ProviderPin:       pin,
				Build:             journal.BuildInfo{Version: "v0", Commit: "abc", TreeState: "clean", Source: "test"},
			},
			CorpusVersion: "v1",
			CorpusDigest:  digest,
			Provider:      bench.ProviderMock,
			Jobs:          2,
		}
		for _, id := range ids {
			r.Tasks = append(r.Tasks, bench.TaskResult{
				TaskID: id, Passed: passed[id], Bucket: bucketFor(passed[id]),
				Tokens: journal.TokenCounts{Total: 100},
			})
		}
		return r
	}

	a := build("qwen/qwen3-coder-next", "hasha", "default", "parasail/bf16", corpusDigest, passedA)
	b := build("minimax/minimax-m2", "hashb", "minimax-m2-v1", "minimax/fp8", corpusDigest, passedB)
	return a, b
}

func bucketFor(passed bool) bench.Bucket {
	if passed {
		return bench.BucketUnclassified
	}
	return bench.BucketModel
}

// TestWriteCompareReportRendersTheTable holds the contingency table's
// arithmetic to the report's text, not just to [bench.Result]'s fields: a
// report that scored correctly but printed the wrong cell in the wrong place
// would pass every test in mcnemar_test.go and still mislead a reader.
func TestWriteCompareReportRendersTheTable(t *testing.T) {
	passedA := map[string]bool{"t1": true, "t2": true, "t3": false, "t4": true, "t5": false}
	passedB := map[string]bool{"t1": true, "t2": false, "t3": true, "t4": false, "t5": false}
	a, b := twoRuns(t, passedA, passedB, "sha256:same")

	res, err := bench.Compare(a.Scored(), b.Scored())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}

	var out strings.Builder
	if err := bench.WriteCompareReport(&out, res, a, b); err != nil {
		t.Fatalf("WriteCompareReport: %v", err)
	}
	report := out.String()
	t.Logf("report:\n%s", report)

	for _, want := range []string{
		a.Arm.ModelID, b.Arm.ModelID,
		a.Arm.HarnessConfigName, b.Arm.HarnessConfigName,
		a.RunID, b.RunID,
		"discordant pairs   3",
		"method             exact-binomial",
		"direction          favours A",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not contain %q:\n%s", want, report)
		}
	}
}

// TestWriteCompareReportFlagsPoolabilityHazards is the check that a
// comparison can be arithmetically clean and still not be the evidence
// ADR-0005/ADR-0007 ask for, and that the report says so rather than staying
// silent about it. [bench.Compare] itself does not refuse any of these —
// only an unpaired task set is a hard refusal — so the notes are this file's
// own job.
func TestWriteCompareReportFlagsPoolabilityHazards(t *testing.T) {
	passed := map[string]bool{"t1": true, "t2": true, "t3": true, "t4": true, "t5": true}

	t.Run("differing corpus digest", func(t *testing.T) {
		a, b := twoRuns(t, passed, passed, "sha256:aaa")
		b.CorpusDigest = "sha256:bbb"
		report := renderCompare(t, a, b)
		if !strings.Contains(report, "corpus digest differs") {
			t.Errorf("report does not flag the corpus digest mismatch:\n%s", report)
		}
	})

	t.Run("dirty build", func(t *testing.T) {
		a, b := twoRuns(t, passed, passed, "sha256:same")
		a.Arm.Build.TreeState = "dirty"
		report := renderCompare(t, a, b)
		if !strings.Contains(report, `run A's build tree state is "dirty"`) {
			t.Errorf("report does not flag the dirty build:\n%s", report)
		}
	})

	t.Run("mock vs live", func(t *testing.T) {
		a, b := twoRuns(t, passed, passed, "sha256:same")
		a.Provider = bench.ProviderLive
		b.Provider = bench.ProviderMock
		report := renderCompare(t, a, b)
		if !strings.Contains(report, "providers differ") {
			t.Errorf("report does not flag the provider mismatch:\n%s", report)
		}
	})

	t.Run("both mock", func(t *testing.T) {
		a, b := twoRuns(t, passed, passed, "sha256:same")
		report := renderCompare(t, a, b)
		if !strings.Contains(report, "both runs replayed the mock provider") {
			t.Errorf("report does not flag that both runs are mock:\n%s", report)
		}
	})

	t.Run("clean pair has no notes", func(t *testing.T) {
		a, b := twoRuns(t, passed, passed, "sha256:same")
		a.Provider, b.Provider = bench.ProviderLive, bench.ProviderLive
		report := renderCompare(t, a, b)
		if strings.Contains(report, "NOTE") {
			t.Errorf("a clean pair should carry no poolability notes:\n%s", report)
		}
	})
}

func renderCompare(t *testing.T, a, b *bench.RunResult) string {
	t.Helper()
	res, err := bench.Compare(a.Scored(), b.Scored())
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	var out strings.Builder
	if err := bench.WriteCompareReport(&out, res, a, b); err != nil {
		t.Fatalf("WriteCompareReport: %v", err)
	}
	return out.String()
}

// TestWriteCompareReportZeroDiscordant covers [bench.MethodNone]'s report
// path, which prints no statistic at all rather than a nonsensical one.
func TestWriteCompareReportZeroDiscordant(t *testing.T) {
	passed := map[string]bool{"t1": true, "t2": false, "t3": true, "t4": false, "t5": true}
	a, b := twoRuns(t, passed, passed, "sha256:same")
	report := renderCompare(t, a, b)

	for _, want := range []string{
		"method             none",
		"the arms agreed on every task",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not contain %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "statistic") {
		t.Errorf("MethodNone should print no statistic line:\n%s", report)
	}
}
