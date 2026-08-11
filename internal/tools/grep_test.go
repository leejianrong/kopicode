package tools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/tools"
)

func grep(t *testing.T, s *tools.Set, req tools.GrepRequest) string {
	t.Helper()
	out, err := s.Grep(context.Background(), req)
	if err != nil {
		t.Fatalf("Grep(%+v): %v", req, err)
	}
	return out
}

func TestGrep(t *testing.T) {
	f := tree(t)
	s := f.set(t)

	out := grep(t, s, tools.GrepRequest{Pattern: `func \w+\(\) error`})
	if head := lines(out)[0]; !strings.HasPrefix(head, `grep "func \\w+\\(\\) error": 2 matches in 2 files under the repository root`) {
		t.Errorf("header = %q", head)
	}
	for _, want := range []string{
		"internal/a/a.go:3: func A() error { return nil }",
		"internal/b/b.go:3: func B() error { return nil }",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	// .git and .kopicode are not searched, for the same reason list_dir does
	// not walk them.
	if strings.Contains(out, ".git/") || strings.Contains(out, ".kopicode/") {
		t.Errorf("grep searched the excluded directories:\n%s", out)
	}
}

// TestGrepEmitsNoAnchors is the ADR-0006 guarantee, asserted as a property of
// the output rather than an intention in a comment. Anchors are obtainable only
// from a read; that is what makes an edit into a region the model was never
// shown structurally impossible, and anchoring grep hits would quietly downgrade
// it to a convention.
func TestGrepEmitsNoAnchors(t *testing.T) {
	f := tree(t)
	s := f.set(t)

	// Take the real anchors for the file, then require that none of them
	// appears anywhere in grep's output.
	src := "package a\n\nfunc A() error { return nil }\n"
	anchors := anchor.Derive(anchor.Split([]byte(src)))

	out := grep(t, s, tools.GrepRequest{Pattern: "func A"})
	for i, a := range anchors {
		if strings.Contains(out, a) {
			t.Errorf("grep output carries the anchor for line %d (%s):\n%s", i+1, a, out)
		}
	}
}

// TestGrepLineNumbersMatchReadFile is what makes a grep hit actionable: the
// model greps, then reads at that offset, and the two had better agree about
// which line is which. They do because both split through anchor.Split.
func TestGrepLineNumbersMatchReadFile(t *testing.T) {
	f := newFixture(t, map[string]string{
		"crlf.go": strings.ReplaceAll(sample, "\n", "\r\n"),
	})
	s := f.set(t)

	out := grep(t, s, tools.GrepRequest{Pattern: "hello"})
	hit := lines(out)[1]
	var line int
	if _, err := fmt.Sscanf(hit, "crlf.go:%d:", &line); err != nil {
		t.Fatalf("could not read a line number out of %q: %v", hit, err)
	}

	read := read(t, s, tools.ReadRequest{Path: "crlf.go", Offset: line, Limit: 1})
	if !strings.Contains(read, "hello") {
		t.Errorf("read_file at the line grep reported does not hold the match:\n%s", read)
	}
	// And the reported text has no terminator clinging to it.
	if strings.Contains(hit, "\r") {
		t.Errorf("grep leaked a CR into its output: %q", hit)
	}
}

func TestGrepFilters(t *testing.T) {
	f := tree(t)
	s := f.set(t)

	t.Run("include narrows to a name pattern", func(t *testing.T) {
		out := grep(t, s, tools.GrepRequest{Pattern: "package", Include: "*_test.go"})
		if !strings.Contains(out, "internal/a/a_test.go:1:") {
			t.Errorf("missing the test file:\n%s", out)
		}
		if strings.Contains(out, "a/a.go:") {
			t.Errorf("include did not exclude the non-test file:\n%s", out)
		}
		if !strings.Contains(lines(out)[0], `including "*_test.go"`) {
			t.Errorf("header does not state the filter: %q", lines(out)[0])
		}
	})

	t.Run("a path narrows the search", func(t *testing.T) {
		out := grep(t, s, tools.GrepRequest{Pattern: "package", Path: "internal/b"})
		if strings.Contains(out, "internal/a") {
			t.Errorf("search left the given path:\n%s", out)
		}
		if !strings.Contains(lines(out)[0], "under internal/b") {
			t.Errorf("header does not name the scope: %q", lines(out)[0])
		}
	})

	t.Run("a single file is searched directly", func(t *testing.T) {
		out := grep(t, s, tools.GrepRequest{Pattern: "package", Path: "main.go"})
		if !strings.Contains(lines(out)[0], "in main.go") {
			t.Errorf("header = %q", lines(out)[0])
		}
		if !strings.Contains(out, "main.go:1: package main") {
			t.Errorf("missing the match:\n%s", out)
		}
	})

	t.Run("ignore case", func(t *testing.T) {
		if out := grep(t, s, tools.GrepRequest{Pattern: "PACKAGE MAIN"}); !strings.Contains(out, "no matches") {
			t.Fatalf("case-sensitive search matched:\n%s", out)
		}
		out := grep(t, s, tools.GrepRequest{Pattern: "PACKAGE MAIN", IgnoreCase: true})
		if !strings.Contains(out, "main.go:1:") {
			t.Errorf("case-insensitive search missed it:\n%s", out)
		}
	})

	t.Run("no matches says so rather than returning nothing", func(t *testing.T) {
		out := grep(t, s, tools.GrepRequest{Pattern: "zzz-nothing-here"})
		if !strings.Contains(out, "no matches") {
			t.Errorf("got %q", out)
		}
	})
}

func TestGrepStatesItsMatchBound(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": strings.Repeat("needle\n", 10)})
	s := f.set(t)
	s.Limits.MaxMatches = 4

	out := grep(t, s, tools.GrepRequest{Pattern: "needle"})
	if got := len(lines(out)) - 2; got != 4 {
		t.Errorf("printed %d matches, want 4:\n%s", got, out)
	}
	for _, want := range []string{"max_matches=4", "more matches remain"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not state %q:\n%s", want, out)
		}
	}
	// The header counts what is printed, never a total the output does not
	// contain.
	if head := lines(out)[0]; !strings.Contains(head, "4 matches") {
		t.Errorf("header = %q", head)
	}
}

