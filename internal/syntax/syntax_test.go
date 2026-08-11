package syntax_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/syntax"
)

// The seam here is the package's public one: build a repository in a temp
// directory, run the gate over a file in it, and assert on the [syntax.Result]
// and on the tree afterwards. Nothing reaches into a step's internals — what
// the engine will journal is exactly what these tests read.
//
// Two things are load-bearing across the whole file and are worth stating once.
//
// **A checker that is not installed must never read as a pass.** The gate is
// what SLICE-1 §9 charges to the `harness` bucket, and the slice-1 bar is zero
// harness failures. Both directions are wrong: a missing node reported as a
// pass silently stops checking JavaScript, and a missing node reported as a
// failure charges every `.js` edit on an ordinary machine to the harness.
//
// **The Go path is testable for real, the others are not.** `go` and `gofmt`
// are present by definition — this suite runs under `go test`. python and node
// are not, so those tests assert when the binary is there and say what went
// unexercised when it is not. Availability itself is tested through the
// [syntax.Gate.LookPath] seam, which does not depend on the machine at all.

// fixture is a repository in a temp directory.
type fixture struct {
	t    *testing.T
	root string
}

// newFixture writes files, keyed by slash path relative to the root.
func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return &fixture{t: t, root: root}
}

func (f *fixture) gate() *syntax.Gate {
	return &syntax.Gate{Root: f.root, Timeout: 60 * time.Second}
}

// check runs the gate and fails the test on an unexpected error.
func (f *fixture) check(g *syntax.Gate, path string) syntax.Result {
	f.t.Helper()
	res, err := g.Check(context.Background(), path)
	if err != nil {
		f.t.Fatalf("Check(%q): unexpected error: %v", path, err)
	}
	return res
}

// have reports whether name is on PATH, so a test can say what it did not
// exercise rather than silently passing.
func have(t *testing.T, name string) bool {
	t.Helper()
	_, err := exec.LookPath(name)
	return err == nil
}

const validGo = "package main\n\nfunc main() {}\n"

// brokenGo does not parse: the parameter list is never closed.
const brokenGo = "package main\n\nfunc main( {\n"

// untypedGo parses cleanly and does not typecheck. It is the case the go build
// decision turns on.
const untypedGo = "package main\n\nfunc main() { var s string = 1; _ = s }\n"

const goMod = "module example.com/fixture\n\ngo 1.26\n"

// --- the verdict --------------------------------------------------------

func TestASyntaxErrorFailsTheGate(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": goMod, "main.go": brokenGo})
	res := f.check(f.gate(), "main.go")

	if res.Outcome != syntax.Failed {
		t.Fatalf("outcome = %s, want %s\n%s", res.Outcome, syntax.Failed, res.Output)
	}
	if !res.Ran() {
		t.Error("Ran() is false for a gate that ran and failed")
	}
	if res.ExitCode == 0 {
		t.Errorf("exit code = 0 on a failure")
	}
	if !strings.HasPrefix(res.Checker, "gofmt -e") {
		t.Errorf("checker = %q, want the gofmt step to have decided the verdict", res.Checker)
	}
	if !strings.Contains(res.Output, "main.go:3") {
		t.Errorf("output does not name the offending line:\n%s", res.Output)
	}
}

