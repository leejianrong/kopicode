package arch_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/leejianrong/kopicode"

// frontEndAllowed lists the internal packages a front end under cmd/ may import.
// Both surfaces must drive the engine through its interface only
// (docs/adr/0003-single-repo-internal-engine.md decision 3).
//
// internal/build is the one exception, and it is narrow by construction. It is
// a leaf value package holding the artifact's own identity: no engine
// behaviour, no policy, nothing a surface could reach past the interface to
// touch. Both front ends need it for `--version`, and printing that from the
// same call the engine journals is what stops a binary from having two answers
// to "who are you". TestBuildPackageIsALeaf below is what keeps the exception
// from widening — the day internal/build imports something, it stops being a
// value package and this entry stops being defensible.
var frontEndAllowed = map[string]bool{
	modulePath + "/internal/engine": true,
	modulePath + "/internal/bench":  true,
	modulePath + "/internal/build":  true,
}

// TestBuildPackageIsALeaf guards the exception above.
//
// internal/build is allowlisted for cmd/ because it depends on nothing. If it
// grew an import of internal/engine, every front end would inherit a path into
// the engine's internals through a package nobody re-reads, and ADR-0003's
// boundary would be gone without a single line of cmd/ changing.
func TestBuildPackageIsALeaf(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "build")

	for file, imports := range internalImports(t, dir) {
		rel, err := filepath.Rel(repoRoot(t), file)
		if err != nil {
			rel = file
		}
		for _, imp := range imports {
			t.Errorf(
				"%s imports %s\n"+
					"internal/build must import nothing from this module: it is allowlisted\n"+
					"for cmd/ precisely because it is a leaf, and an import here turns that\n"+
					"allowance into a path from a front end into the engine\n"+
					"see docs/adr/0003-single-repo-internal-engine.md decision 3",
				rel, imp,
			)
		}
	}
}

// repoRoot resolves the module root relative to this test's package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	return root
}

// internalImports returns every module-internal import found under dir, keyed by
// the file that imported it.
func internalImports(t *testing.T, dir string) map[string][]string {
	t.Helper()
	found := map[string][]string{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(p, modulePath+"/internal/") {
				found[path] = append(found[path], p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return found
}

// TestFrontEndsOnlyImportEngineInterface is the guard for ADR-0003's boundary.
// Go's internal/ rule stops importers outside the module; nothing but this test
// stops cmd/ from reaching past the engine interface into its internals.
func TestFrontEndsOnlyImportEngineInterface(t *testing.T) {
	cmdDir := filepath.Join(repoRoot(t), "cmd")

	for file, imports := range internalImports(t, cmdDir) {
		rel, err := filepath.Rel(repoRoot(t), file)
		if err != nil {
			rel = file
		}
		for _, imp := range imports {
			if !frontEndAllowed[imp] {
				t.Errorf(
					"%s imports %s\n"+
						"front ends may only import the engine interface: %s\n"+
						"see docs/adr/0003-single-repo-internal-engine.md decision 3",
					rel, imp, strings.Join(allowedList(), ", "),
				)
			}
		}
	}
}

// TestEngineDoesNotImportSurfaces keeps the dependency arrow pointing one way.
// The engine decides policy; the surfaces decide presentation. An engine that
// imports a surface has started deciding presentation.
func TestEngineDoesNotImportSurfaces(t *testing.T) {
	internalDir := filepath.Join(repoRoot(t), "internal")

	err := filepath.WalkDir(internalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, spec := range f.Imports {
			p, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil {
				return unquoteErr
			}
			if strings.HasPrefix(p, modulePath+"/cmd/") {
				rel, relErr := filepath.Rel(repoRoot(t), path)
				if relErr != nil {
					rel = path
				}
				t.Errorf("%s imports the surface package %s — the engine must not depend on a front end", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", internalDir, err)
	}
}

// wireContractPairs are packages that agree on a set of strings and must not
// agree on a dependency edge.
//
// internal/journal records classifications internal/parse produces — the
// extraction route, the failure kind, and the argument encoding — as plain
// strings, and both sides say in their doc comments that this is deliberate:
// "internal/parse must not import this package and this package must not import
// it, so the coupling is a documented wire contract instead of a dependency
// edge" (journal.ToolCallRepaired.Classification). Until KAN-838 that rule was
// prose on both sides and enforced on neither.
//
// The direction that matters most is parse importing journal, because it is the
// tempting one: a package that produces events wants to emit them. The engine
// journals; packages return data (CLAUDE.md, "Boundaries that must not be
// crossed"). The reverse direction is banned too, so that "just for a test" does
// not make journal depend on the taxonomy it is supposed to outlive — an old
// session must stay readable after parse renames a Kind, which it cannot do if
// the reader compiles against the new one.
// internal/provider is the second pair, for the same reason and with one
// difference. The wire format, the journal and the fixtures declare the pin, the
// token counts and the sampling parameters three times over, and each says in
// its doc comment that this is deliberate — "this package must not import the
// journal (the engine journals; packages return data)". KAN-776 added a client
// to that package, which is the moment the temptation becomes concrete: a thing
// that makes provider calls wants to record them.
//
// The difference is test files. internal/provider's shape_test.go imports the
// journal on purpose — it is the third party that holds the three declarations
// to each other, exactly as internal/provider/mock's test is — so banning the
// import there would delete the check that keeps the shapes from drifting. So
// this pair is enforced over product code only, and
// TestTheWireContractWalkerDistinguishesTestFiles below asserts that the
// distinction is real rather than assumed.
var wireContractPairs = []wireContractPair{
	{a: "internal/parse", b: "internal/journal", includeTests: true},
	{a: "internal/provider", b: "internal/journal", includeTests: false},
}

// wireContractPair is two packages that share a vocabulary of strings and must
// not share a dependency edge.
type wireContractPair struct {
	a, b string
	// includeTests extends the ban to _test.go files. It is on where the
	// vocabulary is the whole coupling (parse and journal have no legitimate
	// third-party test between them) and off where a test is the mechanism that
	// holds the two shapes equal.
	includeTests bool
}

// packageImports returns the module-internal imports of the Go files directly in
// dir, keyed by file. Unlike internalImports it does not descend.
//
// The distinction matters: internal/provider has sub-packages, and one of them —
// internal/provider/mock — legitimately imports the journal. A recursive scan
// would report that as a violation of the parent's rule and there would be no
// way to state the rule at all.
func packageImports(t *testing.T, dir string, includeTests bool) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}

	found := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if !includeTests && strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquoting an import in %s: %v", path, err)
			}
			if strings.HasPrefix(p, modulePath+"/internal/") {
				found[path] = append(found[path], p)
			}
		}
	}
	return found
}

// TestTheWireContractWalkerDistinguishesTestFiles is the positive control for
// the product-code-only pair above.
//
// internal/provider's own test imports the journal — that is the shape-agreement
// test, and it is supposed to. So the walker must find that import when it is
// asked to include test files and must not find it when it is not. A walker that
// returned nothing either way would let TestWireContractPairsDoNotImportEachOther
// pass over a product file that imported the journal on every line.
func TestTheWireContractWalkerDistinguishesTestFiles(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "provider")
	banned := modulePath + "/internal/journal"

	var withTests bool
	for _, imports := range packageImports(t, dir, true) {
		for _, imp := range imports {
			if imp == banned {
				withTests = true
			}
		}
	}
	if !withTests {
		t.Fatalf("the walker found no import of %s under internal/provider even with test files "+
			"included, but shape_test.go imports it — the traversal is broken and the guard below "+
			"is asserting nothing", banned)
	}

	for file, imports := range packageImports(t, dir, false) {
		for _, imp := range imports {
			if imp == banned {
				t.Fatalf("%s is a product file importing %s; the walker's test-file filter is not "+
					"doing what the guard below assumes", file, banned)
			}
		}
	}
}

