package engine_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/tools"
	"github.com/leejianrong/kopicode/internal/verify"
)

// Forced verification at the loop's seam (KAN-787, docs/SLICE-1.md §5).
//
// internal/verify owns discovery and the subprocess, and its own tests cover
// both. What is under test here is the three things only the engine can get
// wrong: running it after the turns that changed something and not the others,
// journaling the full output, and refusing to call a session completed over a
// tree the project's own command rejected.

// --- the helper subprocess ----------------------------------------------
//
// Verification shells out, and the engine's tests must not depend on what is
// installed on the machine running them. So the command is this test binary,
// re-executed with an argument TestMain recognises; the Verifier's LookPath
// seam is what points argv[0] at it.

const (
	verifyHelperEnv  = "KOPICODE_ENGINE_VERIFY_HELPER"
	verifyHelperFlag = "--kopicode-verify-helper"
)

func TestMain(m *testing.M) {
	// Both conditions, so neither an inherited environment variable nor a stray
	// argument can put an ordinary test run into helper mode.
	if os.Getenv(verifyHelperEnv) != "" && len(os.Args) > 2 && os.Args[1] == verifyHelperFlag {
		os.Exit(verifyHelperMain(os.Args[2]))
	}
	os.Exit(m.Run())
}

// verifyHelperMain is the fixture command. It is not a test and never returns
// to the testing package.
func verifyHelperMain(mode string) int {
	switch mode {
	case "pass":
		fmt.Println("ok  \texample.com/x\t0.014s")
		return 0
	case "fail":
		fmt.Println("--- FAIL: TestGreetsAnEmptyName")
		fmt.Fprintln(os.Stderr, "FAIL\texample.com/x\t0.021s")
		return 1
	case "big":
		for i := range 3000 {
			fmt.Printf("verification line %04d: %s\n", i, strings.Repeat("y", 60))
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		return 2
	}
}

// withVerify installs v as this session's verifier, rooted at the harness's own
// tree. Root is filled in here rather than by the caller because the temp tree
// does not exist until newHarness builds it.
func withVerify(v *verify.Verifier) harnessOption {
	return func(c *engine.Config) {
		v.Root = c.Tools.Root.Path()
		c.Verify = v
	}
}

// withVerificationForced sets whether the arm forces verification. It is a
// harness configuration field and is in the hash preimage (ADR-0007 decision 6),
// so it is set here rather than as an engine flag.
func withVerificationForced(forced bool) harnessOption {
	return func(c *engine.Config) { c.Selection.Config.Verification.Forced = forced }
}

// helperVerifier is a Verifier whose configured command is the helper in mode.
// Root is left empty and filled in by the harness once the temp tree exists.
func helperVerifier(t *testing.T, mode string) *verify.Verifier {
	t.Helper()
	t.Setenv(verifyHelperEnv, "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	return &verify.Verifier{
		// argv[0] is a plain name because it is what reaches the journal.
		Configured: []string{"project-verify", verifyHelperFlag, mode},
		LookPath:   func(string) (string, error) { return exe, nil },
	}
}

// withHelperVerification wires the helper in mode as this session's
// verification command.
func withHelperVerification(t *testing.T, mode string) harnessOption {
	t.Helper()
	return withVerify(helperVerifier(t, mode))
}

// recordingProvider remembers the requests the loop sent, so a test can assert
// what the model was shown next without reaching into the loop's state.
type recordingProvider struct {
	inner    engine.Provider
	requests []provider.Request
}

func (r *recordingProvider) Complete(ctx context.Context, req provider.Request) (*provider.Stream, error) {
	r.requests = append(r.requests, req)
	return r.inner.Complete(ctx, req)
}

// writeThenStop is the two-turn script every test below runs: turn 1 writes a
// file, turn 2 says it is done. The write is what earns the turn a verification.
func writeThenStop() []scriptedReply {
	return []scriptedReply{
		{calls: []wireCall{nativeCall("call-write", tools.ToolWriteFile,
			`{"path":"greet.go","content":"package greet\n"}`)}},
		{text: "Done — the change is in place."},
	}
}

// --- when it runs -------------------------------------------------------

// TestAModifyingTurnIsVerified is the card's "run it after any turn that
// modified files", and the adjacency matters: the verification describes the
// tree the snapshot recorded, so it follows the snapshot rather than preceding
// it.
func TestAModifyingTurnIsVerified(t *testing.T) {
	h := scriptHarness(t, writeThenStop(), oneAttemptPerTurn(2), nil,
		withMaxTurns(2), withHelperVerification(t, "pass"))

	if _, err := h.eng.Run(t.Context(), "add a greeting"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run := sole[journal.VerificationRun](t, h.events())
	if run.ExitCode != 0 {
		t.Errorf("VerificationRun exit code = %d, want 0", run.ExitCode)
	}
	if run.Source != string(verify.SourceConfigured) {
		t.Errorf("VerificationRun source = %q, want %q", run.Source, verify.SourceConfigured)
	}
	if len(run.Command) == 0 || run.Command[0] != "project-verify" {
		t.Errorf("VerificationRun command = %v; it is argv and argv[0] is the plain binary name",
			run.Command)
	}
	if !strings.Contains(run.Output.Inline, "ok  \texample.com/x") {
		t.Errorf("VerificationRun lost the command's output:\n%s", run.Output.Inline)
	}

	types := h.types()
	snapAt, verifyAt := -1, -1
	for i, ty := range types {
		switch ty {
		case journal.TypeTurnSnapshot:
			snapAt = i
		case journal.TypeVerificationRun:
			verifyAt = i
		}
	}
	if snapAt < 0 || verifyAt < 0 || verifyAt < snapAt {
		t.Errorf("VerificationRun does not follow TurnSnapshot:\n%v", types)
	}
}

// TestATurnThatChangedNothingIsNotVerified is the other half. A read-only turn
// has nothing to verify, and running a project's whole suite after every read
// would make the loop unusable long before it made it wrong.
func TestATurnThatChangedNothingIsNotVerified(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-read", tools.ToolReadFile, `{"path":"greet.go"}`)}},
		{text: "I have read it."},
	}
	h := scriptHarness(t, replies, oneAttemptPerTurn(2), map[string]string{"greet.go": greetGo},
		withMaxTurns(2), withHelperVerification(t, "fail"))

	res, err := h.eng.Run(t.Context(), "read it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("Stop = %s, want completed; nothing was modified, so the failing verification "+
			"command should never have run", res.Stop)
	}
	for _, ty := range h.types() {
		if ty == journal.TypeVerificationRun {
			t.Fatalf("a read-only turn was verified:\n%v", h.types())
		}
	}
}

