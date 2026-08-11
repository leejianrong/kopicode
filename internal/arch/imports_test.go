package arch_test

import (
	"go/parser"
	"go/token"
	"io/fs"
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

func allowedList() []string {
	out := make([]string, 0, len(frontEndAllowed))
	for k := range frontEndAllowed {
		out = append(out, strings.TrimPrefix(k, modulePath+"/"))
	}
	return out
}
