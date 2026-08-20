// Package verify is forced verification: the project's own test command, run
// after any turn that modified files, journaled whole, and refused the right to
// report success when it exits non-zero (docs/SLICE-1.md §5, affordance V1).
//
// # Why the honest-none case is the hard part
//
// The command is either named by the repository (.kopicode/config.toml) or
// discovered from what is lying in the tree. Discovery can fail, and what it
// must never do is fail *quietly*: a harness that silently skipped verification
// on a project it did not recognise would report every session as verified,
// which is the flattering direction and the one that corrupts the measurement.
// So [SourceNone] is a value, [NotRun] is [Outcome]'s zero value rather than
// [Passed], and a [Result] nobody filled in says that nothing vouched for the
// tree. The engine journals that result exactly as it journals a run.
//
// This is the same shape internal/syntax settled on for the post-edit gate, and
// deliberately so: two gates in one loop that disagreed about how "did not run"
// is spelled would eventually be read as though they agreed.
//
// # Discovery never executes anything
//
// [Discover] reads files. It does not run `make -n`, it does not ask `npm` what
// scripts exist, and it does not shell out at all — probing a tree by building
// it turns a discovery step into a side effect, and a side effect that runs
// before the user has consented to anything. Everything it concludes is a
// function of the bytes in the tree, which is also what makes the four cases in
// this card's acceptance criteria testable from a temp directory.
//
// # What blocks a success report, and what does not
//
// Only [Failed] — a verification command that ran and exited non-zero. A
// [NotRun] result does not block, for the reason internal/syntax gives about
// timeouts: a slow machine, a missing toolchain or a project kopicode does not
// recognise is not evidence that the model's change is wrong, and charging it as
// one would make the loop unable to finish a task on any project without a
// Makefile. The trade is stated rather than hidden: a timed-out verification
// leaves the harness with no evidence either way, and the record says so.
//
// # What this package does not do
//
// It does not import internal/journal — packages return data and the engine
// journals it — and [Result] is shaped so a journal.VerificationRun can be
// filled from it without inventing a field. It does not decide *when* to run
// (the engine does), and it does not read .kopicode/config.toml (internal/harness
// does, and hands the command over as [Verifier.Configured]).
package verify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/leejianrong/kopicode/internal/procgroup"
)

// DefaultTimeout bounds one verification run.
//
// It is much longer than the post-edit syntax gate's 30s and longer than
// run_shell's 120s, because the subject is a project's whole test suite and not
// one file: a suite that takes three minutes is ordinary, and a bound that cut
// it off would report [SkipTimedOut] on every turn of a perfectly healthy
// project. It is still a bound, because verification runs unattended in a bench
// arm and a hung suite there is a machine that stops making progress.
const DefaultTimeout = 10 * time.Minute

// DefaultGrace is how long a signalled process group has to exit before it is
// killed outright. A test runner given the chance to exit removes its temporary
// files and reaps its own children.
const DefaultGrace = 5 * time.Second

// Source says where the verification command came from.
//
// The three values are journal.VerificationRun.Source's vocabulary. [SourceNone]
// is the honest no-op this card asks for by name, and it is a value here rather
// than an empty string so that a caller cannot get it by forgetting to set the
// field.
type Source string

const (
	// SourceNone means no command was configured and none was discovered.
	SourceNone Source = "none"
	// SourceConfigured means .kopicode/config.toml named the command.
	SourceConfigured Source = "configured"
	// SourceDiscovered means it was found in the tree by [Discover].
	SourceDiscovered Source = "discovered"
)

// Rule names the signal that produced a discovered command, so the record says
// *why* this command and not another one. It is "" for a configured command and
// for none at all.
type Rule string

const (
	// RuleNone means nothing matched.
	RuleNone Rule = ""
	// RuleMakefileTarget means a makefile in the root declares one of
	// [MakefileTargets].
	RuleMakefileTarget Rule = "makefile_target"
	// RuleGoModule means go.mod sits in the root.
	RuleGoModule Rule = "go_module"
	// RuleNodeTestScript means package.json declares a non-empty scripts.test.
	RuleNodeTestScript Rule = "node_test_script"
	// RuleUvProject means pyproject.toml sits in the root beside a uv.lock or a
	// [tool.uv] table.
	RuleUvProject Rule = "uv_project"
)

