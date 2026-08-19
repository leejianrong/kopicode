package bench_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/leejianrong/kopicode/internal/bench"
	"github.com/leejianrong/kopicode/internal/journal"
)

// sampleRun builds a RunResult with something in every field this package
// round-trips, including the one field that does not survive a naive
// encoding/json round trip: OracleResult.Err.
func sampleRun(t *testing.T) *bench.RunResult {
	t.Helper()
	started := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	return &bench.RunResult{
		RunID: "run-20260819t120000z",
		Arm: bench.ArmIdentity{
			ModelID:           "qwen/qwen3-coder-next",
			HarnessConfigHash: "deadbeefcafef00d",
			HarnessConfigName: "default",
			ProviderPin:       "parasail/bf16",
			Build: journal.BuildInfo{
				Version: "v0.1.0", Commit: "abc123", TreeState: "clean", Source: "ldflags",
			},
		},
		CorpusVersion: "v1",
		CorpusDigest:  "sha256:abcdef",
		Commit:        "0123456789abcdef",
		Tasks: []bench.TaskResult{
			{
				TaskID:     "go-config-retries",
				Passed:     true,
				SessionID:  "run-20260819t120000z-go-config-retries",
				JournalDir: "/tmp/out/go-config-retries",
				Stop:       "completed",
				Turns:      2,
				Tokens:     journal.TokenCounts{Prompt: 100, Completion: 20, Total: 120},
				Bucket:     bench.BucketModel,
				Worktree:   "/tmp/wt/go-config-retries",
				Duration:   3*time.Second + 400*time.Millisecond,
				Oracle: bench.OracleResult{
					Argv: []string{"go", "test", "./..."}, Passed: true, ExitCode: 0,
					Duration: 500 * time.Millisecond, Output: "ok\n",
				},
			},
			{
				TaskID:     "py-fix-typo",
				Passed:     false,
				SessionID:  "run-20260819t120000z-py-fix-typo",
				JournalDir: "/tmp/out/py-fix-typo",
				Stop:       "max_turns",
				Turns:      20,
				Tokens:     journal.TokenCounts{Prompt: 500, Completion: 90, Total: 590},
				SessionErr: "",
				Bucket:     bench.BucketHarness,
				Worktree:   "/tmp/wt/py-fix-typo",
				Duration:   9 * time.Second,
				Oracle: bench.OracleResult{
					Argv: []string{"pytest"}, Passed: false, ExitCode: 1,
					Duration: 200 * time.Millisecond, Output: "1 failed\n",
					// The field that does not survive a naive JSON round
					// trip: a non-nil error whose concrete type has no
					// exported fields, so encoding/json's default behaviour
					// would silently emit "{}" here.
					Err: errors.New("exec: \"pytest\": executable file not found in $PATH"),
				},
			},
		},
		Reclamation: bench.Reclamation{Created: 2, Removed: 2},
		Jobs:        4,
		Started:     started,
		Duration:    12 * time.Second,
		OutDir:      "/tmp/out",
		Provider:    bench.ProviderMock,
	}
}

// TestSaveLoadResultRoundTrips is the property the rest of this file's tests
// pick apart: every field a report or a paired comparison reads survives
// [bench.SaveResult] followed by [bench.LoadResult], including the ones that
// are not simple scalars.
func TestSaveLoadResultRoundTrips(t *testing.T) {
	want := sampleRun(t)

	var buf bytes.Buffer
	if err := bench.SaveResult(&buf, want); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}

	got, err := bench.LoadResult(&buf)
	if err != nil {
		t.Fatalf("LoadResult: %v", err)
	}

	// OracleResult.Err is compared by message, not by errors.Is/As: a loaded
	// error is reconstructed as a plain errors.New over the saved message and
	// deliberately gives up the original concrete type. Nothing downstream
	// type-switches on it, only checks it against nil, so cmp.Diff would
	// otherwise fail this test over a type it never claimed to preserve.
	opt := cmp.Comparer(func(a, b error) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return a.Error() == b.Error()
	})
	if diff := cmp.Diff(want, got, opt); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
}

