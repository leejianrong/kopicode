package syntax

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file is in-package on purpose. It is the completeness half of the suite,
// and completeness is a claim about the registry rather than about behaviour —
// there is no way to ask from outside whether two languages claim the same
// extension, or whether a Skip constant somebody added has a sentence to go
// with it.
//
// The shape follows internal/arch and internal/tools/cancel_test.go: the set of
// things under test is **derived** rather than listed, so the next language, or
// the next Skip value, fails the suite instead of quietly checking nothing. A
// per-extension checker registry is exactly the structure that rots silently,
// because the failure mode of a missing entry is an honest-looking "no checker
// available" that nobody investigates.

// declaredConsts returns the names of every constant of type name declared in
// this package's non-test source.
//
// It reads the source rather than using reflection because Go constants are not
// enumerable at runtime: a `const LangRuby Language = "ruby"` nobody wired up is
// invisible to reflect and perfectly visible to the parser.
func declaredConsts(t *testing.T, typeName string) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, s := range gen.Specs {
				spec, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				id, ok := spec.Type.(*ast.Ident)
				if !ok || id.Name != typeName {
					continue
				}
				for _, n := range spec.Names {
					names = append(names, n.Name)
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("found no constants of type %s; the guard is broken, not the registry", typeName)
	}
	sort.Strings(names)
	return names
}

// constValue resolves a declared constant by name, so the parser's view and the
// compiler's can be compared.
var (
	languageByName = map[string]Language{
		"LangUnknown":    LangUnknown,
		"LangGo":         LangGo,
		"LangPython":     LangPython,
		"LangJavaScript": LangJavaScript,
		"LangTypeScript": LangTypeScript,
	}
	skipByName = map[string]Skip{
		"SkipNone":          SkipNone,
		"SkipNoChecker":     SkipNoChecker,
		"SkipToolMissing":   SkipToolMissing,
		"SkipCheckerBroken": SkipCheckerBroken,
		"SkipNoProject":     SkipNoProject,
		"SkipNotAFile":      SkipNotAFile,
		"SkipTimedOut":      SkipTimedOut,
		"SkipCancelled":     SkipCancelled,
		"SkipNotReached":    SkipNotReached,
	}
)

// TestEveryDeclaredLanguageIsWiredUp is the guard the card asks for: a language
// added to the type without being added to the registry must fail the suite,
// not return "no checker available" forever.
func TestEveryDeclaredLanguageIsWiredUp(t *testing.T) {
	declared := declaredConsts(t, "Language")

	registered := map[Language]bool{}
	for _, c := range registry {
		registered[c.lang] = true
	}

	for _, name := range declared {
		lang, known := languageByName[name]
		if !known {
			t.Errorf("%s is a Language constant with no entry in languageByName; add one, "+
				"so the rest of this file can see it at all", name)
			continue
		}
		if lang == LangUnknown {
			continue
		}
		if !registered[lang] {
			t.Errorf("%s (%q) is declared but has no entry in registry, so every file in that "+
				"language gets an honest no-op forever. Add a checker, or delete the constant",
				name, lang)
		}
	}

	// And the other way: a registry entry for something the type does not
	// declare would be unreachable through LanguageOf.
	for _, c := range registry {
		if !slices.ContainsFunc(declared, func(n string) bool { return languageByName[n] == c.lang }) {
			t.Errorf("registry has an entry for %q, which is not a declared Language constant", c.lang)
		}
	}
}

// wantExtensions is the promise in SLICE-1's affordance table, written down
// where a change to it has to be deliberate. A language whose extensions move
// without this table moving fails, which is the point: the set of files
// kopicode claims to check is a product decision, not an implementation detail.
var wantExtensions = map[Language][]string{
	LangGo:         {".go"},
	LangPython:     {".py"},
	LangJavaScript: {".js", ".mjs", ".cjs"},
	LangTypeScript: {".ts", ".tsx", ".mts", ".cts"},
}

