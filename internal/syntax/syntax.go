// Package syntax is the post-edit syntax gate: a language-native check run
// immediately after an edit, selected by file extension and shelled out to
// (ADR-0006 §4 and §5, SLICE-1 §6, affordance V2).
//
// # Why it exists, and why its honesty matters more than its coverage
//
// SLICE-1 §9 charges a gate failure that follows an EditApplied straight to the
// `harness` bucket, on the reasoning that a correct edit into the intended
// region almost never produces code that does not parse. That makes the gate a
// measurement instrument as much as a product feature, and it puts the whole
// weight of the design on one distinction:
//
//	a gate that did not run must never be readable as a gate that passed.
//
// Get that wrong and every edit to a `.js` file on a machine without node
// reads as a clean pass, or — worse, and in the flattering direction — every
// unsupported language silently vouches for itself. So [Outcome]'s zero value
// is [NotRun], not [Passed]; there is no bool anywhere in [Result] that could
// be mistaken for "ok"; and [Result.ExitCode] is -1 rather than 0 whenever
// nothing ran, so a caller that copies the field without looking gets a value
// that is obviously not a success. [Result.Skip] then says *why* it did not
// run, because "no checker for .md" and "node is not installed" call for
// completely different responses.
//
// # Verdict steps and advisory steps
//
// A language's checker is a sequence of steps, and they are not all the same
// kind of evidence:
//
//   - A **verdict step** has the edited file, alone, as its subject. Its exit
//     status is the gate's verdict. `gofmt -e`, `python -m py_compile` and
//     `node --check` are verdict steps: each parses one file and needs no
//     package context, so a failure is about the edit and nothing else.
//   - An **advisory step** has a whole package or project as its subject.
//     `go build` is the example. It runs, its full output is returned to the
//     model, and its exit status is recorded in [Result.Steps] — but it does
//     not set [Result.Outcome].
//
// The reason for that split is the acceptance criterion. SLICE-1's slice-1 bar
// is *zero* failures classified `harness`, and a correct edit can legitimately
// leave a package failing to typecheck: the model edits a caller this turn and
// writes the callee next. Charging that to the edit fills the harness bucket
// with mid-refactor noise and breaks the measurement the project exists to
// make. Parsing has no such excuse, which is exactly the property §9's rule was
// argued from.
//
// Where the toolchain offers a file-scoped parse check, that is the verdict and
// the project-scoped check is advisory. Where it does not — TypeScript ships no
// single-file syntax checker, only `tsc` over a project — the project-scoped
// check becomes the verdict, because the alternative is no coverage at all for
// a language SLICE-1 lists as covered. That is stated rather than hidden:
// [Step.Verdict] records which kind each step was, so a later reading of the
// numbers can tell them apart.
//
// # Subprocesses
//
// Every checker is shelled out to, never linked (CLAUDE.md, ADR-0006 §5). Each
// one runs in a process group of its own via internal/procgroup, under a
// timeout, and is killed as a group on cancellation — a checker that hangs
// after every edit would hang the agent loop.
//
// Every subprocess sets both Dir and Env deliberately.
// TestEveryStepNamesItsDirectoryAndBuildsItsEnvironment checks the values, and
// TestOnlyOneSubprocessCallSite checks there is nowhere else to build a command
// from. internal/arch's static guard covers git commands only; these are not
// git, which is a reason to be more careful rather than less. An inherited
// GOFLAGS, GOOS, PYTHONPATH or NODE_OPTIONS changes what the checker concludes,
// so [Gate] strips them — see env.go for the list and the reasoning.
//
// # Output
//
// Nothing is truncated, ever (CLAUDE.md). The one edit made to any captured
// stream is that the absolute repository root is rewritten to its repo-relative
// form, so that a journal recorded on one machine reads the same on another;
// nothing is dropped by it. Checkers are invoked with Dir set and a *relative*
// path argument for the same reason, which is why gofmt and py_compile emit no
// absolute paths at all. The residue is named honestly in [Result.Output]'s
// documentation rather than papered over.
//
// # What this package does not do
//
// It does not import internal/journal — packages return data and the engine
// journals it — and [Result] is shaped so that a journal.SyntaxGateRun can be
// filled from it without inventing a field. It does not decide *when* to run
// (the engine does), and it does not implement the three-bucket classifier
// (KAN-797); it only makes sure the classifier is given something true.
package syntax

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/leejianrong/kopicode/internal/procgroup"
)