// TestGrepStatesWhatItSkipped covers the files grep declines to read. A file
// silently omitted from a search is worse than one reported as unsearchable:
// the model concludes the string is not there.
func TestGrepStatesWhatItSkipped(t *testing.T) {
	f := newFixture(t, map[string]string{
		"a.txt":   "needle\n",
		"big.txt": "needle\n" + strings.Repeat("x\n", 200),
		"b.bin":   "needle\x00\x00",
	})
	s := f.set(t)
	s.Limits.MaxFileBytes = 128

	out := grep(t, s, tools.GrepRequest{Pattern: "needle"})
	for _, want := range []string{
		"1 match in 1 file",
		"skipped 1 binary file and 1 file over max_file_bytes=128",
		"a.txt:1: needle",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not state %q:\n%s", want, out)
		}
	}
}

func TestGrepRefusals(t *testing.T) {
	f := tree(t)
	s := f.set(t)
	ctx := context.Background()

	t.Run("an uncompilable pattern", func(t *testing.T) {
		_, err := s.Grep(ctx, tools.GrepRequest{Pattern: "func ("})
		wantFault(t, err, tools.FaultTask)
		if !strings.Contains(err.Error(), "not a valid regular expression") {
			t.Errorf("message = %q", err)
		}
	})

	t.Run("no pattern at all", func(t *testing.T) {
		_, err := s.Grep(ctx, tools.GrepRequest{})
		wantFault(t, err, tools.FaultTask)
	})

	t.Run("a malformed include", func(t *testing.T) {
		_, err := s.Grep(ctx, tools.GrepRequest{Pattern: "x", Include: "[unclosed"})
		wantFault(t, err, tools.FaultTask)
	})

	t.Run("cancellation is nobody's fault", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := s.Grep(cancelled, tools.GrepRequest{Pattern: "package"})
		wantFault(t, err, tools.FaultCancelled)
	})
}