func TestRegisteredExtensionsAreTheOnesPromised(t *testing.T) {
	got := map[Language][]string{}
	for _, c := range registry {
		got[c.lang] = append(got[c.lang], c.exts...)
	}

	for lang, want := range wantExtensions {
		have := got[lang]
		sort.Strings(have)
		sorted := slices.Clone(want)
		sort.Strings(sorted)
		if strings.Join(have, ",") != strings.Join(sorted, ",") {
			t.Errorf("%s claims %v, want %v", lang, have, sorted)
		}
		delete(got, lang)
	}
	for lang, exts := range got {
		t.Errorf("%s claims %v but has no row in wantExtensions; the affordance table in "+
			"SLICE-1 §V2 is a promise and this is where it is kept", lang, exts)
	}

	// .jsx is called out by name because registering it is the mistake that
	// looks right: node --check rejects JSX outright, so every React file
	// would fail the gate and, per SLICE-1 §9, land in the harness bucket.
	if LanguageOf("App.jsx") != LangUnknown {
		t.Error(".jsx is registered; node --check cannot parse JSX, so this would charge " +
			"every React edit to the harness bucket")
	}
}

func TestNoExtensionIsClaimedTwice(t *testing.T) {
	owner := map[string]Language{}
	for _, c := range registry {
		for _, ext := range c.exts {
			if prev, dup := owner[ext]; dup {
				t.Errorf("%q is claimed by both %s and %s; the index is a map, so one of them "+
					"silently wins", ext, prev, c.lang)
			}
			owner[ext] = c.lang
		}
	}
}

func TestEveryRegistryEntryIsUsable(t *testing.T) {
	for _, c := range registry {
		if c.lang == LangUnknown {
			t.Error("registry has an entry for LangUnknown, which is the zero value")
		}
		if len(c.exts) == 0 {
			t.Errorf("%s claims no extensions, so LanguageOf can never select it", c.lang)
		}
		if c.plan == nil {
			t.Errorf("%s has no plan", c.lang)
		}
		for _, ext := range c.exts {
			if !strings.HasPrefix(ext, ".") || ext != strings.ToLower(ext) {
				t.Errorf("%s claims %q; extensions are lowercase and carry the leading dot, "+
					"because LanguageOf lowercases what it is given", c.lang, ext)
			}
		}
	}
}

// TestEveryDeclaredSkipExplainsItself keeps the honest no-op honest. A Skip
// value with no sentence renders as "no reason was recorded, which is itself a
// bug", which is better than silence and much worse than a sentence.
func TestEveryDeclaredSkipExplainsItself(t *testing.T) {
	fallback := skipReason(Result{Skip: Skip("invented-for-this-test")})

	for _, name := range declaredConsts(t, "Skip") {
		skip, known := skipByName[name]
		if !known {
			t.Errorf("%s is a Skip constant with no entry in skipByName", name)
			continue
		}
		switch skip {
		case SkipNone, SkipNotReached:
			// Neither is ever a gate-level skip: SkipNone means nothing was
			// skipped, and SkipNotReached only ever appears on a step.
			continue
		}
		if got := skipReason(Result{Skip: skip, Language: LangGo}); got == fallback {
			t.Errorf("%s (%q) has no case in skipReason, so a model is told only that "+
				"something went unrecorded", name, skip)
		}
	}
}

// --- subprocess discipline ----------------------------------------------

// TestEveryStepNamesItsDirectoryAndBuildsItsEnvironment is this package's
// version of the rule internal/arch enforces for git.
//
// CLAUDE.md's rule is written about git because that is what corrupted this
// repository, but the reason generalises: a subprocess that inherits its
// working directory runs wherever the harness happened to be, and one that
// inherits its environment does whatever GOFLAGS, GOOS, PYTHONPATH or
// NODE_OPTIONS tells it to. These are not git, which is a reason to be more
// careful rather than less — internal/arch's static guard does not cover them.
//
// This checks the values rather than merely that an assignment exists, which is
// the one thing the static analysis cannot do.
func TestEveryStepNamesItsDirectoryAndBuildsItsEnvironment(t *testing.T) {
	// Every strippable variable is set to a value nothing else would produce,
	// so the assertion is "the inherited value did not survive" rather than
	// "the name is absent". The distinction matters: the Go steps deliberately
	// *set* GOFLAGS, and a test that merely looked for the name would force
	// them to leave it inherited instead.
	const sentinel = "inherited-from-the-parent"
	for _, s := range stripped {
		t.Setenv(s.name, sentinel)
	}

	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/f\n\ngo 1.26\n")
	write(t, root, "tsconfig.json", "{}\n")
	files := map[Language]string{
		LangGo:         "main.go",
		LangPython:     "main.py",
		LangJavaScript: "main.js",
		LangTypeScript: "main.ts",
	}
	for _, name := range files {
		write(t, root, name, "\n")
	}

	// Every checker resolves, so that every plan produces its steps rather
	// than skipping out before the fields under test are set.
	g := &Gate{Root: root, LookPath: func(name string) (string, error) {
		return filepath.Join(root, name), nil
	}}

	for lang, file := range files {
		specs, skip := g.plan(lang, file)
		if skip != SkipNone {
			t.Fatalf("%s: plan skipped with %q; the fixture was meant to make every step runnable",
				lang, skip)
		}
		for _, sp := range specs {
			if sp.cleanup != nil {
				sp.cleanup()
			}
			if sp.dir == "" {
				t.Errorf("%s: step %v does not name a directory, so it would run wherever the "+
					"harness happened to be", lang, sp.argv)
			}
			if !strings.HasPrefix(sp.dir, root) {
				t.Errorf("%s: step %v runs in %q, outside the repository root", lang, sp.argv, sp.dir)
			}
			if sp.env == nil {
				t.Errorf("%s: step %v inherits its environment; GOFLAGS, GOOS, PYTHONPATH and "+
					"NODE_OPTIONS all change what a checker concludes", lang, sp.argv)
			}
			for _, kv := range sp.env {
				name, value, _ := strings.Cut(kv, "=")
				if value != sentinel {
					continue
				}
				if slices.ContainsFunc(stripped, func(s struct{ name, why string }) bool {
					return s.name == name
				}) {
					t.Errorf("%s: step %v inherited %s, which %s", lang, sp.argv, name,
						reasonFor(name))
				}
			}
			if len(sp.argv) == 0 || filepath.IsAbs(sp.argv[0]) {
				t.Errorf("%s: argv = %v; argv[0] is recorded in the journal and must be the "+
					"plain binary name, not this machine's path to it", lang, sp.argv)
			}
		}
	}
}