// DefaultTimeout bounds one checker step.
//
// It is much shorter than run_shell's 120s on purpose: the gate runs after
// *every* edit, so its cost is paid on every turn, and a parse check that takes
// thirty seconds has already failed at being a fast feedback signal. The bound
// that matters is the advisory `go build` on a cold build cache; a verdict step
// that reaches it is a broken toolchain, and either way a timeout is recorded
// as [SkipTimedOut] and never as a failure.
const DefaultTimeout = 30 * time.Second

// DefaultGrace is how long a signalled process group has to exit before it is
// killed outright.
const DefaultGrace = 3 * time.Second

// Outcome is what the gate concluded about a file.
//
// Three values and not a bool, because a bool can only say "ok" or "not ok" and
// the whole point of this package is the third answer. The zero value is
// [NotRun]: a [Result] nobody filled in reports that nothing vouched for the
// file, which is the safe direction to be wrong in.
type Outcome uint8

const (
	// NotRun means no verdict step executed: no checker is registered for the
	// extension, the checker is not installed, the project file it needs is
	// absent, or it was stopped before it could conclude. [Result.Skip] says
	// which. It is emphatically **not** a pass.
	NotRun Outcome = iota

	// Passed means a verdict step ran and exited zero.
	Passed

	// Failed means a verdict step ran and exited non-zero. This is the signal
	// SLICE-1 §9 charges to `harness` when it follows an EditApplied.
	Failed
)

var outcomeText = map[Outcome]string{
	NotRun: "not_run",
	Passed: "passed",
	Failed: "failed",
}

// String returns the wire form.
//
// An unrecognised value renders as "not_run" rather than as "unknown" or as a
// number. Every other fallback in this package leans the same way: the only
// string that must never appear by accident is the one meaning "passed".
func (o Outcome) String() string {
	if s, ok := outcomeText[o]; ok {
		return s
	}
	return outcomeText[NotRun]
}

// Skip says why a step, or the gate as a whole, did not reach a verdict.
//
// The values are kept apart because each implies a different response, and a
// caller that saw one string could not tell a language kopicode does not check
// from a machine that is missing a toolchain — the first is permanent and
// expected, the second is worth telling somebody about.
type Skip string

const (
	// SkipNone means nothing was skipped.
	SkipNone Skip = ""

	// SkipNoChecker means no checker is registered for this file's extension.
	// This is the honest no-op the card asks for, and it is the common case:
	// Markdown, TOML, JSON, and every language kopicode does not check.
	SkipNoChecker Skip = "no_checker"

	// SkipToolMissing means a checker is registered for this language but its
	// binary is not installed. It is deliberately distinct from
	// [SkipCheckerBroken]: absent is an ordinary machine, broken is a machine
	// somebody should look at.
	SkipToolMissing Skip = "tool_missing"

	// SkipCheckerBroken means the binary was found and could not be started —
	// present but not executable, or a bad interpreter line. It is the third
	// answer to "is node installed", and it must not read as a syntax failure:
	// a broken toolchain is not evidence about an edit.
	SkipCheckerBroken Skip = "checker_broken"

	// SkipNoProject means the checker needs a project file that is not there:
	// no tsconfig.json above a .ts file, no go.mod above a .go file. The card
	// is explicit that tsc runs only "where a tsconfig exists", so its absence
	// is recorded rather than assumed away.
	SkipNoProject Skip = "no_project"

	// SkipNotAFile means the path is not a regular file — deleted between the
	// edit and the check, or a directory. Nothing can vouch for it.
	SkipNotAFile Skip = "not_a_file"

	// SkipTimedOut means the step outlived [Gate.Timeout] and its process group
	// was killed. A slow machine is not a broken edit, so this is a
	// did-not-run and never a failure.
	SkipTimedOut Skip = "timed_out"

	// SkipCancelled means the context ended while the step was running. Nobody
	// failed; whoever started the call decided to stop it.
	SkipCancelled Skip = "cancelled"

	// SkipNotReached means the step never started because an earlier step had
	// already settled the verdict — there is nothing to learn from building a
	// package whose source does not parse.
	SkipNotReached Skip = "not_reached"
)

