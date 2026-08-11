package tools_test

import (
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// resolverFixture builds a repository with a symlink out of it, and returns the
// adapter together with the two real directory paths expectations are written
// against. Both come back symlink-resolved, because t.TempDir sits under /tmp,
// which is itself a symlink on macOS.
func resolverFixture(t *testing.T) (tools.Resolver, string, string) {
	t.Helper()
	f := newFixture(t, map[string]string{"main.go": "package main\n"})
	f.symlink(t, f.outside, "escape")

	root, err := tools.OpenRoot(f.root)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", f.root, err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	outside, err := filepath.EvalSymlinks(f.outside)
	if err != nil {
		t.Fatalf("resolving %s: %v", f.outside, err)
	}
	return root.Resolver(), root.Path(), outside
}

// TestResolverResolvesWithoutContaining is the whole difference between this
// adapter and [tools.Root.Resolve], and the reason the adapter exists.
//
// Resolve refuses a path outside the root with ErrOutsideRoot, which is right
// for a tool that is about to open a file. The permission gate is not opening
// anything: it has to know *where* a write landed in order to decide that it
// landed outside and must be asked about (SLICE-1 §10). An adapter that refused
// instead of answering would turn that rule into a silent denial.
func TestResolverResolvesWithoutContaining(t *testing.T) {
	resolver, root, outside := resolverFixture(t)

	tests := []struct {
		name string
		path string
		want string
	}{
		{"relative, inside", "main.go", filepath.Join(root, "main.go")},
		{"absolute, inside", filepath.Join(root, "main.go"), filepath.Join(root, "main.go")},
		{"the root itself", root, root},
		{"a file that does not exist yet", "new/deep/file.go", filepath.Join(root, "new", "deep", "file.go")},
		{"relative, out through dot dot", filepath.Join("..", "outside", "secret.txt"), filepath.Join(outside, "secret.txt")},
		{"out through a symlink", filepath.Join("escape", "secret.txt"), filepath.Join(outside, "secret.txt")},
		{"absolute, elsewhere entirely", outside, outside},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolver.Resolve(tc.path)
			if err != nil {
				t.Fatalf("Resolve(%q) failed: %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestResolverRefusesWhatItCannotAnswerFor: the two cases where returning a
// path would be worse than returning an error, because both would look
// contained to whoever is judging.
func TestResolverRefusesWhatItCannotAnswerFor(t *testing.T) {
	resolver, _, _ := resolverFixture(t)

	t.Run("empty path", func(t *testing.T) {
		if got, err := resolver.Resolve(""); err == nil {
			t.Fatalf(`Resolve("") = %q, want an error`, got)
		}
	})

	t.Run("zero resolver", func(t *testing.T) {
		if got, err := (tools.Resolver{}).Resolve("main.go"); err == nil {
			t.Fatalf("the zero Resolver returned %q; it has no root and must refuse", got)
		}
	})
}

// TestResolverAgreesWithResolveInsideTheRoot: the two share one absolutisation
// and one link walk, and this is what says so. Where Root.Resolve accepts a
// path, the adapter must produce the same absolute path — a divergence here
// would mean the gate judged one path and the tool opened another.
func TestResolverAgreesWithResolveInsideTheRoot(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": "package main\n", "pkg/x.go": "package pkg\n"})
	f.symlink(t, f.outside, "escape")

	root, err := tools.OpenRoot(f.root)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", f.root, err)
	}
	t.Cleanup(func() { _ = root.Close() })

	for _, given := range []string{"main.go", "pkg/x.go", "pkg/../main.go", "new/file.go", "."} {
		t.Run(given, func(t *testing.T) {
			p, err := root.Resolve(tools.ToolReadFile, given)
			if err != nil {
				t.Fatalf("Root.Resolve(%q) failed: %v", given, err)
			}
			got, err := root.Resolver().Resolve(given)
			if err != nil {
				t.Fatalf("Resolver.Resolve(%q) failed: %v", given, err)
			}
			if got != p.Abs {
				t.Errorf("Resolver.Resolve(%q) = %q, but Root.Resolve gave %q", given, got, p.Abs)
			}
		})
	}
}