// TestSaveResultDoesNotEscapeHTML holds the reason SaveResult uses its own
// encoder rather than json.Marshal: a path or an oracle message containing
// `<`, `>` or `&` must read back byte for byte and not gain \uXXXX noise.
func TestSaveResultDoesNotEscapeHTML(t *testing.T) {
	r := sampleRun(t)
	r.Tasks[0].Worktree = "/tmp/wt/<script>&weird"

	var buf bytes.Buffer
	if err := bench.SaveResult(&buf, r); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	// encoding/json's default HTML-safety mode rewrites the bytes '<', '>'
	// and '&' into the six-character unicode escapes \u003c, \u003e and
	// \u0026 — not required by JSON itself, only a convenience for embedding
	// JSON inside an HTML <script> tag, which this file is never destined
	// for. If SaveResult used json.Marshal directly instead of an encoder
	// with SetEscapeHTML(false), those escapes would appear here and the
	// literal path would not.
	const escaped = `\u003c`
	if strings.Contains(buf.String(), escaped) {
		t.Errorf("the saved JSON HTML-escaped a path with %s:\n%s", escaped, buf.String())
	}
	if !strings.Contains(buf.String(), "<script>&weird") {
		t.Errorf("the saved JSON does not carry the path's literal bytes:\n%s", buf.String())
	}

	got, err := bench.LoadResult(&buf)
	if err != nil {
		t.Fatalf("LoadResult: %v", err)
	}
	if got.Tasks[0].Worktree != r.Tasks[0].Worktree {
		t.Errorf("Worktree = %q, want %q", got.Tasks[0].Worktree, r.Tasks[0].Worktree)
	}
}

// TestSaveResultRefusesNil matches the rest of this package's rule that a
// refusal to score or persist carries a typed reason rather than a nil
// pointer dereference.
func TestSaveResultRefusesNil(t *testing.T) {
	var buf bytes.Buffer
	if err := bench.SaveResult(&buf, nil); err == nil {
		t.Fatal("SaveResult(nil): want an error, got nil")
	}
}

// TestLoadResultRefusesEmpty is the guard against the JSON zero value: valid
// JSON, valid Go, and a comparison that would print a header for a run that
// never happened if [LoadResult] let it through.
func TestLoadResultRefusesEmpty(t *testing.T) {
	_, err := bench.LoadResult(strings.NewReader(`{}`))
	if !errors.Is(err, bench.ErrEmptyResult) {
		t.Fatalf("LoadResult({}) error = %v, want %v", err, bench.ErrEmptyResult)
	}
}

// TestLoadResultRefusesGarbage is the ordinary "not JSON at all" case, kept
// distinct from the empty-but-valid case above because the message a human
// sees should say which one happened.
func TestLoadResultRefusesGarbage(t *testing.T) {
	_, err := bench.LoadResult(strings.NewReader(`not json`))
	if err == nil {
		t.Fatal("LoadResult(garbage): want an error, got nil")
	}
	if errors.Is(err, bench.ErrEmptyResult) {
		t.Errorf("LoadResult(garbage) should not be ErrEmptyResult: %v", err)
	}
}

// TestSavedResultScoresTheSameAsTheOriginal is the property that actually
// matters for KAN-943: a result loaded back from disk must produce the same
// [bench.Compare] outcome as the in-memory result would have, because that is
// the entire reason to persist one.
func TestSavedResultScoresTheSameAsTheOriginal(t *testing.T) {
	a := sampleRun(t)
	b := sampleRun(t)
	b.Arm.ModelID = "minimax/minimax-m2"
	b.Tasks[1].Passed = true // flips one outcome, so the comparison is not the degenerate case

	wantResult, err := bench.Compare(a.Scored(), b.Scored())
	if err != nil {
		t.Fatalf("Compare on the in-memory results: %v", err)
	}

	var bufA, bufB bytes.Buffer
	if err := bench.SaveResult(&bufA, a); err != nil {
		t.Fatalf("SaveResult(a): %v", err)
	}
	if err := bench.SaveResult(&bufB, b); err != nil {
		t.Fatalf("SaveResult(b): %v", err)
	}
	loadedA, err := bench.LoadResult(&bufA)
	if err != nil {
		t.Fatalf("LoadResult(a): %v", err)
	}
	loadedB, err := bench.LoadResult(&bufB)
	if err != nil {
		t.Fatalf("LoadResult(b): %v", err)
	}

	gotResult, err := bench.Compare(loadedA.Scored(), loadedB.Scored())
	if err != nil {
		t.Fatalf("Compare on the round-tripped results: %v", err)
	}
	if diff := cmp.Diff(wantResult, gotResult); diff != "" {
		t.Errorf("a round trip through Save/LoadResult changed the comparison (-want +got):\n%s", diff)
	}
}
