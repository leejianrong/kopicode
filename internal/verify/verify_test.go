package verify_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/verify"
)

// --- the helper subprocess ----------------------------------------------
//
// Verification shells out, so testing it needs something to shell out *to*, and
// every candidate on a real machine is a hazard: `make` is not installed
// everywhere, `go test` in a fixture would compile a package, and a shell script
// is a portability question this repository has already answered once
// (make xbuild builds these files for Windows too).
//
// So the command under test is this test binary, re-executed with an argument
// that TestMain recognises. It exits with the code the case asks for, writes
// what the case asks for, and hangs when the case is about a timeout — none of
// which depends on what is installed. The Verifier's LookPath seam is what
// points argv[0] at it, which is the same seam internal/syntax uses to test a
// missing toolchain.

const (
	helperEnv  = "KOPICODE_VERIFY_HELPER"
	helperFlag = "--kopicode-helper"
)

func TestMain(m *testing.M) {
	// Both conditions, so neither an inherited environment variable nor a
	// stray argument can put an ordinary test run into helper mode.
	if os.Getenv(helperEnv) != "" && len(os.Args) > 2 && os.Args[1] == helperFlag {
		os.Exit(helperMain(os.Args[2], os.Args[3:]))
	}
	os.Exit(m.Run())
}

// helperMain is the fixture command. It is not a test and never returns to the
// testing package.
func helperMain(mode string, args []string) int {
	switch mode {
	case "pass":
		fmt.Println("all 12 tests passed")
		return 0
	case "fail":
		fmt.Println("--- FAIL: TestThing")
		fmt.Fprintln(os.Stderr, "exit status 1")
		return 3
	case "hang":
		// Marks that it started, then waits to be killed. Nothing here signals
		// the parent; the parent's cancellation is driven through the LookPath
		// seam so no test has to wait for this file to appear.
		_ = os.WriteFile("started", nil, 0o644)
		select {}
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		return 0
	case "cwd":
		_ = os.WriteFile("ran-here", nil, 0o644)
		return 0
	case "big":
		// One long line per iteration, so the total is well over the journal's
		// 64 KiB spill threshold and any clipping is obvious.
		for i := range 4000 {
			fmt.Printf("line %04d: %s\n", i, strings.Repeat("x", 60))
		}
		return 0
	case "argv":
		fmt.Println(strings.Join(args, "|"))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		return 2
	}
}

// helperArgv is the command that runs the helper in mode.
func helperArgv(mode string) []string {
	// argv[0] is a plain name on purpose: it is what reaches the journal, and a
	// machine-specific path there would make the record depend on who wrote it.
	return []string{"project-verify", helperFlag, mode}
}

// helperLookPath resolves any name to this test binary.
func helperLookPath(t *testing.T) func(string) (string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}
	return func(string) (string, error) { return exe, nil }
}

// helperVerifier is a Verifier that runs the helper in mode against a fresh
// temp root.
func helperVerifier(t *testing.T, mode string) *verify.Verifier {
	t.Helper()
	t.Setenv(helperEnv, "1")
	return &verify.Verifier{
		Root:       t.TempDir(),
		Configured: helperArgv(mode),
		LookPath:   helperLookPath(t),
	}
}

// firedClock is a clock whose every timer has already gone off. It makes the
// timeout path deterministic without waiting for one.
type firedClock struct{}

func (firedClock) Now() time.Time { return time.Unix(0, 0).UTC() }

func (firedClock) NewTimer(time.Duration) (<-chan time.Time, func()) {
	ch := make(chan time.Time, 1)
	ch <- time.Unix(0, 0).UTC()
	return ch, func() {}
}

// --- what blocks a success report ---------------------------------------

// TestANonZeroExitFailsAndBlocks is the load-bearing half of KAN-787 at this
// layer: the harness may not report success over a tree the project's own
// command rejected.
func TestANonZeroExitFailsAndBlocks(t *testing.T) {
	res, err := helperVerifier(t, "fail").Run(t.Context())
	if err != nil {
		t.Fatalf("Run() returned an error for a failing command: %v\n"+
			"a command that fails is a result, not an error — an error return invites a caller "+
			"to drop the diagnostic the model needs", err)
	}

	if res.Outcome != verify.Failed {
		t.Errorf("Outcome = %s, want failed", res.Outcome)
	}
	if !res.Blocks() {
		t.Error("Blocks() is false for a command that exited non-zero; this is the one result " +
			"that must forbid a success report (docs/SLICE-1.md §5)")
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Output, "--- FAIL: TestThing") || !strings.Contains(res.Output, "exit status 1") {
		t.Errorf("Output lost the command's diagnostic; both streams must survive whole:\n%s", res.Output)
	}
}