// Step is one checker invocation.
type Step struct {
	// Argv is the command as it was run, with argv[0] the plain binary name
	// rather than its resolved absolute path. The resolved path is what gets
	// executed and the plain name is what gets recorded, because an absolute
	// path would make the journal depend on which machine wrote it.
	Argv []string
	// Checker is Argv joined by spaces: the model-facing name of this step,
	// and what journal.SyntaxGateRun.Checker records when this step decided
	// the verdict.
	Checker string
	// Dir is the working directory the step ran in, as a slash path relative
	// to [Gate.Root]. "." is the root itself.
	Dir string

	// Verdict reports whether this step's exit status set [Result.Outcome].
	// See the package doc: a step whose subject is the edited file alone is a
	// verdict step, one whose subject is a whole package or project is not.
	Verdict bool

	// Outcome is what this step concluded.
	Outcome Outcome
	// Skip says why, when Outcome is [NotRun].
	Skip Skip
	// ExitCode is the process's status, or -1 when it never produced one.
	ExitCode int
	// Signal names the signal that ended the process, "" when none did.
	Signal string
	// StoppedBy names what this package sent to end the run, "" when it sent
	// nothing.
	StoppedBy string

	// Output is everything the step wrote, whole and unclipped.
	Output string
	// Duration is wall-clock time from start to reap, off the injected clock.
	Duration time.Duration
}

// Result is what the gate concluded about one file.
//
// It carries everything journal.SyntaxGateRun needs and nothing it has to
// invent: Path maps to Path, [Result.Ran] to Ran, [Result.Checker] to Checker,
// [Result.ExitCode] to ExitCode and [Result.Output] to Output. The engine
// journals it; this package does not import the journal.
//
// KAN-797's classifier wants exactly one predicate out of this —
// `Outcome == Failed` on a gate run that followed an EditApplied — and the
// three-way [Outcome] is what makes that predicate safe to write. There is no
// success bool here to get it wrong with.
type Result struct {
	// Path is the file, as the slash path relative to [Gate.Root] that was
	// asked about.
	Path string
	// Language is the language selected from the extension, [LangUnknown] when
	// the extension is not registered.
	Language Language

	// Outcome is the verdict. Its zero value is [NotRun].
	Outcome Outcome
	// Skip says why nothing vouched for the file, when Outcome is [NotRun].
	Skip Skip

	// Checker is the command that decided the verdict, "" when none did. It is
	// "" exactly when Outcome is [NotRun], which is a second, independent way
	// the two cases stay distinguishable.
	Checker string
	// ExitCode is the deciding step's status. It is **-1**, never 0, whenever
	// Outcome is [NotRun] — a caller that copies this field into
	// journal.SyntaxGateRun.ExitCode without checking Ran first gets a value
	// that is visibly not a success.
	ExitCode int

	// Output is the model-facing rendering: a headline, then every step that
	// ran with its own header and its complete output. Nothing is truncated;
	// oversized payloads are the journal's blob spill to handle.
	//
	// The absolute repository root is rewritten to its repo-relative form so
	// that the same check on the same file reads identically on another
	// machine. What cannot be normalised without dropping content is stated
	// rather than removed: `node --check` prints the absolute path it resolved
	// and a "Node.js vX.Y.Z" trailer, and the trailer varies with the
	// installed runtime. SLICE-1's byte-identical-journal criterion is about
	// one loop replayed against fixed provider output, which holds; identical
	// output across two machines with different toolchains was never on offer
	// from a gate that shells out.
	Output string

	// Steps is every step in the plan, in order, including those that did not
	// run. It is what keeps the verdict honest: an advisory `go build` failure
	// is recorded here with its real exit code even though Outcome stays
	// [Passed].
	Steps []Step

	// Duration is wall-clock time across the whole gate.
	Duration time.Duration
}

// Ran reports whether a verdict step actually executed. It is
// journal.SyntaxGateRun.Ran, and the reason that field exists.
func (r Result) Ran() bool { return r.Outcome != NotRun }