// Outcome is what verification concluded about the tree.
//
// Three values and not a bool, because a bool can only say "ok" or "not ok" and
// the whole point of this package is the third answer. The zero value is
// [NotRun]: a [Result] nobody filled in reports that nothing vouched for the
// tree, which is the safe direction to be wrong in.
type Outcome uint8

const (
	// NotRun means no command executed: none was configured or discovered, the
	// binary is not installed, or the run was stopped before it concluded.
	// [Result.Skip] says which. It is emphatically **not** a pass.
	NotRun Outcome = iota

	// Passed means the command ran and exited zero.
	Passed

	// Failed means the command ran and exited non-zero. This is the one outcome
	// that blocks a success report (docs/SLICE-1.md §5).
	Failed
)

var outcomeText = map[Outcome]string{
	NotRun: "not_run",
	Passed: "passed",
	Failed: "failed",
}

// String returns the wire form.
//
// An unrecognised value renders as "not_run". Every fallback in this package
// leans the same way: the only string that must never appear by accident is the
// one meaning "passed".
func (o Outcome) String() string {
	if s, ok := outcomeText[o]; ok {
		return s
	}
	return outcomeText[NotRun]
}

// Skip says why verification did not reach a verdict.
//
// The values are kept apart because each implies a different response, and a
// caller that saw one string could not tell a project kopicode does not
// recognise from a machine that is missing the toolchain the project named.
type Skip string

const (
	// SkipNone means nothing was skipped.
	SkipNone Skip = ""

	// SkipNoCommand means no command was configured and discovery found none.
	// This is the honest no-op the card asks for.
	SkipNoCommand Skip = "no_command"

	// SkipToolMissing means the command's binary is not installed on this
	// machine. Deliberately distinct from [SkipCommandBroken]: absent is an
	// ordinary machine, broken is a machine somebody should look at.
	SkipToolMissing Skip = "tool_missing"

	// SkipCommandBroken means the binary was found and could not be started, or
	// was ended by a signal. Neither is evidence about the model's change.
	SkipCommandBroken Skip = "command_broken"

	// SkipTimedOut means the run outlived [Verifier.Timeout] and its process
	// group was killed. A slow machine is not a broken change, so this is a
	// did-not-run and never a failure.
	SkipTimedOut Skip = "timed_out"

	// SkipCancelled means the context ended while the command was running.
	// Nobody failed; whoever started the turn decided to stop it.
	SkipCancelled Skip = "cancelled"
)

// Plan is the command verification will run, and where it came from.
//
// A plan is decided without executing anything, which is what lets a caller ask
// "what would you run here?" — the question the card's acceptance criteria are
// written in — without consenting to a subprocess.
type Plan struct {
	// Argv is the command, argv-style. It is nil exactly when Source is
	// [SourceNone]. It is argv and never a shell string because
	// journal.VerificationRun records argv: a shell string would have to be
	// re-split on replay, by a splitter that is not the shell that ran it.
	Argv []string
	// Source is where the command came from.
	Source Source
	// Rule names the signal that matched, for a discovered command.
	Rule Rule
	// Detail is the tree evidence the rule matched on — the makefile's name and
	// target, the file that was found. It is "" for a configured command and for
	// none, and it is diagnostics rather than a decision.
	Detail string
}

// Found reports whether there is a command to run.
func (p Plan) Found() bool { return len(p.Argv) > 0 }

// String renders the command for a message.
func (p Plan) String() string {
	if !p.Found() {
		return "(none)"
	}
	return strings.Join(p.Argv, " ")
}