// TestAZeroExitPasses is the positive control. Without it every assertion about
// "did not pass" above could be passing for the wrong reason.
func TestAZeroExitPasses(t *testing.T) {
	res, err := helperVerifier(t, "pass").Run(t.Context())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if res.Outcome != verify.Passed {
		t.Errorf("Outcome = %s, want passed", res.Outcome)
	}
	if res.Blocks() {
		t.Error("Blocks() is true for a command that exited zero")
	}
	if !res.Ran() {
		t.Error("Ran() is false for a command that ran")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "all 12 tests passed") {
		t.Errorf("Output lost the command's own output:\n%s", res.Output)
	}
}

// TestNothingThatDidNotRunReadsAsAPass is the honest-none rule as a property,
// over every way a run can fail to conclude.
//
// The three assertions per case are independent by design: a caller that reads
// Outcome, a caller that reads Ran, and a caller that copies ExitCode into
// journal.VerificationRun without checking either must all be told the same
// thing. -1 rather than 0 is what protects the third one.
func TestNothingThatDidNotRunReadsAsAPass(t *testing.T) {
	cases := []struct {
		name string
		mode string
		skip verify.Skip
		with func(*verify.Verifier)
	}{
		{
			name: "no command configured and none discovered",
			skip: verify.SkipNoCommand,
			with: func(v *verify.Verifier) { v.Configured = nil; v.LookPath = nil },
		},
		{
			name: "the command's binary is not installed",
			mode: "pass",
			skip: verify.SkipToolMissing,
			with: func(v *verify.Verifier) {
				v.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
			},
		},
		{
			name: "the command outlived its timeout",
			mode: "hang",
			skip: verify.SkipTimedOut,
			with: func(v *verify.Verifier) { v.Clock = firedClock{} },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := helperVerifier(t, tc.mode)
			tc.with(v)

			res, err := v.Run(t.Context())
			if err != nil {
				t.Fatalf("Run() = %v; a did-not-run is an answer, not a failure — turning one into "+
					"an error routes an ordinary machine into the harness bucket", err)
			}

			if res.Outcome != verify.NotRun {
				t.Errorf("Outcome = %s, want not_run", res.Outcome)
			}
			if res.Ran() {
				t.Error("Ran() is true for a run that did not conclude")
			}
			if res.Blocks() {
				t.Error("Blocks() is true for a run that did not conclude; a slow machine, a missing " +
					"toolchain and an unrecognised project are not evidence the model's change is wrong")
			}
			if res.ExitCode != -1 {
				t.Errorf("ExitCode = %d, want -1; a caller copying this field into the journal "+
					"without checking Ran must get a value that is visibly not a success", res.ExitCode)
			}
			if res.Skip != tc.skip {
				t.Errorf("Skip = %q, want %q", res.Skip, tc.skip)
			}
			if strings.Contains(res.Output, "passed") {
				t.Errorf("the headline for a run that did not happen contains \"passed\":\n%s", res.Output)
			}
			if !strings.Contains(res.Output, "nothing here is a pass") {
				t.Errorf("the headline does not say in words that this is not a pass:\n%s", res.Output)
			}
		})
	}
}

// TestTheZeroResultIsNotAPass is the structural version of the rule above: a
// Result nobody filled in must report that nothing vouched for the tree.
func TestTheZeroResultIsNotAPass(t *testing.T) {
	var zero verify.Result

	if zero.Outcome != verify.NotRun {
		t.Errorf("the zero Outcome is %s; it must be not_run, because a struct nobody filled in "+
			"is the case most likely to be read as a pass", zero.Outcome)
	}
	if zero.Ran() || zero.Blocks() {
		t.Errorf("the zero Result reports Ran=%v Blocks=%v", zero.Ran(), zero.Blocks())
	}
	if verify.Outcome(200).String() != "not_run" {
		t.Errorf("an unrecognised Outcome renders as %q; every fallback in this package must lean "+
			"away from the string meaning \"passed\"", verify.Outcome(200))
	}
}