// TestAnArmThatDoesNotForceVerificationDoesNotVerify holds the bound to the
// harness configuration. Verification.Forced is in the hash preimage, so a run
// that verified under a configuration saying it does not would be an arm doing
// something the value identifying it denies.
func TestAnArmThatDoesNotForceVerificationDoesNotVerify(t *testing.T) {
	h := scriptHarness(t, writeThenStop(), oneAttemptPerTurn(2), nil,
		withMaxTurns(2), withVerificationForced(false), withHelperVerification(t, "fail"))

	res, err := h.eng.Run(t.Context(), "add a greeting")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("Stop = %s, want completed", res.Stop)
	}
	for _, ty := range h.types() {
		if ty == journal.TypeVerificationRun {
			t.Fatalf("an arm with Verification.Forced false ran verification anyway:\n%v", h.types())
		}
	}
}

// --- refusing to report success -----------------------------------------

// TestAFailedVerificationRefusesToReportSuccess is the load-bearing half of
// KAN-787.
//
// The model modified a file, the project's own command rejected the result, and
// the model then said it was finished. "Completed" is exactly the word forced
// verification exists to withhold, and the exit code and the session record have
// to say so too — a Stop that differed while SessionEnded still read "completed"
// would leave the classifier reading the flattering half.
func TestAFailedVerificationRefusesToReportSuccess(t *testing.T) {
	h := scriptHarness(t, writeThenStop(), oneAttemptPerTurn(2), nil,
		withMaxTurns(2), withHelperVerification(t, "fail"))

	res, err := h.eng.Run(t.Context(), "add a greeting")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Stop == engine.StopCompleted {
		t.Fatal("the engine reported success over a tree the project's own verification command " +
			"rejected (docs/SLICE-1.md §5)")
	}
	if res.Stop != engine.StopVerificationFailed {
		t.Errorf("Stop = %s, want verification_failed", res.Stop)
	}
	if got := res.Stop.ExitCode(); got != 1 {
		t.Errorf("exit code = %d, want 1 (task not completed)", got)
	}

	run := sole[journal.VerificationRun](t, h.events())
	if run.ExitCode == 0 {
		t.Errorf("VerificationRun records exit 0 for the command that failed")
	}
	if !strings.Contains(run.Output.Inline, "--- FAIL: TestGreetsAnEmptyName") {
		t.Errorf("VerificationRun lost the failure the model needs to see:\n%s", run.Output.Inline)
	}

	if err := h.eng.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ended := sole[journal.SessionEnded](t, h.events())
	if ended.Reason == "completed" {
		t.Error("SessionEnded records \"completed\" for a session whose verification failed")
	}
	if ended.Reason != "verification_failed" || ended.ExitCode != 1 {
		t.Errorf("SessionEnded = %s/%d, want verification_failed/1", ended.Reason, ended.ExitCode)
	}
	if ended.Detail.Inline == "" {
		t.Error("SessionEnded says verification failed and not which command said so")
	}
}