// Gate runs the post-edit syntax check for one repository.
//
// The engine builds one per session and calls [Gate.Check] after each edit. It
// holds no state between calls and is safe for concurrent use.
type Gate struct {
	// Root is the absolute path to the repository root. Every checker runs
	// with its working directory at or below it, and every path argument is
	// relative, so no absolute path reaches a diagnostic.
	Root string

	// Timeout bounds one step. Zero means [DefaultTimeout].
	Timeout time.Duration
	// Grace is how long a signalled process group has to exit. Zero means
	// [DefaultGrace].
	Grace time.Duration

	// Clock is where the timeout comes from. Nil means the real one.
	Clock Clock

	// LookPath resolves a checker's binary. Nil means exec.LookPath.
	//
	// It is injected because availability detection is the part of this
	// package most likely to be wrong and least likely to be exercised: the
	// machine that runs the tests has go and node installed, so "node is
	// absent" would otherwise never be tested at all — and that is precisely
	// the path that must not report a pass.
	LookPath func(name string) (string, error)
}

// Clock is where a timeout comes from.
//
// It is injected for the reason internal/tools gives: a test that waits out the
// real timeout is either a slow suite or a short timeout that proves nothing
// about the one shipping. It also satisfies procgroup.Clock, so the grace
// period is measured by the same clock as the timeout rather than by a second
// source that could disagree with it.
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

func (g *Gate) clock() Clock {
	if g.Clock == nil {
		return RealClock{}
	}
	return g.Clock
}

func (g *Gate) timeout() time.Duration {
	if g.Timeout <= 0 {
		return DefaultTimeout
	}
	return g.Timeout
}

func (g *Gate) grace() time.Duration {
	if g.Grace <= 0 {
		return DefaultGrace
	}
	return g.Grace
}

// lookPath resolves name to an executable, or reports that it is not installed.
func (g *Gate) lookPath(name string) (string, error) {
	if g.LookPath != nil {
		return g.LookPath(name)
	}
	return exec.LookPath(name)
}

// ErrBadPath reports a path argument that is not a repository-relative path:
// absolute, or escaping the root with "..". It is the one thing [Gate.Check]
// treats as the caller's bug rather than as something to report honestly about.
var ErrBadPath = errors.New("path is not relative to the repository root")

// Check runs the gate for one file and reports what, if anything, vouched for
// it.
//
// path is a slash path relative to [Gate.Root] — the same form
// journal.SyntaxGateRun records and the same form the tools use.
//
// **A checker that fails is not an error.** A non-zero exit is a [Result] with
// [Failed], returned with a nil error, because the checker's diagnostic is
// exactly what the model needs next and an error return invites a caller to
// drop it. The error return is reserved for a path that was never valid
// ([ErrBadPath]) and for a cancellation — and a cancellation returns the
// partial result alongside the error, following KAN-808: the result is for the
// caller, the error is for the classifier, and neither substitutes for the
// other.
//
// Everything else — a missing checker, a deleted file, a broken binary, a
// timeout — comes back as [NotRun] with a [Skip] saying which, and nil. Those
// are answers, not failures, and turning them into errors would route an
// ordinary machine without node into the `harness` bucket.
func (g *Gate) Check(ctx context.Context, path string) (Result, error) {
	start := g.clock().Now()

	res := Result{Path: path, ExitCode: -1}
	if err := ctx.Err(); err != nil {
		res.Skip = SkipCancelled
		res.Output = g.render(res)
		return res, fmt.Errorf("syntax gate on %q: cancelled before starting: %w", path, err)
	}

	rel, err := localPath(path)
	if err != nil {
		return res, fmt.Errorf("syntax gate on %q: %w", path, err)
	}
	abs := filepath.Join(g.Root, rel)

	info, err := os.Stat(abs)
	switch {
	case err != nil, !info.Mode().IsRegular():
		res.Skip = SkipNotAFile
		res.Duration = g.clock().Now().Sub(start)
		res.Output = g.render(res)
		return res, nil
	}

	res.Language = LanguageOf(rel)
	if res.Language == LangUnknown {
		res.Skip = SkipNoChecker
		res.Duration = g.clock().Now().Sub(start)
		res.Output = g.render(res)
		return res, nil
	}

	specs, skip := g.plan(res.Language, rel)
	defer func() {
		for _, s := range specs {
			if s.cleanup != nil {
				s.cleanup()
			}
		}
	}()
	if skip != SkipNone {
		res.Skip = skip
		res.Duration = g.clock().Now().Sub(start)
		res.Output = g.render(res)
		return res, nil
	}

	var cancelled error
	settled := false
	for _, sp := range specs {
		if settled || cancelled != nil {
			res.Steps = append(res.Steps, notReached(sp))
			continue
		}
		if sp.skip != SkipNone {
			// Only an advisory step reaches here: a verdict step that cannot
			// run is a gate-level skip decided in plan, because the gate then
			// has nothing at all to say about the file.
			unrunnable := notReached(sp)
			unrunnable.Skip = sp.skip
			res.Steps = append(res.Steps, unrunnable)
			continue
		}

		step, cerr := g.runStep(ctx, sp)
		res.Steps = append(res.Steps, step)
		if cerr != nil {
			cancelled = cerr
			continue
		}
		if !sp.verdict {
			continue
		}

		res.Outcome = step.Outcome
		res.Skip = step.Skip
		res.Checker = step.Checker
		res.ExitCode = step.ExitCode
		// A verdict step that did not conclude — timed out, broken binary —
		// leaves the gate with nothing to say about the file, and the later
		// steps have no verdict to add.
		settled = step.Outcome != Passed
	}

	// Belt and braces on the invariant the whole package rests on: nothing
	// that did not run may carry a plausible-looking exit code.
	if res.Outcome == NotRun {
		res.Checker = ""
		res.ExitCode = -1
	}

	res.Duration = g.clock().Now().Sub(start)
	res.Output = g.render(res)
	if cancelled != nil {
		if res.Outcome == NotRun && res.Skip == SkipNone {
			res.Skip = SkipCancelled
		}
		return res, fmt.Errorf("syntax gate on %q: %w", path, cancelled)
	}
	return res, nil
}