// TestTheHonestNoneCaseNamesTheFix is what makes "says so honestly" useful
// rather than merely true. The reader is a model deciding whether it is
// finished, and a message that does not say what would change the answer sends
// it looking in the wrong place.
func TestTheHonestNoneCaseNamesTheFix(t *testing.T) {
	v := &verify.Verifier{Root: t.TempDir()}

	res, err := v.Run(t.Context())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if res.Source != verify.SourceNone {
		t.Errorf("Source = %q, want %q", res.Source, verify.SourceNone)
	}
	if res.Command != nil {
		t.Errorf("Command = %v; nothing was found, so nothing may be recorded as having run", res.Command)
	}
	if !strings.Contains(res.Output, ".kopicode/config.toml") {
		t.Errorf("the message does not name the file that would fix it:\n%s", res.Output)
	}
}

// --- precedence ---------------------------------------------------------

// TestConfiguredBeatsDiscovered is docs/SLICE-1.md §5's ordering: a repository
// that says how it is verified has answered the question, and no amount of
// evidence in the tree overrules it.
func TestConfiguredBeatsDiscovered(t *testing.T) {
	root := tree(t, map[string]string{"go.mod": "module example.com/x\n\ngo 1.26\n"})

	discovered := (&verify.Verifier{Root: root}).Plan()
	if want := []string{"go", "test", "./..."}; !slices.Equal(discovered.Argv, want) {
		t.Fatalf("the fixture is not discoverable as a Go module: %v", discovered.Argv)
	}

	configured := (&verify.Verifier{Root: root, Configured: []string{"make", "ci"}}).Plan()
	if want := []string{"make", "ci"}; !slices.Equal(configured.Argv, want) {
		t.Errorf("Plan() = %v, want %v", configured.Argv, want)
	}
	if configured.Source != verify.SourceConfigured {
		t.Errorf("Source = %q, want %q", configured.Source, verify.SourceConfigured)
	}
}

// TestPlanDoesNotAliasTheConfiguredCommand keeps a caller from editing the
// selection's own slice through a plan it was handed.
func TestPlanDoesNotAliasTheConfiguredCommand(t *testing.T) {
	configured := []string{"make", "test"}
	plan := (&verify.Verifier{Root: t.TempDir(), Configured: configured}).Plan()

	plan.Argv[1] = "rm"
	if configured[1] != "test" {
		t.Errorf("editing the plan changed the configured command to %v; the two must not alias",
			configured)
	}
}

// --- the subprocess -----------------------------------------------------

// TestTheCommandRunsInTheRepositoryRoot is the Dir half of CLAUDE.md's
// subprocess rule, checked by observing where the command actually landed rather
// than by reading the field.
func TestTheCommandRunsInTheRepositoryRoot(t *testing.T) {
	v := helperVerifier(t, "cwd")

	if _, err := v.Run(t.Context()); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(v.Root, "ran-here")); err != nil {
		t.Errorf("the command did not run in %s: %v\n"+
			"a subprocess that inherits its working directory runs wherever the harness "+
			"happened to be", v.Root, err)
	}
}

// TestTheCommandBuildsItsOwnEnvironment is the Env half, and the reason it
// matters here more than anywhere else: this subprocess is the project's whole
// test suite and its output is journaled verbatim.
//
// Each strippable variable is set to a value nothing else would produce, so the
// assertion is "the inherited value did not survive" rather than "the name is
// absent" — a suite is free to set GOFLAGS for itself.
func TestTheCommandBuildsItsOwnEnvironment(t *testing.T) {
	const sentinel = "inherited-from-the-parent"
	mustNotSurvive := []string{
		"OPENROUTER_API_KEY",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_GLOBAL",
		"GOFLAGS", "GOOS", "GOARCH",
		"PYTHONPATH", "NODE_OPTIONS",
	}
	for _, name := range mustNotSurvive {
		t.Setenv(name, sentinel)
	}
	t.Setenv("KOPICODE_TOOLCHAIN_SENTINEL", sentinel)

	res, err := helperVerifier(t, "env").Run(t.Context())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	got := map[string]string{}
	for _, line := range strings.Split(res.Output, "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			got[name] = value
		}
	}

	for _, name := range mustNotSurvive {
		if got[name] == sentinel {
			t.Errorf("the verification command inherited %s=%s\n"+
				"GIT_DIR redirects git at another repository whatever the working directory says, "+
				"and GOFLAGS/GOOS change what the suite even compiles "+
				"(.claude/skills/agent-ground-rules/SKILL.md)", name, sentinel)
		}
	}

	// The negative control. A denylist that had quietly become an allowlist
	// would pass every assertion above and break every real project's suite,
	// which is a gate that reports not_run forever.
	if got["KOPICODE_TOOLCHAIN_SENTINEL"] != sentinel {
		t.Error("a variable that is not on the strip list did not survive; this is a denylist, " +
			"and a suite that cannot find its own toolchain reports not_run on every turn")
	}
}

