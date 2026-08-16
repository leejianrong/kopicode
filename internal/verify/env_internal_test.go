package verify

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestTheStripListCoversTheCredential is the one entry that is not about the
// correctness of a verdict. CLAUDE.md: the provider key appears in no journal,
// no blob and no log — and this subprocess's output is journaled verbatim, so
// the cheapest guarantee is that the process which could print it never has it.
func TestTheStripListCoversTheCredential(t *testing.T) {
	t.Setenv(APIKeyEnv, "sk-not-a-real-key")

	v := &Verifier{Root: t.TempDir()}
	for _, kv := range v.baseEnv() {
		if strings.HasPrefix(kv, APIKeyEnv+"=") {
			t.Fatalf("a verification command would inherit %s", APIKeyEnv)
		}
	}
}

// TestTheStripListCoversWhatChangesAVerdict is the specification half of the
// environment rule, and it exists because a dynamic test cannot be it.
//
// "Everything on the strip list is stripped" is true of an empty strip list, so
// an implementation and its assertion can move together and stay green. This is
// a hand-written list on purpose: it is the specification, and `stripped` is the
// implementation being held to it. Removing a variable from the strip list fails
// here; adding one does not require touching this table, which is the asymmetry
// you want.
func TestTheStripListCoversWhatChangesAVerdict(t *testing.T) {
	must := []string{
		APIKeyEnv,

		// The class that has already corrupted this repository once. GIT_DIR
		// beats a correctly-set working directory, so a suite that shells out to
		// git would act on whatever the ambient environment names.
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM",

		// Each of these changes what the suite concludes about a tree that has
		// not changed, which is the definition of a variable a measurement
		// instrument must not inherit.
		"GOFLAGS", "GOOS", "GOARCH", "CGO_ENABLED", "GOWORK",
		"PYTHONPATH", "PYTHONHOME",
		"NODE_OPTIONS", "NODE_PATH",
	}

	for _, name := range must {
		if !slices.ContainsFunc(stripped, func(s struct{ name, why string }) bool {
			return s.name == name
		}) {
			t.Errorf("%s is not stripped from a verification command's environment; inheriting it "+
				"makes the verdict depend on whoever's shell started kopicode", name)
		}
	}
	for _, s := range stripped {
		if s.why == "" {
			t.Errorf("%s is stripped with no reason recorded", s.name)
		}
	}
}

// TestTheEnvironmentIsNotAnAllowlist is the negative control for the rule above,
// and it is the failure that would not be noticed: a verification command that
// cannot find its own toolchain reports not_run on every turn, which is honest
// and useless.
func TestTheEnvironmentIsNotAnAllowlist(t *testing.T) {
	t.Setenv("KOPICODE_A_VARIABLE_NOTHING_STRIPS", "kept")

	v := &Verifier{Root: t.TempDir()}
	if !slices.Contains(v.baseEnv(), "KOPICODE_A_VARIABLE_NOTHING_STRIPS=kept") {
		t.Error("baseEnv dropped a variable that is not on the strip list; PATH, HOME, TMPDIR, " +
			"GOMODCACHE and a dozen distribution-specific variables all have to survive or the " +
			"project's own suite does not run at all")
	}
}

// TestOnlyOneSubprocessCallSite is the structural half of the subprocess rule.
//
// internal/arch checks statically that every exec.Command in the tree assigns
// Dir and Env and that the Env is not the ambient one. What it cannot check is
// that a second call site got the process group, the timeout and the graceful
// kill right too. Keeping there to be exactly one is how those four decisions
// stay in one place a reviewer can read.
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

	if len(sites) == 0 {
		t.Fatal("positive control failed: the walk found no exec.Command at all, so it is " +
			"asserting nothing about a package whose whole job is to run one")
	}
	if len(sites) != 1 {
		t.Errorf("this package starts a subprocess in %d places: %s\n"+
			"every verification goes through Verifier.exec, so a second call site is a second "+
			"place that can forget Dir, Env, the process group or the timeout",
			len(sites), strings.Join(sites, ", "))
	}
}