// TestWireContractPairsDoNotImportEachOther guards that rule statically.
//
// It covers test files as well as product code. Reading the other package's
// types to check the shapes line up is fine; importing is not, and a
// `parse_test` that imported journal would compile, pass, and quietly turn a
// wire contract into a dependency.
func TestWireContractPairsDoNotImportEachOther(t *testing.T) {
	// Positive control. internal/provider/mock's test legitimately imports both
	// — it is the third party that maps one onto the other — so if the walker
	// cannot see those imports it cannot see a violation either, and the checks
	// below would pass over anything.
	control := map[string]bool{}
	for _, imports := range packageImports(t, filepath.Join(repoRoot(t), "internal", "provider", "mock"), true) {
		for _, imp := range imports {
			control[imp] = true
		}
	}
	for _, pkg := range []string{"internal/parse", "internal/journal"} {
		if !control[modulePath+"/"+pkg] {
			t.Fatalf("positive control failed: the import walker did not find %s under "+
				"internal/provider/mock, which does import it — the traversal is broken and "+
				"the guard below is asserting nothing", pkg)
		}
	}

	for _, pair := range wireContractPairs {
		sides := [2]string{pair.a, pair.b}
		for i, pkg := range sides {
			banned := modulePath + "/" + sides[1-i]
			dir := filepath.Join(repoRoot(t), filepath.FromSlash(pkg))

			for file, imports := range packageImports(t, dir, pair.includeTests) {
				rel, err := filepath.Rel(repoRoot(t), file)
				if err != nil {
					rel = file
				}
				for _, imp := range imports {
					if imp != banned {
						continue
					}
					t.Errorf(
						"%s imports %s\n"+
							"%s and %s share a vocabulary of strings, not a dependency edge:\n"+
							"the engine journals, packages return data, and an old session has to stay\n"+
							"readable after the taxonomy on the other side is renamed\n"+
							"see CLAUDE.md \"Boundaries that must not be crossed\" and the comment on\n"+
							"journal.ToolCallRepaired.Classification",
						rel, imp, pkg, sides[1-i],
					)
				}
			}
		}
	}
}

func allowedList() []string {
	out := make([]string, 0, len(frontEndAllowed))
	for k := range frontEndAllowed {
		out = append(out, strings.TrimPrefix(k, modulePath+"/"))
	}
	return out
}