// Result is what one verification produced.
//
// It carries everything journal.VerificationRun needs and nothing it has to
// invent: [Result.Command] maps to Command, [Result.Source] to Source,
// [Result.ExitCode] to ExitCode and [Result.Output] to Output. The engine
// journals it; this package does not import the journal.
type Result struct {
	// Command is the argv that ran, or the argv that would have run when
	// something stopped it. Nil when no command was found at all.
	Command []string
	// Source is where the command came from, [SourceNone] when there was none.
	Source Source
	// Rule and Detail carry [Plan]'s provenance through to the caller.
	Rule   Rule
	Detail string

	// Outcome is the verdict. Its zero value is [NotRun].
	Outcome Outcome
	// Skip says why nothing vouched for the tree, when Outcome is [NotRun].
	Skip Skip

	// ExitCode is the command's status. It is **-1**, never 0, whenever Outcome
	// is [NotRun] — a caller that copies this field into
	// journal.VerificationRun.ExitCode without checking [Result.Ran] first gets a
	// value that is visibly not a success.
	ExitCode int
	// Signal names the signal that ended the process as the OS reported it, ""
	// when none did.
	Signal string
	// StoppedBy names what this package sent to end the run, "" when it sent
	// nothing.
	StoppedBy string

	// Output is the model-facing rendering: a headline, then the command's
	// complete combined output. Nothing is truncated — CLAUDE.md's rule is that
	// clipping the diagnostic output which justifies a fix is the specific
	// failure being designed out — and oversized payloads are the journal's blob
	// spill to handle.
	//
	// The absolute repository root is rewritten to its repo-relative form so that
	// the same run reads the same on another machine. No duration appears here:
	// this string is journaled and then compared byte for byte when a session is
	// replayed (docs/SLICE-1.md §Test Plan), and wall-clock time is the one value
	// guaranteed to differ between two identical runs. It stays on
	// [Result.Duration], where the engine can reach it and the comparison cannot.
	Output string

	// Duration is wall-clock time from start to reap, off the injected clock.
	Duration time.Duration
}

// Ran reports whether a command actually executed and concluded.
func (r Result) Ran() bool { return r.Outcome != NotRun }

// Blocks reports whether this result forbids reporting success.
//
// Exactly one outcome does: a command that ran and exited non-zero. See the
// package doc on why a [NotRun] does not.
func (r Result) Blocks() bool { return r.Outcome == Failed }

// Summary is [Result.Output]'s headline alone, with no raw stream behind it —
// the one-line form the engine hands the model when a run passes (KAN-960).
// A pass has nothing further worth spending tokens on every time it recurs,
// unlike a failure, where the stream is the fix's own evidence.
func (r Result) Summary() string { return "verification: " + headline(r) }

// Verifier runs forced verification for one repository.
//
// The engine builds one per session and calls [Verifier.Run] after each turn
// that could have changed the tree. It holds no state between calls.
type Verifier struct {
	// Root is the absolute path to the repository root. The command runs with
	// its working directory there.
	Root string

	// Configured is the command .kopicode/config.toml named, nil when the file
	// named none. A non-nil value beats discovery outright: a repository that
	// says how it is verified has answered the question.
	//
	// It is supplied rather than read here because internal/harness owns that
	// file and its precedence chain (ADR-0007 decision 2), and a second reader
	// would be a second answer.
	Configured []string

	// Timeout bounds one run. Zero means [DefaultTimeout].
	Timeout time.Duration
	// Grace is how long a signalled process group has to exit. Zero means
	// [DefaultGrace].
	Grace time.Duration

	// Clock is where the timeout comes from. Nil means the real one.
	Clock Clock

	// LookPath resolves the command's binary. Nil means exec.LookPath.
	//
	// It is injected for the reason internal/syntax injects it: the machine that
	// runs the tests has make, go and npm installed, so "uv is not installed"
	// would otherwise never be exercised — and that is precisely the path that
	// must not report a pass.
	LookPath func(name string) (string, error)
}

// Clock is where a timeout comes from.
//
// It is injected because a test that waits out the real timeout is either a slow
// suite or a short timeout that proves nothing about the one shipping. It also
// satisfies procgroup.Clock, so the grace period is measured by the same clock
// as the timeout rather than by a second source that could disagree with it.
type Clock interface {
	// Now is the current time. Only differences are ever used.
	Now() time.Time
	// NewTimer returns a channel that receives once d has elapsed, and a
	// function that releases the timer. The caller always calls it.
	NewTimer(d time.Duration) (<-chan time.Time, func())
}

// RealClock is the production clock.
type RealClock struct{}