// notReached is a step the gate never got to.
func notReached(sp spec) Step {
	return Step{
		Argv:     sp.argv,
		Checker:  strings.Join(sp.argv, " "),
		Dir:      sp.relDir,
		Verdict:  sp.verdict,
		Outcome:  NotRun,
		Skip:     SkipNotReached,
		ExitCode: -1,
	}
}

// runStep executes one checker and reports what it produced.
//
// It returns a non-nil error only for a cancellation. Every other way a step
// can fail to conclude is a [Skip] on the returned [Step], because none of them
// is evidence about the edit.
func (g *Gate) runStep(ctx context.Context, sp spec) (Step, error) {
	step := Step{
		Argv:     sp.argv,
		Checker:  strings.Join(sp.argv, " "),
		Dir:      sp.relDir,
		Verdict:  sp.verdict,
		Outcome:  NotRun,
		ExitCode: -1,
	}

	var out bytes.Buffer
	// argv[0] is the plain binary name and cmd.Path the resolved one: the
	// process is looked up exactly once, through the injectable seam, and
	// the machine-specific path never reaches Argv and so never reaches the
	// journal.
	cmd := exec.Command(sp.resolved, sp.argv[1:]...) //nolint:gosec // the argv is built here, never by the model
	cmd.Args[0] = sp.argv[0]
	cmd.Dir = sp.dir
	cmd.Env = sp.env
	cmd.Stderr = &out
	if sp.captureStdout {
		cmd.Stdout = &out
	}
	cmd.WaitDelay = g.grace()
	procgroup.Isolate(cmd)

	clock := g.clock()
	started := clock.Now()
	if err := cmd.Start(); err != nil {
		step.Skip = SkipCheckerBroken
		// Normalised like any captured stream: the error names the absolute
		// path the binary was resolved to, and this string is journaled.
		step.Output = g.normalise(fmt.Sprintf("%s could not be started: %v\n", sp.argv[0], err))
		step.Duration = clock.Now().Sub(started)
		return step, nil
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	timer, stop := clock.NewTimer(g.timeout())
	defer stop()

	var cancelled error
	select {
	case <-waited:
	case <-timer:
		step.Skip = SkipTimedOut
		step.StoppedBy, _ = procgroup.Stop(cmd, waited, clock, g.grace())
	case <-ctx.Done():
		step.Skip = SkipCancelled
		step.StoppedBy, _ = procgroup.Stop(cmd, waited, clock, g.grace())
		cancelled = ctx.Err()
	}

	// cmd.Wait has returned on every path above, which is what makes reading
	// the buffer here safe: os/exec's copying goroutines are joined by Wait, so
	// touching it earlier would be a data race rather than merely a partial
	// read.
	step.Output = g.normalise(out.String())
	step.Duration = clock.Now().Sub(started)

	if step.Skip != SkipNone {
		return step, cancelled
	}
	if ps := cmd.ProcessState; ps != nil {
		step.ExitCode, step.Signal = procgroup.ExitStatus(ps)
	}
	// A checker killed by a signal produced no verdict either. Reading its
	// -1 as "non-zero, therefore a syntax error" would charge an OOM kill to
	// the edit.
	switch {
	case step.Signal != "":
		step.Skip = SkipCheckerBroken
	case step.ExitCode == 0:
		step.Outcome = Passed
	default:
		step.Outcome = Failed
	}
	return step, nil
}

// localPath converts a repository-relative slash path to a local one, refusing
// anything that is absolute or escapes the root.
func localPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: it is empty", ErrBadPath)
	}
	local, err := filepath.Localize(path)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBadPath, err)
	}
	return local, nil
}