func reasonFor(name string) string {
	for _, s := range stripped {
		if s.name == name {
			return s.why
		}
	}
	return "is on the strip list"
}

// TestTheStripListCoversTheCredential is the one entry that is not about
// correctness of a verdict. CLAUDE.md: the provider key appears in no journal,
// no blob, no log — and the cheapest guarantee is that the process which could
// print it never has it.
func TestTheStripListCoversTheCredential(t *testing.T) {
	t.Setenv(APIKeyEnv, "sk-not-a-real-key")
	g := &Gate{Root: t.TempDir()}
	for _, kv := range g.baseEnv() {
		if strings.HasPrefix(kv, APIKeyEnv+"=") {
			t.Fatalf("a checker would inherit %s", APIKeyEnv)
		}
	}
}

// TestTheStripListCoversWhatChangesAVerdict is the specification half of the
// environment rule, and it exists because the dynamic test above cannot be it.
//
// That test asserts "everything on the strip list is stripped", which is true
// of an empty strip list. Deleting GOFLAGS from `stripped` left it green — the
// implementation and the assertion moved together. So this one is a hand-written
// list, deliberately: it is the *specification*, and `stripped` is the
// implementation being held to it. A variable removed from the strip list now
// fails here, and adding a variable to the strip list does not require touching
// this table, which is the asymmetry you want.
func TestTheStripListCoversWhatChangesAVerdict(t *testing.T) {
	// Each of these can change what a checker concludes about a file that has
	// not changed, which is the definition of a variable that must not be
	// inherited by a measurement instrument.
	must := []string{
		APIKeyEnv,
		"GOFLAGS", "GOOS", "GOARCH", "CGO_ENABLED", "GO111MODULE", "GOEXPERIMENT", "GOWORK",
		"PYTHONPATH", "PYTHONHOME", "PYTHONSTARTUP",
		"NODE_OPTIONS", "NODE_PATH",
	}
	for _, name := range must {
		if !slices.ContainsFunc(stripped, func(s struct{ name, why string }) bool {
			return s.name == name
		}) {
			t.Errorf("%s is not stripped from a checker's environment. It changes what the "+
				"checker concludes about an unchanged file, so inheriting it makes the gate's "+
				"verdict depend on whoever's shell started kopicode", name)
		}
	}
	for _, s := range stripped {
		if s.why == "" {
			t.Errorf("%s is stripped with no reason recorded", s.name)
		}
	}
}

func TestGoStepsRefuseTheNetworkAndReadOnlyTheModule(t *testing.T) {
	env := goEnv([]string{"PATH=/usr/bin"})
	for _, want := range []string{"GOPROXY=off", "GOFLAGS=-mod=readonly"} {
		if !slices.Contains(env, want) {
			t.Errorf("the Go steps do not set %s; a gate that runs after every edit must not "+
				"be able to reach the network or rewrite go.mod", want)
		}
	}
}