func TestValidCodePassesTheGate(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": goMod, "main.go": validGo})
	res := f.check(f.gate(), "main.go")

	if res.Outcome != syntax.Passed {
		t.Fatalf("outcome = %s, want %s\n%s", res.Outcome, syntax.Passed, res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d on a pass, want 0", res.ExitCode)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("got %d steps, want gofmt and go build", len(res.Steps))
	}
	if res.Steps[1].Outcome != syntax.Passed {
		t.Errorf("the go build step did not run on a valid file: %+v", res.Steps[1])
	}
}

// TestAGoBuildFailureDoesNotFailTheGate is the decision the card asks to be
// made explicitly, asserted rather than described.
//
// `gofmt -e` and `go build` answer different questions. gofmt parses the one
// file that was edited; go build typechecks a whole package, and a correct edit
// can legitimately leave a package failing to typecheck — the model edits a
// caller this turn and writes the callee next. SLICE-1 §9 charges a gate
// failure straight to `harness` and the slice-1 bar is *zero* harness failures,
// so a type error must not set the verdict.
//
// Nothing is hidden by that: the build's real exit code is on its step, and its
// full output is in Result.Output, which is what the model reads.
func TestAGoBuildFailureDoesNotFailTheGate(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": goMod, "main.go": untypedGo})
	res := f.check(f.gate(), "main.go")

	if res.Outcome != syntax.Passed {
		t.Fatalf("outcome = %s, want %s — a type error is not a parse error\n%s",
			res.Outcome, syntax.Passed, res.Output)
	}
	if !strings.HasPrefix(res.Checker, "gofmt -e") {
		t.Errorf("checker = %q, want the file-scoped step to have decided the verdict", res.Checker)
	}

	build := res.Steps[len(res.Steps)-1]
	if build.Verdict {
		t.Error("the go build step is marked as a verdict step")
	}
	if build.Outcome != syntax.Failed {
		t.Fatalf("the go build step did not fail on code that does not typecheck: %+v", build)
	}
	if !strings.Contains(res.Output, "cannot use 1") {
		t.Errorf("the build diagnostic was not returned to the model:\n%s", res.Output)
	}
}

// TestTheBuildStepIsScopedToTheEditedPackage keeps the advisory step from
// reporting on the whole module. A break in a sibling package is not this
// edit's business and is not even fetched.
func TestTheBuildStepIsScopedToTheEditedPackage(t *testing.T) {
	f := newFixture(t, map[string]string{
		"go.mod":           goMod,
		"good/good.go":     "package good\n\nfunc F() int { return 1 }\n",
		"broken/broken.go": "package broken\n\nfunc G() int { return \"no\" }\n",
		"good/another.go":  "package good\n\nfunc H() int { return 2 }\n",
	})
	res := f.check(f.gate(), "good/good.go")

	if res.Outcome != syntax.Passed {
		t.Fatalf("outcome = %s, want %s\n%s", res.Outcome, syntax.Passed, res.Output)
	}
	build := res.Steps[len(res.Steps)-1]
	if build.Outcome != syntax.Passed {
		t.Errorf("the build step reported on a package that was not edited: %+v\n%s",
			build, res.Output)
	}
	if got := strings.Join(build.Argv, " "); !strings.HasSuffix(got, "./good") {
		t.Errorf("build argv = %q, want it scoped to ./good", got)
	}
}

// --- the honest no-op ---------------------------------------------------

// notRunCase is one way the gate can decline to vouch for a file.
type notRunCase struct {
	name  string
	files map[string]string
	path  string
	gate  func(*fixture) *syntax.Gate
	want  syntax.Skip
}

// notRunCases is every route to [syntax.NotRun] that a caller can reach through
// the public API. TestNothingThatDidNotRunLooksLikeAPass asserts the same
// invariants over all of them, so a new route added later inherits the guard
// rather than needing its own.
var notRunCases = []notRunCase{
	{
		name:  "no checker is registered for the extension",
		files: map[string]string{"NOTES.md": "# notes\n"},
		path:  "NOTES.md",
		want:  syntax.SkipNoChecker,
	},
	{
		name:  "jsx is deliberately unregistered, because node --check rejects it",
		files: map[string]string{"App.jsx": "const a = <div/>;\n"},
		path:  "App.jsx",
		want:  syntax.SkipNoChecker,
	},
	{
		name:  "the checker is not installed",
		files: map[string]string{"app.js": "syntax error here (\n"},
		path:  "app.js",
		gate:  func(f *fixture) *syntax.Gate { return withLookPath(f, notInstalled) },
		want:  syntax.SkipToolMissing,
	},
	{
		name:  "the checker is installed but cannot be started",
		files: map[string]string{"app.js": "syntax error here (\n", "fake": "not an executable\n"},
		path:  "app.js",
		gate: func(f *fixture) *syntax.Gate {
			return withLookPath(f, func(string) (string, error) {
				return filepath.Join(f.root, "fake"), nil
			})
		},
		want: syntax.SkipCheckerBroken,
	},
	{
		// The card is explicit that tsc runs only "where a tsconfig exists", so
		// its absence is detected and recorded rather than assumed away.
		name:  "typescript without a tsconfig",
		files: map[string]string{"app.ts": "const a: number = 1;\n"},
		path:  "app.ts",
		want:  syntax.SkipNoProject,
	},
	{
		// And the other half: a project that is configured, on a machine with
		// no TypeScript installed. LookPath is forced here rather than relying
		// on tsc being absent, so the case is exercised on a machine that has
		// it too.
		name: "typescript with a tsconfig but no tsc",
		files: map[string]string{
			"tsconfig.json": "{}\n",
			"app.ts":        "const a: number = 1;\n",
		},
		path: "app.ts",
		gate: func(f *fixture) *syntax.Gate { return withLookPath(f, notInstalled) },
		want: syntax.SkipToolMissing,
	},
	{
		name:  "the file is not there",
		files: map[string]string{"go.mod": goMod},
		path:  "gone.go",
		want:  syntax.SkipNotAFile,
	},
	{
		name:  "the path is a directory",
		files: map[string]string{"pkg/x.go": validGo},
		path:  "pkg",
		want:  syntax.SkipNotAFile,
	},
}

func notInstalled(name string) (string, error) {
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func withLookPath(f *fixture, lp func(string) (string, error)) *syntax.Gate {
	g := f.gate()
	g.LookPath = lp
	return g
}

// TestNothingThatDidNotRunLooksLikeAPass is the central guard of this package.
//
// A gate that did not run and a gate that passed must be impossible to confuse,
// and "impossible" has to mean more than a comment. Four independent signals
// have to agree, so that a caller reading any one of them gets the truth:
// Outcome is NotRun, Ran() is false, ExitCode is -1 rather than the 0 that
// reads as success, and Checker is empty because nothing decided anything. The
// rendered output has to say so in words as well, because the reader who most
// needs telling is a model deciding whether its edit landed.
func TestNothingThatDidNotRunLooksLikeAPass(t *testing.T) {
	for _, tc := range notRunCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.files)
			g := f.gate()
			if tc.gate != nil {
				g = tc.gate(f)
			}
			res := f.check(g, tc.path)

			if res.Outcome != syntax.NotRun {
				t.Fatalf("outcome = %s, want %s\n%s", res.Outcome, syntax.NotRun, res.Output)
			}
			if res.Skip != tc.want {
				t.Errorf("skip = %q, want %q", res.Skip, tc.want)
			}
			if res.Ran() {
				t.Error("Ran() is true; journal.SyntaxGateRun.Ran would claim a checker ran")
			}
			if res.ExitCode != -1 {
				t.Errorf("exit code = %d, want -1; %d would read as a clean checker exit",
					res.ExitCode, res.ExitCode)
			}
			if res.Checker != "" {
				t.Errorf("checker = %q, want empty; nothing decided the verdict", res.Checker)
			}
			if !strings.Contains(res.Output, "no checker ran") {
				t.Errorf("output does not say the gate did not run:\n%s", res.Output)
			}
			if strings.Contains(res.Output, "passed") {
				t.Errorf("output of a gate that did not run contains %q:\n%s", "passed", res.Output)
			}
		})
	}
}

