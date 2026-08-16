package bench_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/bench"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
	"github.com/leejianrong/kopicode/internal/verify"
)

// The classifier is asserted the way SLICE-1 §9 defines it: given a *journal*,
// which bucket. Every case below writes real events through the real journal
// and hands the classifier nothing but a [bench.TaskResult] pointing at the
// directory they landed in — no fake reader, no hand-rolled JSONL, and no
// interface standing in for the record. A rule that passed against a mocked
// event stream would prove only that this package can call a mock, and the
// record's shape is exactly what these rules are made of.

// recordSession writes payloads to a real journal under t.TempDir() and returns
// a TaskResult pointing at it, pre-filled with the fields a failing task that
// stopped cleanly would carry. Callers override what their case is about.
func recordSession(t *testing.T, payloads ...journal.Payload) bench.TaskResult {
	t.Helper()

	root := t.TempDir()
	const id = "run-test-task"

	j, err := journal.Open(root, id)
	if err != nil {
		t.Fatalf("opening the journal: %v", err)
	}
	for i, p := range payloads {
		if _, err := j.Append(t.Context(), 1, p); err != nil {
			t.Fatalf("appending payload %d (%s): %v", i, p.Type(), err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatalf("closing the journal: %v", err)
	}

	return bench.TaskResult{
		TaskID:     "task",
		SessionID:  id,
		JournalDir: journal.SessionDir(root, id),
		Stop:       engine.StopCompleted.Reason(),
		Oracle:     bench.OracleResult{ExitCode: 1},
	}
}

func classify(t *testing.T, r bench.TaskResult) bench.Bucket {
	t.Helper()
	b, err := bench.Attribution{}.Classify(t.Context(), r)
	if err != nil {
		t.Fatalf("Classify: unexpected error: %v", err)
	}
	return b
}

// TestClassifyReadsTheJournal covers every rule that is a fact about the
// record, one case per rule plus the near miss that must not trip it.
func TestClassifyReadsTheJournal(t *testing.T) {
	cases := map[string]struct {
		events []journal.Payload
		want   bench.Bucket
	}{
		"a clean session the oracle failed is the model's": {
			events: []journal.Payload{
				journal.EditApplied{Path: "a.go", Mode: "anchored"},
				journal.SyntaxGateRun{Path: "a.go", Checker: "gofmt", Ran: true, ExitCode: 0},
			},
			want: bench.BucketModel,
		},
		"a session with no events at all is the model's": {
			events: nil,
			want:   bench.BucketModel,
		},

		// harness
		"a tool call the repairer gave up on is the harness's": {
			events: []journal.Payload{
				journal.ToolCallRepaired{CallID: "c1", Attempt: 1, Classification: "invalid_json"},
				journal.ToolCallFailed{CallID: "c1", Reason: "invalid_json"},
			},
			want: bench.BucketHarness,
		},
		"a tool internal error is the harness's": {
			events: []journal.Payload{
				journal.ToolResult{CallID: "c1", Tool: "read_file", ErrorKind: tools.FaultInternal.String()},
			},
			want: bench.BucketHarness,
		},
		"a task-level tool error is not": {
			events: []journal.Payload{
				journal.ToolResult{CallID: "c1", Tool: "read_file", ErrorKind: tools.FaultTask.String()},
			},
			want: bench.BucketModel,
		},
		"a cancelled tool is not": {
			events: []journal.Payload{
				journal.ToolResult{CallID: "c1", Tool: "run_shell", ErrorKind: tools.FaultCancelled.String()},
			},
			want: bench.BucketModel,
		},
		"a syntax gate failing straight after an edit is the harness's": {
			events: []journal.Payload{
				journal.EditApplied{Path: "a.go", Mode: "anchored"},
				journal.SyntaxGateRun{Path: "a.go", Checker: "gofmt", Ran: true, ExitCode: 2},
			},
			want: bench.BucketHarness,
		},
		"a syntax gate failing away from an edit is not": {
			events: []journal.Payload{
				journal.EditApplied{Path: "a.go", Mode: "anchored"},
				journal.ToolResult{CallID: "c2", Tool: "run_shell"},
				journal.SyntaxGateRun{Path: "a.go", Checker: "gofmt", Ran: true, ExitCode: 2},
			},
			want: bench.BucketModel,
		},
		"a syntax gate that could not run is not a failure": {
			events: []journal.Payload{
				journal.EditApplied{Path: "a.txt", Mode: "anchored"},
				journal.SyntaxGateRun{Path: "a.txt", Ran: false, ExitCode: 0},
			},
			want: bench.BucketModel,
		},

		// unattributed
		"a fuzzy edit that applied taints the session": {
			events: []journal.Payload{
				journal.EditApplied{Path: "a.go", Mode: tools.ModeFuzzy},
				journal.SyntaxGateRun{Path: "a.go", Checker: "gofmt", Ran: true, ExitCode: 0},
			},
			want: bench.BucketUnattributed,
		},
		"a fuzzy edit that was refused taints it too": {
			events: []journal.Payload{
				journal.EditRejected{Path: "a.go", Mode: tools.ModeFuzzy, Reason: "below_floor"},
			},
			want: bench.BucketUnattributed,
		},
		"an anchored rejection does not": {
			events: []journal.Payload{
				journal.EditRejected{Path: "a.go", Mode: "anchored", Reason: "anchor_drift"},
			},
			want: bench.BucketModel,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := classify(t, recordSession(t, tc.events...)); got != tc.want {
				t.Errorf("bucket = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifySplitsVerificationThatCouldNotRun is KAN-787's asymmetry, which
// is the one rule the card text does not state and the one whose wrong answer
// flatters SLICE-1's acceptance bar rather than breaking it.
//
// A verification that ran and failed is the model's: the harness ran the suite,
// showed the model the output, and the model stopped anyway. A verification
// that could not run is the harness's — it proceeded with no evidence —
// *unless* the project simply has no verification command, which is a project
// shape and not a defect.
func TestClassifySplitsVerificationThatCouldNotRun(t *testing.T) {
	cases := map[string]struct {
		run  journal.VerificationRun
		stop string
		want bench.Bucket
	}{
		"no command configured and none discovered is not a harness failure": {
			run:  journal.VerificationRun{Source: string(verify.SourceNone), ExitCode: -1},
			stop: engine.StopCompleted.Reason(),
			want: bench.BucketModel,
		},
		"a configured command that could not run is": {
			run: journal.VerificationRun{
				Command: []string{"uv", "run", "pytest"}, Source: string(verify.SourceConfigured), ExitCode: -1,
			},
			stop: engine.StopCompleted.Reason(),
			want: bench.BucketHarness,
		},
		"a discovered command that could not run is": {
			run: journal.VerificationRun{
				Command: []string{"make", "test"}, Source: string(verify.SourceDiscovered), ExitCode: -1,
			},
			stop: engine.StopCompleted.Reason(),
			want: bench.BucketHarness,
		},
		"a command that ran and failed is the model's": {
			run: journal.VerificationRun{
				Command: []string{"go", "test", "./..."}, Source: string(verify.SourceDiscovered), ExitCode: 1,
			},
			stop: engine.StopVerificationFailed.Reason(),
			want: bench.BucketModel,
		},
		"a command that ran and passed is the model's": {
			run: journal.VerificationRun{
				Command: []string{"go", "test", "./..."}, Source: string(verify.SourceDiscovered), ExitCode: 0,
			},
			stop: engine.StopCompleted.Reason(),
			want: bench.BucketModel,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := recordSession(t, tc.run)
			r.Stop = tc.stop
			if got := classify(t, r); got != tc.want {
				t.Errorf("bucket = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyReadsTheStop holds the whole of internal/engine/stop.go's bucket
// column, including the two the loop deliberately charges to the model and the
// fail-closed default.
func TestClassifyReadsTheStop(t *testing.T) {
	cases := map[string]struct {
		stop string
		want bench.Bucket
	}{
		"completed":            {stop: engine.StopCompleted.Reason(), want: bench.BucketModel},
		"max turns":            {stop: engine.StopMaxTurns.Reason(), want: bench.BucketHarness},
		"budget exhausted":     {stop: engine.StopBudgetExhausted.Reason(), want: bench.BucketModel},
		"verification failed":  {stop: engine.StopVerificationFailed.Reason(), want: bench.BucketModel},
		"cancelled":            {stop: engine.StopCancelled.Reason(), want: bench.BucketUnclassified},
		"provider error":       {stop: engine.StopProviderError.Reason(), want: bench.BucketHarness},
		"harness error":        {stop: engine.StopHarnessError.Reason(), want: bench.BucketHarness},
		"a stop nobody mapped": {stop: "stop(99)", want: bench.BucketHarness},
		"a stop nobody wrote":  {stop: "", want: bench.BucketHarness},
		"unspecified":          {stop: engine.StopUnspecified.Reason(), want: bench.BucketHarness},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := recordSession(t)
			r.Stop = tc.stop
			if got := classify(t, r); got != tc.want {
				t.Errorf("bucket = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyReadsACancellationOffTheRecord is KAN-857's half of rule 0.
//
// Before that card there was nothing in the journal to read, so a cancellation
// could only be recognised through [bench.TaskResult].Stop — the runner's
// in-memory summary of it. The runner overwrites Stop with a harness error when
// tearing a task down fails, and a cancelled run is exactly when teardown is
// most likely to fail, so the fact could be lost between the session and the
// classifier. Now it is on the record and read from there.
func TestClassifyReadsACancellationOffTheRecord(t *testing.T) {
	cases := map[string]struct {
		events []journal.Payload
		stop   string
		want   bench.Bucket
	}{
		"a cancelled turn is nothing to attribute": {
			events: []journal.Payload{
				journal.TurnCancelled{Phase: "provider_stream", Detail: journal.InlineText("context canceled")},
			},
			stop: engine.StopCompleted.Reason(),
			want: bench.BucketUnclassified,
		},
		"a cancelled session is nothing to attribute": {
			events: []journal.Payload{
				journal.SessionEnded{Reason: engine.StopCancelled.Reason(), ExitCode: 1},
			},
			stop: engine.StopCompleted.Reason(),
			want: bench.BucketUnclassified,
		},
		"a cancellation outranks the harness signal beside it": {
			// The case the record exists for: the operator interrupted the run,
			// reclaiming the worktree then failed, and the runner replaced the
			// stop with its own breakage. Charging the task to `harness` would
			// put a row the operator caused into the one tally with an
			// acceptance bar of zero.
			events: []journal.Payload{
				journal.TurnCancelled{Phase: "tool_call", Detail: journal.InlineText("context canceled")},
				journal.ToolResult{CallID: "c1", Tool: "run_shell", ErrorKind: tools.FaultInternal.String()},
			},
			stop: engine.StopHarnessError.Reason(),
			want: bench.BucketUnclassified,
		},
		"a session with no cancellation is classified as before": {
			events: []journal.Payload{
				journal.ToolResult{CallID: "c1", Tool: "run_shell", ErrorKind: tools.FaultInternal.String()},
			},
			stop: engine.StopHarnessError.Reason(),
			want: bench.BucketHarness,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := recordSession(t, tc.events...)
			r.Stop = tc.stop
			if got := classify(t, r); got != tc.want {
				t.Errorf("bucket = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyReadsWhatTheJournalCannotHold covers the two facts about a task
// that happen outside the session: the panic the runner recovered, and the
// oracle's own execution.
func TestClassifyReadsWhatTheJournalCannotHold(t *testing.T) {
	cases := map[string]struct {
		mutate func(*bench.TaskResult)
		want   bench.Bucket
	}{
		"a recovered panic is the harness's": {
			mutate: func(r *bench.TaskResult) { r.Panicked = true },
			want:   bench.BucketHarness,
		},
		"an oracle that could not be run is the harness's": {
			mutate: func(r *bench.TaskResult) {
				r.Oracle = bench.OracleResult{ExitCode: -1, Err: errors.New("executable file not found")}
			},
			want: bench.BucketHarness,
		},
		"an oracle that timed out is the harness's": {
			mutate: func(r *bench.TaskResult) {
				r.Oracle = bench.OracleResult{ExitCode: -1, TimedOut: true}
			},
			want: bench.BucketHarness,
		},
		"an oracle killed by a signal is the harness's": {
			mutate: func(r *bench.TaskResult) {
				r.Oracle = bench.OracleResult{ExitCode: -1, Signal: "killed"}
			},
			want: bench.BucketHarness,
		},
		"an oracle that answered no is the model's": {
			mutate: func(r *bench.TaskResult) { r.Oracle = bench.OracleResult{ExitCode: 1} },
			want:   bench.BucketModel,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := recordSession(t)
			tc.mutate(&r)
			if got := classify(t, r); got != tc.want {
				t.Errorf("bucket = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClassifyPrecedence is the order the package doc argues for, driven by
// sessions that trip two rules at once.
func TestClassifyPrecedence(t *testing.T) {
	fuzzy := journal.EditApplied{Path: "a.go", Mode: tools.ModeFuzzy}

	t.Run("harness beats unattributed", func(t *testing.T) {
		// A fuzzy edit *and* a repair budget spent. Charging this to
		// `unattributed` would move a known harness failure out of the one
		// bucket whose acceptance bar is zero.
		r := recordSession(t, fuzzy, journal.ToolCallFailed{CallID: "c1", Reason: "invalid_json"})
		if got := classify(t, r); got != bench.BucketHarness {
			t.Errorf("bucket = %q, want %q", got, bench.BucketHarness)
		}
	})

	t.Run("harness beats unattributed on a result-level signal too", func(t *testing.T) {
		r := recordSession(t, fuzzy)
		r.Stop = engine.StopMaxTurns.Reason()
		if got := classify(t, r); got != bench.BucketHarness {
			t.Errorf("bucket = %q, want %q", got, bench.BucketHarness)
		}
	})

	t.Run("unattributed beats model whatever the stop", func(t *testing.T) {
		// The clean stop is exactly the case the bucket exists for: a fuzzy
		// match above the floor and in the wrong place applies without an
		// error and the session finishes tidily.
		for _, stop := range []string{
			engine.StopCompleted.Reason(),
			engine.StopBudgetExhausted.Reason(),
			engine.StopVerificationFailed.Reason(),
		} {
			r := recordSession(t, fuzzy)
			r.Stop = stop
			if got := classify(t, r); got != bench.BucketUnattributed {
				t.Errorf("stop %q: bucket = %q, want %q", stop, got, bench.BucketUnattributed)
			}
		}
	})

	t.Run("nothing to attribute beats everything", func(t *testing.T) {
		// A task the oracle passed is not a failure, and neither is one the
		// runner abandoned. Both must stay out of a tally that counts
		// failures, however messy the session behind them was.
		for name, mutate := range map[string]func(*bench.TaskResult){
			"passed":    func(r *bench.TaskResult) { r.Passed = true; r.Oracle = bench.OracleResult{Passed: true} },
			"cancelled": func(r *bench.TaskResult) { r.Stop = engine.StopCancelled.Reason() },
		} {
			r := recordSession(t, fuzzy, journal.ToolCallFailed{CallID: "c1", Reason: "invalid_json"})
			mutate(&r)
			if got := classify(t, r); got != bench.BucketUnclassified {
				t.Errorf("%s: bucket = %q, want %q", name, got, bench.BucketUnclassified)
			}
		}
	})
}

// TestClassifyRefusesToAttributeAnUnreadableRecord is the fail-loud direction.
// A bucket is a claim about what went wrong; a record nobody could read
// supports none of them, so the classifier says so and the runner prints
// "unclassified" rather than defaulting into a bucket.
func TestClassifyRefusesToAttributeAnUnreadableRecord(t *testing.T) {
	t.Run("no journal directory", func(t *testing.T) {
		r := bench.TaskResult{TaskID: "task", Stop: engine.StopCompleted.Reason()}
		assertNoRecord(t, r)
	})

	t.Run("a journal that is not there", func(t *testing.T) {
		r := bench.TaskResult{
			TaskID:     "task",
			JournalDir: filepath.Join(t.TempDir(), "sessions", "gone"),
			Stop:       engine.StopCompleted.Reason(),
		}
		assertNoRecord(t, r)
	})

	t.Run("a line a crash caught in flight", func(t *testing.T) {
		r := recordSession(t, journal.EditApplied{Path: "a.go", Mode: "anchored"})
		truncateLastLine(t, filepath.Join(r.JournalDir, journal.EventsFile))
		assertNoRecord(t, r)
	})

	t.Run("a line that is not an event", func(t *testing.T) {
		r := recordSession(t, journal.EditApplied{Path: "a.go", Mode: "anchored"})
		appendLine(t, filepath.Join(r.JournalDir, journal.EventsFile), "{ not an event\n")
		assertNoRecord(t, r)
	})

	t.Run("but a failure the runner itself saw is still attributed", func(t *testing.T) {
		// The record is gone and the bucket is already decided from the
		// result. Refusing here would report "nobody looked" about a panic the
		// runner watched happen.
		r := bench.TaskResult{TaskID: "task", Stop: engine.StopHarnessError.Reason(), Panicked: true}
		got, err := bench.Attribution{}.Classify(t.Context(), r)
		if err != nil {
			t.Fatalf("Classify: %v", err)
		}
		if got != bench.BucketHarness {
			t.Errorf("bucket = %q, want %q", got, bench.BucketHarness)
		}
	})
}

func assertNoRecord(t *testing.T, r bench.TaskResult) {
	t.Helper()
	got, err := bench.Attribution{}.Classify(t.Context(), r)
	if !errors.Is(err, bench.ErrNoRecord) {
		t.Errorf("error = %v, want one wrapping ErrNoRecord", err)
	}
	if got != bench.BucketUnclassified {
		t.Errorf("bucket = %q, want %q", got, bench.BucketUnclassified)
	}
}

func truncateLastLine(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := os.WriteFile(path, b[:len(b)-1], 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatalf("appending to %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing %s: %v", path, err)
	}
}

// TestClassifyReadsAnOversizedEventLine guards the reader's buffer choice. A
// tool result under the journal's blob threshold stays inline, and an event
// line is then far longer than bufio.Scanner's default limit — a reader that
// gave up on it would turn a session with one big diagnostic into an
// unclassifiable one, which is the same laundering by another route.
func TestClassifyReadsAnOversizedEventLine(t *testing.T) {
	r := recordSession(t,
		journal.ToolResult{
			CallID: "c1",
			Tool:   "run_shell",
			Output: journal.InlineText(strings.Repeat("x", 200*1024)),
		},
		journal.EditApplied{Path: "a.go", Mode: tools.ModeFuzzy},
	)
	if got := classify(t, r); got != bench.BucketUnattributed {
		t.Errorf("bucket = %q, want %q — the event after the long line was not read", got,
			bench.BucketUnattributed)
	}
}

// TestBucketsCountFailuresOnly is the tally the report prints. A passing task
// belongs to no bucket, so the four counts sum to the failure count exactly.
func TestBucketsCountFailuresOnly(t *testing.T) {
	res := &bench.RunResult{Tasks: []bench.TaskResult{
		{TaskID: "a", Passed: true},
		{TaskID: "b", Passed: true, Bucket: bench.BucketModel},
		{TaskID: "c", Bucket: bench.BucketHarness},
		{TaskID: "d", Bucket: bench.BucketUnattributed},
		{TaskID: "e", Bucket: bench.BucketUnattributed},
		{TaskID: "f", Bucket: bench.BucketModel},
		{TaskID: "g"},
	}}

	got := res.Buckets()
	want := bench.BucketCounts{Failed: 5, Harness: 1, Unattributed: 2, Model: 1, Unclassified: 1}
	if got != want {
		t.Errorf("Buckets() = %+v, want %+v", got, want)
	}
	if sum := got.Harness + got.Unattributed + got.Model + got.Unclassified; sum != got.Failed {
		t.Errorf("the buckets sum to %d, want %d", sum, got.Failed)
	}
}

// TestReportStatesTheUnattributedSizeEveryTime is ADR-0006 §3's "report its
// size every time", and the reason it is a test: a bucket printed only when it
// is non-zero is silent when it is empty and silent when nobody computed it,
// and those are opposite claims.
func TestReportStatesTheUnattributedSizeEveryTime(t *testing.T) {
	cases := map[string]struct {
		tasks []bench.TaskResult
		want  []string
	}{
		"a run with nothing to attribute still prints the tally": {
			tasks: []bench.TaskResult{{TaskID: "a", Passed: true}},
			want: []string{
				"attribution over 0 failed task(s): harness 0, unattributed 0, model 0, unclassified 0",
				"unattributed = 0",
			},
		},
		"an empty unattributed bucket is printed beside a full one": {
			tasks: []bench.TaskResult{
				{TaskID: "a", Bucket: bench.BucketModel},
				{TaskID: "b", Bucket: bench.BucketModel},
			},
			want: []string{
				"attribution over 2 failed task(s): harness 0, unattributed 0, model 2, unclassified 0",
				"unattributed = 0",
			},
		},
		"a harness failure is called out against the slice bar": {
			tasks: []bench.TaskResult{{TaskID: "a", Bucket: bench.BucketHarness}},
			want: []string{
				"attribution over 1 failed task(s): harness 1, unattributed 0, model 0, unclassified 0",
				"1 failure(s) classified `harness`",
			},
		},
		"an unrun classifier says so rather than reading as a clean bill": {
			tasks: []bench.TaskResult{{TaskID: "a"}},
			want: []string{
				"unattributed = 0",
				"1 failure(s) were NOT attributed",
				"This is not a clean bill of health",
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			if err := bench.WriteReport(&b, &bench.RunResult{Tasks: tc.tasks}); err != nil {
				t.Fatalf("WriteReport: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("the report does not say %q:\n%s", want, b.String())
				}
			}
		})
	}
}

// TestRunnerClassifiesWhatItRan drives the classifier through the runner
// rather than calling it directly, because the wiring is half of what this card
// owes: an agent that records a fuzzy edit and stops cleanly must come back
// `unattributed`, and the same run with no classifier must come back
// unclassified. The second half is KAN-796's property and this is what stops it
// being lost — a nil classifier that quietly defaulted into a bucket would make
// a run nobody attributed look like a run with nothing wrong.
func TestRunnerClassifiesWhatItRan(t *testing.T) {
	// The agent never writes the "fixed" file, so every oracle fails and every
	// task has a failure to attribute.
	fuzzyAgent := &fakeAgent{run: func(ctx context.Context, spec bench.SessionSpec) (bench.SessionOutcome, error) {
		j, err := journal.Open(spec.OutDir, spec.SessionID)
		if err != nil {
			return bench.SessionOutcome{}, err
		}
		defer func() { _ = j.Close() }()
		if _, err := j.Append(ctx, 1, journal.EditApplied{
			Path: "a.go", Mode: tools.ModeFuzzy, Diff: journal.InlineText("-a\n+b\n"),
		}); err != nil {
			return bench.SessionOutcome{}, err
		}
		return bench.SessionOutcome{Stop: engine.StopCompleted.Reason(), Turns: 1}, nil
	}}

	t.Run("with a classifier", func(t *testing.T) {
		f := newFixture(t)
		r := newRunner(t, f, fuzzyAgent, func(r *bench.Runner) { r.Classifier = bench.Attribution{} })
		res, err := r.Run(t.Context())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, tr := range res.Tasks {
			if tr.Bucket != bench.BucketUnattributed {
				t.Errorf("%s: bucket = %q, want %q", tr.TaskID, tr.Bucket, bench.BucketUnattributed)
			}
		}
		if got := res.Buckets(); got.Unattributed != len(res.Tasks) || got.Unclassified != 0 {
			t.Errorf("Buckets() = %+v, want every failure unattributed", got)
		}
	})

	t.Run("without one", func(t *testing.T) {
		f := newFixture(t)
		res, err := newRunner(t, f, fuzzyAgent).Run(t.Context())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, tr := range res.Tasks {
			if tr.Bucket != bench.BucketUnclassified {
				t.Errorf("%s: bucket = %q, want %q — a nil classifier must attribute nothing",
					tr.TaskID, tr.Bucket, bench.BucketUnclassified)
			}
		}
		if got := res.Buckets(); got.Unclassified != len(res.Tasks) {
			t.Errorf("Buckets() = %+v, want every failure unclassified", got)
		}
	})
}

// TestReportDoesNotCallAPassingTaskUnclassified keeps the two meanings of an
// empty bucket apart in the per-task table: a passing row has nothing to
// attribute, and a failing row with no bucket means nobody looked.
func TestReportDoesNotCallAPassingTaskUnclassified(t *testing.T) {
	var b strings.Builder
	err := bench.WriteReport(&b, &bench.RunResult{Tasks: []bench.TaskResult{
		{TaskID: "passing", Passed: true},
		{TaskID: "failing"},
	}})
	if err != nil {
		t.Fatalf("WriteReport: %v", err)
	}

	for _, line := range strings.Split(b.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "passing":
			if got := fields[len(fields)-1]; got != "n/a" {
				t.Errorf("a passing task's bucket reads %q, want %q", got, "n/a")
			}
		case "failing":
			if got := fields[len(fields)-1]; got != "unclassified" {
				t.Errorf("an unattributed failure reads %q, want %q", got, "unclassified")
			}
		}
	}
}