// TestOnlyOneSubprocessCallSite is the structural half of the rule above.
//
// The dynamic test can only check the specs that plan() produces. This one
// makes sure there is nowhere else to produce one from: every subprocess in
// this package goes through runStep, so a second exec.Command would be a second
// place that could forget Dir, Env, the process group and the timeout.
func TestOnlyOneSubprocessCallSite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	var sites []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			if sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext" {
				sites = append(sites, name+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
			}
			return true
		})
	}

	if len(sites) != 1 {
		t.Errorf("found %d subprocess call sites %v, want exactly 1.\n"+
			"Every checker in this package goes through runStep, which sets Dir and Env, "+
			"puts the child in its own process group and bounds it with a timeout. A second "+
			"call site is a second place to forget all four — route it through runStep, or "+
			"move the guard and say why in the PR.", len(sites), sites)
	}
}

// --- killability ---------------------------------------------------------

// The two tests below drive runStep with a hand-built spec rather than going
// through Check, because the public seam gives no way to make a checker hang:
// the argv is decided by the registry. What is asserted is still an observable
// outcome — the Step the engine would journal — and the alternative is leaving
// the kill path untested, which for a gate that runs after every edit is the
// one thing that would hang the agent loop.

func sleepSpec(t *testing.T) spec {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fixture needs a POSIX sleep; the kill path is best-effort on Windows anyway")
	}
	return spec{
		argv:          []string{"sh", "-c", "sleep 60"},
		resolved:      "/bin/sh",
		dir:           t.TempDir(),
		relDir:        ".",
		env:           []string{},
		verdict:       true,
		captureStdout: true,
	}
}

func TestATimeoutIsNotAFailure(t *testing.T) {
	clock := newFakeClock()
	g := &Gate{Root: t.TempDir(), Clock: clock, Grace: time.Millisecond}

	done := make(chan Step, 1)
	go func() {
		step, _ := g.runStep(context.Background(), sleepSpec(t))
		done <- step
	}()

	clock.fire(t) // the step's timeout
	step := <-done

	if step.Outcome == Failed {
		t.Fatalf("a checker that outlived its timeout was reported as a syntax failure: %+v", step)
	}
	if step.Skip != SkipTimedOut {
		t.Errorf("skip = %q, want %q", step.Skip, SkipTimedOut)
	}
	if step.StoppedBy == "" {
		t.Error("nothing was reported as having stopped the process group")
	}
}

func TestACancelledStepIsKilledAndReported(t *testing.T) {
	g := &Gate{Root: t.TempDir(), Grace: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan Step, 1)
	errs := make(chan error, 1)
	go func() {
		step, err := g.runStep(ctx, sleepSpec(t))
		done <- step
		errs <- err
	}()

	cancel()
	step, err := <-done, <-errs

	if err == nil {
		t.Fatal("a cancelled step returned a nil error; a silent cancellation reads as a clean stop")
	}
	if step.Outcome != NotRun || step.Skip != SkipCancelled {
		t.Errorf("outcome = %s skip = %q, want not_run/cancelled", step.Outcome, step.Skip)
	}
}

// --- helpers -------------------------------------------------------------

func write(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// fakeClock is the injected clock, in the shape internal/tools settled on: a
// test fires the armed timer instead of waiting out a real one.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	armed  []*fakeTimer
	notify chan struct{}
}

type fakeTimer struct {
	d time.Duration
	c chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		now:    time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC),
		notify: make(chan struct{}, 8),
	}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := &fakeTimer{d: d, c: make(chan time.Time, 1)}
	c.mu.Lock()
	c.armed = append(c.armed, t)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return t.c, func() {}
}

// fire advances to the oldest armed timer and fires it, blocking until one
// exists. The wait is a handshake rather than a poll: NewTimer signals on
// notify, so fire wakes when the step arms its timeout. The deadline is only
// there so a broken run fails as a test rather than as a hang.
func (c *fakeClock) fire(t *testing.T) {
	t.Helper()
	for {
		c.mu.Lock()
		if len(c.armed) > 0 {
			timer := c.armed[0]
			c.armed = c.armed[1:]
			c.now = c.now.Add(timer.d)
			now := c.now
			c.mu.Unlock()
			timer.c <- now
			return
		}
		c.mu.Unlock()

		select {
		case <-c.notify:
		case <-time.After(10 * time.Second):
			t.Fatal("no timer was armed within 10s; the step never reached its timeout")
		}
	}
}