// TestABrokenCheckerIsNotAFailure separates the third answer to "is node
// installed" from the second.
//
// A binary that is present and cannot be executed is a broken machine. Reading
// its non-start as a syntax error would charge a misconfigured toolchain to the
// edit, which is the same laundering ADR-0006 §3 exists to stop, arriving
// through the environment rather than through the matcher.
func TestABrokenCheckerIsNotAFailure(t *testing.T) {
	f := newFixture(t, map[string]string{
		"app.js": "console.log(1)\n",
		"fake":   "not an executable\n",
	})
	g := withLookPath(f, func(string) (string, error) { return filepath.Join(f.root, "fake"), nil })

	res := f.check(g, "app.js")
	if res.Outcome == syntax.Failed {
		t.Fatalf("a checker that could not start was reported as a syntax failure:\n%s", res.Output)
	}
	if res.Skip != syntax.SkipCheckerBroken {
		t.Errorf("skip = %q, want %q", res.Skip, syntax.SkipCheckerBroken)
	}
	if len(res.Steps) != 1 || res.Steps[0].Skip != syntax.SkipCheckerBroken {
		t.Errorf("the broken step was not recorded: %+v", res.Steps)
	}
	// The exec error names the absolute path the binary was resolved to, and
	// this string is journaled like any other captured output.
	if strings.Contains(res.Output, f.root) {
		t.Errorf("the start failure leaked the absolute root into the journal:\n%s", res.Output)
	}
}

