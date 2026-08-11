package arch_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryLdflagsTargetExists is a guard against a silent no-op.
//
// `go build -ldflags '-X some/pkg.name=v'` does not fail when some/pkg.name is
// not a string variable in the link graph. It does nothing at all, and the
// binary ships with the zero value. For KAN-806 that failure mode is precisely
// the one being designed out: every session would report the same fabricated
// build identity, and two incomparable arms would pool because their identities
// matched — a guard that lies is worse than no guard.
//
// It happened here already, in the mild form. `-X main.version` targeted two
// stub main packages, and the flag was carried through every build for weeks
// while proving nothing about the code that would eventually need it.
//
// The check is static rather than a build: it reads the Makefile, expands the
// variables in each -X target, and requires the named symbol to be a
// package-level string variable in this module. It fails closed — a target it
// cannot resolve is a failure, not a skip.
func TestEveryLdflagsTargetExists(t *testing.T) {
	root := repoRoot(t)
	makefile := filepath.Join(root, "Makefile")

	src, err := os.ReadFile(makefile)
	if err != nil {
		t.Fatalf("reading %s: %v", makefile, err)
	}

	targets := ldflagsTargets(string(src))
	if len(targets) == 0 {
		t.Fatal("no -X targets found in the Makefile — either the build identity injection " +
			"was removed (KAN-806, ADR-0007 decision 7) or this guard has stopped reading it")
	}

	vars := makeVars(string(src))
	for _, raw := range targets {
		target, ok := expandMakeVars(raw, vars)
		if !ok {
			t.Errorf("-X target %q could not be expanded from the Makefile's variables; "+
				"this guard cannot check what it cannot resolve", raw)
			continue
		}
		for _, dir := range targetDirs(t, root, target) {
			name := symbolName(target)
			if !hasStringVar(t, dir, name) {
				rel, relErr := filepath.Rel(root, dir)
				if relErr != nil {
					rel = dir
				}
				t.Errorf("-X %s targets %q in %s, which declares no package-level string "+
					"variable of that name\n"+
					"the linker does not report this: it sets nothing and the binary ships "+
					"with the zero value",
					target, name, rel)
			}
		}
	}
}

// xTarget matches the symbol half of an -X flag, i.e. everything up to the '='.
var xTarget = regexp.MustCompile(`-X\s+'?([^\s'=]+)=`)

func ldflagsTargets(makefile string) []string {
	var out []string
	for _, m := range xTarget.FindAllStringSubmatch(makefile, -1) {
		out = append(out, m[1])
	}
	return out
}

// makeAssign matches a simple `NAME := value` or `NAME ?= value` line. Recipes
// are indented with a tab, so requiring the name at column zero keeps shell
// lines out.
var makeAssign = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\s*[:?]?=\s*(.*)$`)

func makeVars(makefile string) map[string]string {
	vars := map[string]string{}
	for _, m := range makeAssign.FindAllStringSubmatch(makefile, -1) {
		vars[m[1]] = strings.TrimSpace(m[2])
	}
	return vars
}

// expandMakeVars resolves $(NAME) references. Only the symbol half of an -X
// flag is ever passed here, so nothing that reaches it should expand to a
// $(shell ...) — and if it does, reporting failure is correct.
func expandMakeVars(s string, vars map[string]string) (string, bool) {
	for range 10 {
		if !strings.Contains(s, "$(") {
			return s, true
		}
		before := s
		for name, value := range vars {
			if strings.Contains(value, "$(shell") {
				continue
			}
			s = strings.ReplaceAll(s, "$("+name+")", value)
		}
		if s == before {
			return s, false
		}
	}
	return s, false
}

// symbolName is the variable name at the end of an -X target.
func symbolName(target string) string {
	last := target[strings.LastIndex(target, "/")+1:]
	if i := strings.Index(last, "."); i >= 0 {
		return last[i+1:]
	}
	return ""
}

// targetDirs resolves an -X import path to the directories that must declare
// the symbol.
//
// A bare `main.name` target is not an error — the linker applies it to whatever
// main package is being built — but it is only meaningful if every main package
// declares it, so it resolves to all of them. That is the completeness check
// the old `-X main.version` needed and never had.
func targetDirs(t *testing.T, root, target string) []string {
	t.Helper()

	slash := strings.LastIndex(target, "/")
	last := target[slash+1:]
	dot := strings.Index(last, ".")
	if dot < 0 {
		t.Errorf("-X target %q has no package.symbol form", target)
		return nil
	}
	importPath := target[:slash+1] + last[:dot]

	if importPath == "main" {
		entries, err := os.ReadDir(filepath.Join(root, "cmd"))
		if err != nil {
			t.Fatalf("reading cmd/: %v", err)
		}
		var dirs []string
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, "cmd", e.Name()))
			}
		}
		return dirs
	}

	if !strings.HasPrefix(importPath, modulePath+"/") {
		t.Errorf("-X target %q names %q, which is not a package in this module",
			target, importPath)
		return nil
	}
	return []string{filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(importPath, modulePath+"/")))}
}

// hasStringVar reports whether dir declares a package-level `var name string`.
// Test files are excluded: a variable the linker can only reach in a test
// binary is not one the shipped artifact carries.
func hasStringVar(t *testing.T, dir, name string) bool {
	t.Helper()
	if name == "" {
		return false
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Errorf("reading %s: %v", dir, err)
		return false
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if parseErr != nil {
			t.Errorf("parsing %s: %v", e.Name(), parseErr)
			continue
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				ident, ok := vs.Type.(*ast.Ident)
				if !ok || ident.Name != "string" {
					continue
				}
				for _, n := range vs.Names {
					if n.Name == name {
						return true
					}
				}
			}
		}
	}
	return false
}