// Now returns the wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// NewTimer wraps time.NewTimer.
func (RealClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

func (v *Verifier) clock() Clock {
	if v.Clock == nil {
		return RealClock{}
	}
	return v.Clock
}

func (v *Verifier) timeout() time.Duration {
	if v.Timeout <= 0 {
		return DefaultTimeout
	}
	return v.Timeout
}

func (v *Verifier) grace() time.Duration {
	if v.Grace <= 0 {
		return DefaultGrace
	}
	return v.Grace
}

func (v *Verifier) lookPath(name string) (string, error) {
	if v.LookPath != nil {
		return v.LookPath(name)
	}
	return exec.LookPath(name)
}

// Plan reports the command this repository will be verified with, without
// running anything.
//
// Configured beats discovered, which is docs/SLICE-1.md §5's ordering and
// ADR-0007's generally: the repository's own statement about itself outranks
// anything inferred from the files lying in it.
func (v *Verifier) Plan() Plan {
	if len(v.Configured) > 0 {
		return Plan{Argv: slices.Clone(v.Configured), Source: SourceConfigured}
	}
	return Discover(v.Root)
}

// Run verifies the tree and reports what happened.
//
// **A command that fails is not an error.** A non-zero exit comes back as a
// [Result] with [Failed] and a nil error, because the suite's output is exactly
// what the model needs next and an error return invites a caller to drop it. The
// error return is reserved for a cancellation, and even then the partial result
// comes back alongside it — following KAN-808: the result is for the caller, the
// error is for the classifier, and neither substitutes for the other.
//
// Everything else — no command found, the binary missing, a timeout — is a
// [NotRun] with a [Skip] saying which, and nil. Those are answers, not failures,
// and turning them into errors would route an ordinary machine without `uv` into
// ADR-0006 §3's harness bucket.
func (v *Verifier) Run(ctx context.Context) (Result, error) {
	start := v.clock().Now()
	plan := v.Plan()

	res := Result{
		Command:  plan.Argv,
		Source:   plan.Source,
		Rule:     plan.Rule,
		Detail:   plan.Detail,
		ExitCode: -1,
	}

	if !plan.Found() {
		res.Skip = SkipNoCommand
		res.Duration = v.clock().Now().Sub(start)
		res.Output = v.render(res, "")
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		res.Skip = SkipCancelled
		res.Duration = v.clock().Now().Sub(start)
		res.Output = v.render(res, "")
		return res, fmt.Errorf("verify: %s: cancelled before starting: %w", plan, err)
	}

	resolved, lookErr := v.lookPath(plan.Argv[0])
	if lookErr != nil {
		// The lookup error is deliberately dropped rather than returned. A
		// missing binary is an answer — this machine does not have what the
		// project named — and returning it would route an ordinary machine into
		// ADR-0006 §3's harness bucket. What it means is on the record as
		// [SkipToolMissing], which is more than the wrapped exec.ErrNotFound
		// would say.
		res.Skip = SkipToolMissing
		res.Duration = v.clock().Now().Sub(start)
		res.Output = v.render(res, "")
		return res, nil //nolint:nilerr // deliberate: a missing binary is a result, not a failure
	}

	raw, cancelled := v.exec(ctx, &res, resolved, start)
	res.Output = v.render(res, raw)
	if cancelled != nil {
		return res, fmt.Errorf("verify: %s: %w", plan, cancelled)
	}
	return res, nil
}

// exec runs the resolved command, fills in everything about how it went, and
// returns its captured output. The error is non-nil only for a cancellation.
//
// It is the one place in this package that starts a subprocess, which is what
// TestOnlyOneSubprocessCallSite holds it to: the Dir and Env decisions below are
// the whole of the package's answer, and a second call site would be a second
// answer that nobody re-reads.
func (v *Verifier) exec(ctx context.Context, res *Result, resolved string, start time.Time) (string, error) {
	var out bytes.Buffer

	// argv[0] is the plain binary name and cmd.Path the resolved one: the
	// process is looked up exactly once, through the injectable seam, and the
	// machine-specific path never reaches Command and so never reaches the
	// journal.
	cmd := exec.Command(resolved, res.Command[1:]...) //nolint:gosec // the argv is configured or discovered here, never written by the model
	cmd.Args[0] = res.Command[0]
	cmd.Dir = v.Root
	cmd.Env = v.baseEnv()
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = v.grace()
	procgroup.Isolate(cmd)

	clock := v.clock()
	if err := cmd.Start(); err != nil {
		res.Skip = SkipCommandBroken
		res.Duration = clock.Now().Sub(start)
		// Normalised like any captured stream: the error names the absolute path
		// the binary was resolved to, and this string is journaled.
		res.Detail = v.normalise(fmt.Sprintf("%s could not be started: %v", res.Command[0], err))
		return "", nil
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	timer, stop := clock.NewTimer(v.timeout())
	defer stop()

	var cancelled error
	select {
	case <-waited:
	case <-timer:
		res.Skip = SkipTimedOut
		res.StoppedBy, _ = procgroup.Stop(cmd, waited, clock, v.grace())
	case <-ctx.Done():
		res.Skip = SkipCancelled
		res.StoppedBy, _ = procgroup.Stop(cmd, waited, clock, v.grace())
		cancelled = ctx.Err()
	}

	// cmd.Wait has returned on every path above, which is what makes reading the
	// buffer here safe: os/exec's copying goroutines are joined by Wait, so
	// touching it earlier would be a data race rather than merely a partial read.
	res.Duration = clock.Now().Sub(start)
	raw := v.normalise(out.String())

	if res.Skip != SkipNone {
		return raw, cancelled
	}
	if ps := cmd.ProcessState; ps != nil {
		res.ExitCode, res.Signal = procgroup.ExitStatus(ps)
	}
	// A suite killed by a signal produced no verdict either. Reading its -1 as
	// "non-zero, therefore the change is broken" would charge an OOM kill to the
	// model.
	switch {
	case res.Signal != "":
		res.Skip = SkipCommandBroken
		res.ExitCode = -1
	case res.ExitCode == 0:
		res.Outcome = Passed
	default:
		res.Outcome = Failed
	}
	return raw, nil
}

// normalise rewrites the absolute repository root out of captured output.
//
// This is the only edit made to the command's output anywhere in this package,
// and it drops nothing: an absolute path becomes the repo-relative path naming
// the same file, which is both what the model thinks in and what makes two
// machines agree. The evaluated form is replaced as well as the literal one
// because a root under a symlinked temp directory — /tmp on macOS — comes back
// from a subprocess resolved.
func (v *Verifier) normalise(s string) string {
	if s == "" || v.Root == "" {
		return s
	}
	roots := []string{v.Root}
	if eval, err := filepath.EvalSymlinks(v.Root); err == nil && eval != v.Root {
		roots = append(roots, eval)
	}
	for _, root := range roots {
		s = strings.ReplaceAll(s, root+string(filepath.Separator), "")
		s = strings.ReplaceAll(s, filepath.ToSlash(root)+"/", "")
		s = strings.ReplaceAll(s, root, ".")
	}
	return s
}

// render is the model-facing form.
//
// The headline never says "ok" for a verification that did not run. It says what
// did not happen and why, and for the honest no-op it says in words that this is
// not a pass — because the reader that most needs telling is a model deciding
// whether it is finished.
func (v *Verifier) render(r Result, raw string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "verification: %s\n", headline(r))
	if r.Detail != "" {
		fmt.Fprintf(&b, "note: %s\n", r.Detail)
	}
	writeStream(&b, raw)
	return b.String()
}

func headline(r Result) string {
	switch r.Outcome {
	case Passed:
		return fmt.Sprintf("`%s` (%s) passed", commandText(r), r.Source)
	case Failed:
		return fmt.Sprintf("`%s` (%s) FAILED, exit %d — this change is not finished",
			commandText(r), r.Source, r.ExitCode)
	default:
		return "nothing ran, so nothing here is a pass — " + skipReason(r)
	}
}

// skipReason is the sentence a model reads when nothing vouched for the tree.
// Each one names what would make the answer different.
func skipReason(r Result) string {
	switch r.Skip {
	case SkipNoCommand:
		return "kopicode found no verification command for this project, and none is " +
			"configured in " + configHint
	case SkipToolMissing:
		return fmt.Sprintf("`%s` is not installed on this machine, so `%s` could not run",
			r.Command[0], commandText(r))
	case SkipCommandBroken:
		return fmt.Sprintf("`%s` was found but could not be run to completion", commandText(r))
	case SkipTimedOut:
		return fmt.Sprintf("`%s` outlived its timeout and its process group was killed",
			commandText(r))
	case SkipCancelled:
		return "the verification was cancelled"
	default:
		return "no reason was recorded, which is itself a bug"
	}
}

// configHint names the file a user edits to fix the honest-none case. It is a
// string rather than an import of internal/harness: this package is a leaf and
// the two agree on a path, not on a dependency edge.
const configHint = ".kopicode/config.toml (verify = [\"make\", \"test\"])"

func commandText(r Result) string { return strings.Join(r.Command, " ") }

// writeStream writes captured output whole, terminated by exactly one newline.
// Nothing is dropped: output that did not end in a newline gains one, and that
// is the only other edit made to it in this package.
func writeStream(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
	}
}