// TestAMissingVerdictCheckerRunsNoAdvisoryStep keeps journal.SyntaxGateRun.Ran
// truthful. "Ran" means a checker reached a verdict about the file; running an
// advisory build while reporting Ran=false would make the field mean two
// different things depending on the language.
func TestAMissingVerdictCheckerRunsNoAdvisoryStep(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": goMod, "main.go": brokenGo})
	g := withLookPath(f, func(name string) (string, error) {
		if name == "gofmt" {
			return notInstalled(name)
		}
		return exec.LookPath(name)
	})

	res := f.check(g, "main.go")
	if res.Outcome != syntax.NotRun || res.Skip != syntax.SkipToolMissing {
		t.Fatalf("outcome = %s skip = %q, want not_run/tool_missing\n%s",
			res.Outcome, res.Skip, res.Output)
	}
	for _, s := range res.Steps {
		if s.Outcome != syntax.NotRun {
			t.Errorf("step %q ran even though no verdict step could: %+v", s.Checker, s)
		}
	}
}

// --- selection ----------------------------------------------------------

func TestLanguageSelectionByExtension(t *testing.T) {
	cases := map[string]syntax.Language{
		"a.go":       syntax.LangGo,
		"a.py":       syntax.LangPython,
		"a.js":       syntax.LangJavaScript,
		"a.mjs":      syntax.LangJavaScript,
		"a.cjs":      syntax.LangJavaScript,
		"a.ts":       syntax.LangTypeScript,
		"a.tsx":      syntax.LangTypeScript,
		"a.GO":       syntax.LangGo,
		"a.jsx":      syntax.LangUnknown,
		"a.md":       syntax.LangUnknown,
		"Makefile":   syntax.LangUnknown,
		"x/y/a.py":   syntax.LangPython,
		"a.go.bak":   syntax.LangUnknown,
		".gitignore": syntax.LangUnknown,
	}
	for name, want := range cases {
		if got := syntax.LanguageOf(name); got != want {
			t.Errorf("LanguageOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// --- the other languages, where the machine has them --------------------

func TestPythonGate(t *testing.T) {
	if !have(t, "python3") && !have(t, "python") {
		t.Skip("no python on PATH: the py_compile verdict path went unexercised here")
	}
	f := newFixture(t, map[string]string{
		"ok.py":  "def f():\n    return 1\n",
		"bad.py": "def f(:\n    pass\n",
	})
	g := f.gate()

	if res := f.check(g, "ok.py"); res.Outcome != syntax.Passed {
		t.Errorf("ok.py: outcome = %s, want %s\n%s", res.Outcome, syntax.Passed, res.Output)
	}
	res := f.check(g, "bad.py")
	if res.Outcome != syntax.Failed {
		t.Fatalf("bad.py: outcome = %s, want %s\n%s", res.Outcome, syntax.Failed, res.Output)
	}
	if !strings.Contains(res.Output, "bad.py") {
		t.Errorf("output does not name the file:\n%s", res.Output)
	}
}

// TestPythonLeavesNoBytecodeInTheTree holds a promise that is easy to break and
// invisible when broken. py_compile writes __pycache__ next to the source by
// design; a gate that runs after every edit would litter the user's tree and
// put the litter into every turn snapshot.
func TestPythonLeavesNoBytecodeInTheTree(t *testing.T) {
	if !have(t, "python3") && !have(t, "python") {
		t.Skip("no python on PATH")
	}
	f := newFixture(t, map[string]string{"pkg/ok.py": "x = 1\n"})
	if res := f.check(f.gate(), "pkg/ok.py"); res.Outcome != syntax.Passed {
		t.Fatalf("outcome = %s, want %s\n%s", res.Outcome, syntax.Passed, res.Output)
	}

	var strays []string
	err := filepath.WalkDir(f.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == "__pycache__" || strings.HasSuffix(d.Name(), ".pyc") {
			strays = append(strays, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the fixture: %v", err)
	}
	if len(strays) > 0 {
		t.Errorf("py_compile left bytecode in the repository: %v", strays)
	}
}

func TestJavaScriptGate(t *testing.T) {
	if !have(t, "node") {
		t.Skip("no node on PATH: the node --check verdict path went unexercised here")
	}
	f := newFixture(t, map[string]string{
		"ok.js":  "console.log(1)\n",
		"bad.js": "function f( {\n",
	})
	g := f.gate()

	if res := f.check(g, "ok.js"); res.Outcome != syntax.Passed {
		t.Errorf("ok.js: outcome = %s, want %s\n%s", res.Outcome, syntax.Passed, res.Output)
	}
	if res := f.check(g, "bad.js"); res.Outcome != syntax.Failed {
		t.Errorf("bad.js: outcome = %s, want %s\n%s", res.Outcome, syntax.Failed, res.Output)
	}
}

// --- determinism --------------------------------------------------------

// TestOutputCarriesNoAbsolutePaths is the machine-independence guard.
//
// The gate's output is journaled, and a journal full of /tmp/TestFoo123456
// cannot be compared across runs, let alone across machines. Two things keep it
// out: every checker runs with Dir set and is handed a *relative* path, and
// whatever absolute root still comes back is rewritten to its repo-relative
// form. Nothing is dropped by either.
func TestOutputCarriesNoAbsolutePaths(t *testing.T) {
	files := map[string]string{"go.mod": goMod, "pkg/bad.go": brokenGo}
	if have(t, "node") {
		files["pkg/bad.js"] = "function f( {\n"
	}
	f := newFixture(t, files)
	g := f.gate()

	for name := range files {
		if name == "go.mod" {
			continue
		}
		res := f.check(g, name)
		if strings.Contains(res.Output, f.root) {
			t.Errorf("%s: output contains the absolute root %q:\n%s", name, f.root, res.Output)
		}
		if !strings.Contains(res.Output, "pkg/bad") {
			t.Errorf("%s: output lost the relative path:\n%s", name, res.Output)
		}
	}
}

// TestTheSameCheckTwiceProducesTheSameOutput is the byte-identical-journal
// requirement reduced to what this package can be held to: the same file, the
// same toolchain and the same root produce the same bytes. Durations vary and
// are not part of Output.
func TestTheSameCheckTwiceProducesTheSameOutput(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": goMod, "main.go": brokenGo})
	g := f.gate()

	first := f.check(g, "main.go")
	second := f.check(g, "main.go")
	if first.Output != second.Output {
		t.Errorf("two runs disagreed:\n--- first ---\n%s\n--- second ---\n%s",
			first.Output, second.Output)
	}
	if first.Checker != second.Checker || first.ExitCode != second.ExitCode {
		t.Errorf("two runs disagreed on the verdict: %q/%d vs %q/%d",
			first.Checker, first.ExitCode, second.Checker, second.ExitCode)
	}
}

// TestArgvRecordsThePlainBinaryName keeps a machine-specific path out of the
// journal. The binary is resolved to an absolute path to be executed; what gets
// recorded is the name a reader would type.
func TestArgvRecordsThePlainBinaryName(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": goMod, "main.go": validGo})
	res := f.check(f.gate(), "main.go")

	for _, s := range res.Steps {
		if filepath.IsAbs(s.Argv[0]) {
			t.Errorf("step argv[0] = %q is an absolute path", s.Argv[0])
		}
		if strings.Contains(s.Checker, f.root) {
			t.Errorf("step checker %q names the fixture root", s.Checker)
		}
	}
}

// --- cancellation -------------------------------------------------------

// TestCancellationIsNeitherAPassNorAFailure follows KAN-808's shape: the result
// is for the caller and the error is for the classifier, and neither
// substitutes for the other. A cancellation reported as a clean pass is charged
// to the model by SLICE-1 §9's "everything else" arm; reported as a failure it
// is charged to the harness. It is neither.
func TestCancellationIsNeitherAPassNorAFailure(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": goMod, "main.go": brokenGo})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := f.gate().Check(ctx, "main.go")
	if err == nil {
		t.Fatal("Check on an ended context returned a nil error; a silent cancellation " +
			"reads as a clean stop and is charged to the model")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not wrap context.Canceled", err)
	}
	if res.Outcome != syntax.NotRun {
		t.Errorf("outcome = %s, want %s", res.Outcome, syntax.NotRun)
	}
	if res.Skip != syntax.SkipCancelled {
		t.Errorf("skip = %q, want %q", res.Skip, syntax.SkipCancelled)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", res.ExitCode)
	}
}

// --- argument handling --------------------------------------------------

func TestAPathOutsideTheRootIsTheCallersBug(t *testing.T) {
	f := newFixture(t, map[string]string{"go.mod": goMod})
	for _, p := range []string{"../escape.go", "/etc/passwd.go", ""} {
		if _, err := f.gate().Check(context.Background(), p); !errors.Is(err, syntax.ErrBadPath) {
			t.Errorf("Check(%q) error = %v, want ErrBadPath", p, err)
		}
	}
}

// --- the enum ------------------------------------------------------------

// TestTheZeroOutcomeIsNotRun is the invariant everything else leans on. If
// Passed were the zero value, a Result nobody filled in would vouch for a file.
func TestTheZeroOutcomeIsNotRun(t *testing.T) {
	var zero syntax.Outcome
	if zero != syntax.NotRun {
		t.Fatalf("the zero Outcome is %s, want %s", zero, syntax.NotRun)
	}
	var res syntax.Result
	if res.Ran() {
		t.Error("the zero Result reports that a checker ran")
	}
	if syntax.Outcome(200).String() != "not_run" {
		t.Errorf("an unrecognised Outcome renders as %q; the one string that must never "+
			"appear by accident is the one meaning success", syntax.Outcome(200))
	}
}

func TestOutcomeWireForms(t *testing.T) {
	want := map[syntax.Outcome]string{
		syntax.NotRun: "not_run",
		syntax.Passed: "passed",
		syntax.Failed: "failed",
	}
	seen := map[string]bool{}
	for o, s := range want {
		if got := o.String(); got != s {
			t.Errorf("Outcome(%d).String() = %q, want %q", o, got, s)
		}
		if seen[s] {
			t.Errorf("two outcomes share the wire form %q", s)
		}
		seen[s] = true
	}
}