// TestTheRecordedCommandIsNotThisMachinesPath keeps the journal portable: argv[0]
// is the plain binary name, and the resolved path is what gets executed.
func TestTheRecordedCommandIsNotThisMachinesPath(t *testing.T) {
	res, err := helperVerifier(t, "argv").Run(t.Context())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if got := res.Command[0]; got != "project-verify" {
		t.Errorf("Command[0] = %q, want the plain name; an absolute path here would make the "+
			"journal depend on which machine wrote it", got)
	}
}

// TestOutputIsNeverTruncated is CLAUDE.md's rule, which this package is one of
// the likeliest places to break: a test suite's output is the largest thing the
// loop journals.
func TestOutputIsNeverTruncated(t *testing.T) {
	res, err := helperVerifier(t, "big").Run(t.Context())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	for _, want := range []string{"line 0000:", "line 1999:", "line 3999:"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("Output is missing %q; clipping the diagnostic output that justifies a fix, "+
				"exactly where a reviewer looks, is the specific failure this project is "+
				"designed out of (oversized payloads spill to a blob and stay whole)", want)
		}
	}
	if len(res.Output) < 4000*60 {
		t.Errorf("Output is %d bytes for roughly %d bytes of command output", len(res.Output), 4000*70)
	}
}

// TestCancellationReturnsTheResultAndTheError is KAN-808's shape: the result is
// for the caller and the error is for the classifier, and neither substitutes
// for the other.
//
// The cancellation is driven through the LookPath seam so that the context is
// already done by the time the command is running, with nothing to wait for and
// no sleep anywhere.
func TestCancellationReturnsTheResultAndTheError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	v := helperVerifier(t, "hang")
	resolve := v.LookPath
	v.LookPath = func(name string) (string, error) {
		cancel()
		return resolve(name)
	}

	res, err := v.Run(ctx)
	if err == nil {
		t.Fatal("Run() returned no error for a cancelled verification; a nil error reads as a " +
			"clean stop, which SLICE-1 §9 charges to the model")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Run() error = %v, want it to name the cancellation", err)
	}
	if res.Outcome != verify.NotRun || res.Skip != verify.SkipCancelled {
		t.Errorf("Outcome/Skip = %s/%q, want not_run/cancelled", res.Outcome, res.Skip)
	}
	if res.Blocks() {
		t.Error("Blocks() is true for a cancelled verification; nobody failed, somebody stopped it")
	}
}

// TestCancellationBeforeAnythingStartsRunsNothing covers the other cancellation
// path — one that lands before the command is launched.
func TestCancellationBeforeAnythingStartsRunsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	v := helperVerifier(t, "cwd")
	res, err := v.Run(ctx)
	if err == nil {
		t.Fatal("Run() returned no error for a context that was already done")
	}
	if res.Skip != verify.SkipCancelled {
		t.Errorf("Skip = %q, want cancelled", res.Skip)
	}
	if _, statErr := os.Stat(filepath.Join(v.Root, "ran-here")); statErr == nil {
		t.Error("the command ran despite the context being done before Run was called")
	}
}

// TestTheSameRunTwiceProducesTheSameOutput holds the byte-identical-journal
// criterion (docs/SLICE-1.md §Test Plan) at this package's edge. Wall-clock time
// is the one value guaranteed to differ between two identical runs, so it must
// not be in the string that gets journaled.
func TestTheSameRunTwiceProducesTheSameOutput(t *testing.T) {
	v := helperVerifier(t, "pass")

	first, err := v.Run(t.Context())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	second, err := v.Run(t.Context())
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}

	if first.Output != second.Output {
		t.Errorf("two identical runs produced different output:\n%q\n%q\n"+
			"a replayed session's journal is compared byte for byte, so nothing that varies "+
			"between two runs of the same command may appear here", first.Output, second.Output)
	}
}
