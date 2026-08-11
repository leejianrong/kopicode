package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// tree is a small repository: two packages, a test file, a nested directory,
// and a .git that a recursive walk must not enter.
func tree(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, map[string]string{
		"README.md":                  "# demo\n",
		"main.go":                    "package main\n",
		"internal/a/a.go":            "package a\n\nfunc A() error { return nil }\n",
		"internal/a/a_test.go":       "package a\n",
		"internal/b/b.go":            "package b\n\nfunc B() error { return nil }\n",
		".git/HEAD":                  "ref: refs/heads/main\n",
		".git/objects/aa/deadbeef":   "junk\n",
		".kopicode/sessions/x.jsonl": "{}\n",
	})
}

func list(t *testing.T, s *tools.Set, req tools.ListRequest) string {
	t.Helper()
	out, err := s.ListDir(context.Background(), req)
	if err != nil {
		t.Fatalf("ListDir(%+v): %v", req, err)
	}
	return out
}

func TestListDir(t *testing.T) {
	f := tree(t)
	s := f.set(t)

	out := list(t, s, tools.ListRequest{})
	head := lines(out)[0]
	if !strings.HasPrefix(head, ".: 5 entries") {
		t.Errorf("header = %q", head)
	}
	// A non-recursive listing hides nothing, including the dot directories.
	for _, want := range []string{".git/", ".kopicode/", "internal/", "main.go", "README.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
	// Sizes are present for regular files and absent for everything else.
	for _, l := range lines(out)[1:] {
		fields := strings.Fields(l)
		if strings.HasSuffix(fields[1], "/") && fields[0] != "-" {
			t.Errorf("directory line carries a size: %q", l)
		}
	}
}

func TestListDirRecursiveExcludesTheHarnessAndGit(t *testing.T) {
	f := tree(t)
	s := f.set(t)

	out := list(t, s, tools.ListRequest{Recursive: true})
	if strings.Contains(out, "HEAD") || strings.Contains(out, "deadbeef") {
		t.Errorf("recursive listing walked into .git:\n%s", out)
	}
	if strings.Contains(out, "sessions") {
		t.Errorf("recursive listing walked into .kopicode:\n%s", out)
	}
	// The exclusion is stated, not silent.
	if head := lines(out)[0]; !strings.Contains(head, "excluding .git and .kopicode") {
		t.Errorf("header %q does not state the exclusion", head)
	}
	for _, want := range []string{"internal/a/a.go", "internal/b/b.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
}

// TestListDirFindsFilesByName is the load-bearing test for SLICE-1's decision
// to drop a separate glob tool: if this fails, a model has no way to find a
// file it only knows the name of, and the tool set has a hole in it.
func TestListDirFindsFilesByName(t *testing.T) {
	f := tree(t)
	s := f.set(t)

	cases := []struct {
		pattern string
		want    []string
		absent  []string
	}{
		{"*_test.go", []string{"internal/a/a_test.go"}, []string{"internal/a/a.go"}},
		{"**/*_test.go", []string{"internal/a/a_test.go"}, []string{"internal/a/a.go"}},
		{"*.go", []string{"main.go", "internal/a/a.go", "internal/b/b.go"}, []string{"README.md"}},
		{"internal/*/*.go", []string{"internal/a/a.go"}, []string{"main.go"}},
		{"b.go", []string{"internal/b/b.go"}, []string{"internal/a/a.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			out := list(t, s, tools.ListRequest{Recursive: true, Pattern: tc.pattern})
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("unexpected %q:\n%s", absent, out)
				}
			}
			if head := lines(out)[0]; !strings.Contains(head, tc.pattern) {
				t.Errorf("header %q does not name the pattern", head)
			}
		})
	}
}

func TestListDirStatesItsBound(t *testing.T) {
	f := tree(t)
	s := f.set(t)
	s.Limits.MaxEntries = 2

	out := list(t, s, tools.ListRequest{Recursive: true})
	notice := lines(out)[1]
	for _, want := range []string{"showing the first 2", "max_entries=2"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not mention %q", notice, want)
		}
	}
	if got := len(lines(out)) - 2; got != 2 {
		t.Errorf("printed %d entries, want 2", got)
	}
	// The header still reports how many there were, so a bounded listing is
	// not mistaken for a small directory.
	if head := lines(out)[0]; !strings.Contains(head, ": 8 entries") {
		t.Errorf("header %q should report the true total", head)
	}
}

func TestListDirRefusals(t *testing.T) {
	f := tree(t)
	s := f.set(t)
	ctx := context.Background()

	t.Run("a file points at read_file", func(t *testing.T) {
		_, err := s.ListDir(ctx, tools.ListRequest{Path: "main.go"})
		if !errors.Is(err, tools.ErrNotRegular) {
			t.Fatalf("got %v, want ErrNotRegular", err)
		}
		if !strings.Contains(err.Error(), "read_file") {
			t.Errorf("message %q should say what to use instead", err)
		}
	})

	t.Run("a missing directory", func(t *testing.T) {
		_, err := s.ListDir(ctx, tools.ListRequest{Path: "nope"})
		wantFault(t, err, tools.FaultTask)
	})

	// A malformed pattern must be reported, not treated as "matches nothing":
	// an empty listing is indistinguishable from an empty directory, and that
	// is a turn spent looking in the wrong place.
	t.Run("a malformed pattern", func(t *testing.T) {
		_, err := s.ListDir(ctx, tools.ListRequest{Pattern: "[unclosed"})
		wantFault(t, err, tools.FaultTask)
		if !strings.Contains(err.Error(), "malformed") {
			t.Errorf("message = %q", err)
		}
	})

	t.Run("cancellation is internal, not the model's fault", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := s.ListDir(cancelled, tools.ListRequest{})
		wantFault(t, err, tools.FaultInternal)
	})
}