// TestAFailedVerificationBecomesTheNextTurnsObservation is the other half of
// SLICE-1 §5's sentence. The failure is not merely recorded and held against the
// model: the model is shown it and gets to fix it, which is the whole point of
// running the suite inside the loop rather than after it.
func TestAFailedVerificationBecomesTheNextTurnsObservation(t *testing.T) {
	p := script(t, writeThenStop(), oneAttemptPerTurn(2))
	spy := &recordingProvider{inner: p}
	h := newHarness(t, p, nil,
		withMaxTurns(2), withProvider(spy), withHelperVerification(t, "fail"))

	if _, err := h.eng.Run(t.Context(), "add a greeting"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(spy.requests) < 2 {
		t.Fatalf("the loop sent %d requests; the second turn is where the observation lands",
			len(spy.requests))
	}
	last := spy.requests[len(spy.requests)-1]

	found := false
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "--- FAIL: TestGreetsAnEmptyName") {
			found = true
		}
	}
	if !found {
		t.Errorf("the verification failure never reached the model:\n%+v", last.Messages)
	}
}

// TestAPassingVerificationTellsTheModel is KAN-960's fix. A model that never
// learns a passing verification even ran has no way to tell "nobody has
// checked yet" from "it already passed" — and unattended mode denies every
// run_shell call it tries instead, so a model that does not trust silence has
// no move left but to keep retrying the same denied call, sometimes past
// max_turns. A pass now answers back, in one line rather than the whole
// stream: the model's confirmation costs a few tokens once, not the entire
// suite's output every time it recurs.
func TestAPassingVerificationTellsTheModel(t *testing.T) {
	p := script(t, writeThenStop(), oneAttemptPerTurn(2))
	spy := &recordingProvider{inner: p}
	h := newHarness(t, p, nil,
		withMaxTurns(2), withProvider(spy), withHelperVerification(t, "big"))

	if _, err := h.eng.Run(t.Context(), "add a greeting"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(spy.requests) < 2 {
		t.Fatalf("the loop sent %d requests; the second turn is where the confirmation lands",
			len(spy.requests))
	}
	last := spy.requests[len(spy.requests)-1]

	found := false
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "verification:") && strings.Contains(m.Content, "passed") {
			found = true
		}
		if strings.Contains(m.Content, "verification line 0000") {
			t.Errorf("a pass sent the whole 3000-line stream to the model rather than a one-line "+
				"summary:\n%+v", last.Messages)
		}
	}
	if !found {
		t.Errorf("a passing verification never told the model it ran at all:\n%+v", last.Messages)
	}
}

// TestAPassingVerificationAnswersAnEarlierFailure is what makes the rule usable
// rather than a trap. A model that fixes what the suite complained about must be
// able to finish; only a later passing run clears an earlier rejection.
func TestAPassingVerificationAnswersAnEarlierFailure(t *testing.T) {
	replies := []scriptedReply{
		{calls: []wireCall{nativeCall("call-1", tools.ToolWriteFile,
			`{"path":"greet.go","content":"package greet\n"}`)}},
		{calls: []wireCall{nativeCall("call-2", tools.ToolWriteFile,
			`{"path":"greet.go","content":"package greet // fixed\n"}`)}},
		{text: "Fixed, and the suite is green."},
	}

	// The first turn fails and the second passes, so the verifier's command has
	// to change between them. Swapping the Configured argv is the smallest way
	// to say that, and it is safe because the loop is single-threaded.
	v := helperVerifier(t, "fail")
	h := scriptHarness(t, replies, oneAttemptPerTurn(3), nil, withMaxTurns(3), withVerify(v))

	// Plan() clones the configured argv before LookPath is reached, so flipping
	// the mode here takes effect on the *next* run: the first verification fails
	// and every one after it passes.
	original := v.LookPath
	v.LookPath = func(name string) (string, error) {
		v.Configured[2] = "pass"
		return original(name)
	}

	res, err := h.eng.Run(t.Context(), "add a greeting")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("Stop = %s, want completed; the second verification passed, and a rejection that "+
			"cannot be answered would make the loop unable to finish any task it ever failed",
			res.Stop)
	}
}