// normalise rewrites the absolute repository root out of a captured stream.
//
// This is the only edit made to any checker's output anywhere in this package,
// and it drops nothing: an absolute path becomes the repo-relative path naming
// the same file, which is both what the model thinks in and what makes two
// machines agree. The evaluated form is replaced as well as the literal one
// because a root under a symlinked temp directory — /tmp on macOS — comes back
// from a subprocess resolved.
func (g *Gate) normalise(s string) string {
	if s == "" || g.Root == "" {
		return s
	}
	roots := []string{g.Root}
	if eval, err := filepath.EvalSymlinks(g.Root); err == nil && eval != g.Root {
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
// The headline never says "ok" for a gate that did not run. It says what did
// not happen and why, and for the honest no-op it says in words that this is
// not a pass — because the reader that most needs telling is a model deciding
// whether its edit landed.
func (g *Gate) render(r Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "syntax gate on %s: %s\n", r.Path, headline(r))
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "--- %s: %s ---\n", s.Checker, stepStatus(s))
		writeStream(&b, s.Output)
	}
	return b.String()
}

func headline(r Result) string {
	switch r.Outcome {
	case Passed:
		return fmt.Sprintf("passed (%s)", r.Checker)
	case Failed:
		return fmt.Sprintf("FAILED (%s exited %d)", r.Checker, r.ExitCode)
	default:
		return "no checker ran, so nothing here is a pass — " + skipReason(r)
	}
}

// skipReason is the sentence a model reads when the gate declined to vouch for
// a file. Each one names what would make the answer different.
func skipReason(r Result) string {
	switch r.Skip {
	case SkipNoChecker:
		return "kopicode has no syntax checker for this file type"
	case SkipToolMissing:
		return fmt.Sprintf("the %s checker is not installed on this machine", r.Language)
	case SkipCheckerBroken:
		return fmt.Sprintf("the %s checker is installed but could not be run", r.Language)
	case SkipNoProject:
		return fmt.Sprintf("the %s checker needs a project file that is not present", r.Language)
	case SkipNotAFile:
		return "the path is not a regular file"
	case SkipTimedOut:
		return "the checker outlived its timeout and its process group was killed"
	case SkipCancelled:
		return "the check was cancelled"
	default:
		return "no reason was recorded, which is itself a bug"
	}
}

// stepStatus is the header a step gets in [Result.Output].
//
// **No duration appears here, deliberately.** run_shell renders one and this
// does not, because the two strings are read by different things: a shell
// result is a tool result a human reads, and this one is journaled and then
// compared byte-for-byte when a session is replayed (SLICE-1's acceptance
// criteria). Wall-clock time is the one value guaranteed to differ between two
// identical runs, so it stays on [Step.Duration], where the engine can reach it
// and the comparison cannot. TestTheSameCheckTwiceProducesTheSameOutput is what
// found this.
func stepStatus(s Step) string {
	switch {
	case s.Skip == SkipNotReached:
		return "not reached"
	case s.Skip != SkipNone && s.StoppedBy != "":
		return fmt.Sprintf("%s, process group killed with %s", s.Skip, s.StoppedBy)
	case s.Skip != SkipNone:
		return string(s.Skip)
	case s.Signal != "":
		return "killed by " + s.Signal
	default:
		return fmt.Sprintf("exit %d", s.ExitCode)
	}
}

// writeStream writes a captured stream whole, terminated by exactly one newline
// so the next header starts on its own line. Nothing is dropped: a stream that
// did not end in a newline gains one, and that is the only other edit made to
// any stream in this package.
func writeStream(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
	}
}