// --- what does not block ------------------------------------------------

// TestAVerificationThatCouldNotRunIsRecordedAndDoesNotBlock is the honest-none
// case at the loop's seam, and both halves matter.
//
// Recorded, because a silent skip is how a promised gate stops being one: the
// record has to say that nothing vouched for this tree. Not blocking, because a
// project kopicode does not recognise is not evidence the model's change is
// wrong, and charging it as one would make every task on such a project
// unfinishable.
func TestAVerificationThatCouldNotRunIsRecordedAndDoesNotBlock(t *testing.T) {
	// No configured command and a temp tree with no makefile, no go.mod, no
	// package.json and no pyproject.toml — discovery has nothing to find.
	h := scriptHarness(t, writeThenStop(), oneAttemptPerTurn(2), nil, withMaxTurns(2))

	res, err := h.eng.Run(t.Context(), "add a greeting")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Errorf("Stop = %s, want completed", res.Stop)
	}

	run := sole[journal.VerificationRun](t, h.events())
	if run.Source != string(verify.SourceNone) {
		t.Errorf("VerificationRun source = %q, want %q", run.Source, verify.SourceNone)
	}
	if len(run.Command) != 0 {
		t.Errorf("VerificationRun records the command %v for a session that ran none", run.Command)
	}
	if run.ExitCode != -1 {
		t.Errorf("VerificationRun exit code = %d for a command that never ran; 0 there reads as a "+
			"pass to anything that does not also read Source", run.ExitCode)
	}
	if run.Skip != string(verify.SkipNoCommand) {
		t.Errorf("VerificationRun skip = %q, want %q — KAN-876's field must reach the journal from "+
			"the engine loop, not just from internal/verify's own Result", run.Skip, verify.SkipNoCommand)
	}
	if !strings.Contains(run.Output.Inline, "nothing here is a pass") {
		t.Errorf("the record does not say in words that nothing vouched for this tree:\n%s",
			run.Output.Inline)
	}
}

// TestANilVerifierIsNotASkip guards the direction the engine's Config field
// leans. A gate whose absence reads as a pass is the failure internal/syntax
// argues at length about, so forgetting the field must produce a real verifier
// over the session's own root — which the test above already relies on, and this
// one states outright.
func TestANilVerifierIsNotASkip(t *testing.T) {
	h := scriptHarness(t, writeThenStop(), oneAttemptPerTurn(2), nil, withMaxTurns(2))

	if _, err := h.eng.Run(t.Context(), "add a greeting"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	saw := false
	for _, ty := range h.types() {
		if ty == journal.TypeVerificationRun {
			saw = true
		}
	}
	if !saw {
		t.Errorf("a Config with no Verify skipped verification entirely; nil must mean the default "+
			"verifier, not \"do not check\":\n%v", h.types())
	}
}

// --- the record ---------------------------------------------------------

// TestVerificationOutputIsJournaledWhole is CLAUDE.md's rule at the place it is
// most likely to be broken: a test suite's output is the largest thing this loop
// writes, and clipping the diagnostic that justifies the next fix is the
// specific failure this project is designed out of.
func TestVerificationOutputIsJournaledWhole(t *testing.T) {
	h := scriptHarness(t, writeThenStop(), oneAttemptPerTurn(2), nil,
		withMaxTurns(2), withHelperVerification(t, "big"))

	if _, err := h.eng.Run(t.Context(), "add a greeting"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run := sole[journal.VerificationRun](t, h.events())
	if !run.Output.Spilled() {
		t.Fatalf("output of %d bytes was not spilled to a blob; a suite's output is the largest "+
			"thing this loop writes and the spill is what keeps it whole", run.Output.Size)
	}
	// Read puts the content back, so this is the value that was appended.
	// Nothing may be missing from it.
	if int64(len(run.Output.Inline)) != run.Output.Size {
		t.Errorf("rehydrated %d bytes of a %d-byte value: something was clipped",
			len(run.Output.Inline), run.Output.Size)
	}
	for _, want := range []string{"verification line 0000:", "verification line 1500:", "verification line 2999:"} {
		if !strings.Contains(run.Output.Inline, want) {
			t.Errorf("the journaled verification output is missing %q", want)
		}
	}
}
