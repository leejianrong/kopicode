package tools_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/anchor"
	"github.com/leejianrong/kopicode/internal/tools"
)

const sample = `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`

// read is the call under test, spelled once.
func read(t *testing.T, s *tools.Set, req tools.ReadRequest) string {
	t.Helper()
	out, err := s.ReadFile(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadFile(%+v): %v", req, err)
	}
	return out
}

// body drops the header (and any bound notice) and returns the rendered lines.
func body(t *testing.T, out string) []string {
	t.Helper()
	all := lines(out)
	for i, l := range all {
		if isRendered(l) {
			return all[i:]
		}
	}
	return nil
}

// isRendered reports whether a line looks like anchor.Render's output: eight
// lowercase hex characters, a space, then a right-aligned number and a bar.
func isRendered(l string) bool {
	if len(l) < anchor.Length+2 {
		return false
	}
	for _, c := range l[:anchor.Length] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return l[anchor.Length] == ' ' && strings.Contains(l, "|")
}

// anchorsOf extracts the anchor column, which is the only field the model is
// asked to copy back and therefore the only one edit_file will resolve.
func anchorsOf(t *testing.T, out string) []string {
	t.Helper()
	rendered := body(t, out)
	got := make([]string, len(rendered))
	for i, l := range rendered {
		got[i] = l[:anchor.Length]
	}
	return got
}

// --- the rendering contract -----------------------------------------------

// TestRenderIsAnchorsRendering pins read_file to anchor.Render rather than to a
// second formatter. A copy here would drift from the derivation and silently
// stop matching recorded fixtures — ADR-0006 §7 keeps both halves in one
// package for exactly this reason.
func TestRenderIsAnchorsRendering(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": sample})
	s := f.set(t)

	out := read(t, s, tools.ReadRequest{Path: "main.go"})
	want := anchor.Render(anchor.Split([]byte(sample)))
	got := body(t, out)

	if len(got) != len(want) {
		t.Fatalf("got %d rendered lines, want %d:\n%s", len(got), len(want), out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d: got %q, want %q", i+1, got[i], want[i])
		}
	}
	if header := lines(out)[0]; header != "main.go: 7 lines, 66 bytes" {
		t.Errorf("header = %q", header)
	}
}

// --- the card's acceptance criterion --------------------------------------

// TestAnchorsAreStableAcrossReads is KAN-780's acceptance criterion and
// SLICE-1's Test Plan line, asserted through the tool because the tool is what
// the model sees. The derivation's own properties are internal/anchor's tests.
func TestAnchorsAreStableAcrossReads(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": sample})
	s := f.set(t)

	first := read(t, s, tools.ReadRequest{Path: "main.go"})
	for i := range 3 {
		if got := read(t, s, tools.ReadRequest{Path: "main.go"}); got != first {
			t.Fatalf("read %d differs from the first:\n%s\n---\n%s", i+2, first, got)
		}
	}

	// A second Set over the same root is a fresh process's worth of state. An
	// anchor that only survives within one session is not an addressing scheme.
	other, err := tools.NewSet(f.root)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	defer other.Close()
	if got := read(t, other, tools.ReadRequest{Path: "main.go"}); got != first {
		t.Errorf("a second Set read differently:\n%s\n---\n%s", first, got)
	}
}

// TestAnchorsChangeWhenTheLineChanges is the other half of the criterion. The
// changed line and its immediate neighbours must move — that is the ±1 window
// ADR-0006 §7 chose — and nothing further away may.
func TestAnchorsChangeWhenTheLineChanges(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": sample})
	s := f.set(t)

	before := anchorsOf(t, read(t, s, tools.ReadRequest{Path: "main.go"}))

	edited := strings.Replace(sample, `fmt.Println("hello")`, `fmt.Println("goodbye")`, 1)
	f.write(t, "main.go", edited)
	after := anchorsOf(t, read(t, s, tools.ReadRequest{Path: "main.go"}))

	if len(before) != len(after) {
		t.Fatalf("line count changed: %d then %d", len(before), len(after))
	}
	// Line 6 is the edited one; 5 and 7 are its neighbours.
	for _, i := range []int{4, 5, 6} {
		if before[i] == after[i] {
			t.Errorf("line %d: anchor %s did not change", i+1, before[i])
		}
	}
	for _, i := range []int{0, 1, 2, 3} {
		if before[i] != after[i] {
			t.Errorf("line %d: anchor moved from %s to %s, but the edit was three lines away",
				i+1, before[i], after[i])
		}
	}
}

// TestWindowAnchorsMatchTheWholeFile is the trap in bounded reads: anchors
// derived over a window would give its first and last lines anchors no other
// read reproduces, and edit_file would then reject them as drift on a file
// nothing had touched.
func TestWindowAnchorsMatchTheWholeFile(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": sample})
	s := f.set(t)

	whole := anchorsOf(t, read(t, s, tools.ReadRequest{Path: "main.go"}))
	window := anchorsOf(t, read(t, s, tools.ReadRequest{Path: "main.go", Offset: 3, Limit: 3}))

	if len(window) != 3 {
		t.Fatalf("got %d lines, want 3", len(window))
	}
	for i, got := range window {
		if want := whole[i+2]; got != want {
			t.Errorf("window line %d: anchor %s, want %s from the whole-file read", i+3, got, want)
		}
	}
}

// TestCRLFReadsLikeLF holds the cross-platform half of the contract: a fixture
// recorded on one checkout must not reject every edit on the other.
func TestCRLFReadsLikeLF(t *testing.T) {
	f := newFixture(t, map[string]string{
		"lf.go":   sample,
		"crlf.go": strings.ReplaceAll(sample, "\n", "\r\n"),
	})
	s := f.set(t)

	lf := anchorsOf(t, read(t, s, tools.ReadRequest{Path: "lf.go"}))
	crlf := anchorsOf(t, read(t, s, tools.ReadRequest{Path: "crlf.go"}))
	if len(lf) != len(crlf) {
		t.Fatalf("line counts differ: %d and %d", len(lf), len(crlf))
	}
	for i := range lf {
		if lf[i] != crlf[i] {
			t.Errorf("line %d: LF %s, CRLF %s", i+1, lf[i], crlf[i])
		}
	}
}

// --- the four decided cases -----------------------------------------------

func TestReadEmptyFile(t *testing.T) {
	f := newFixture(t, map[string]string{"empty.txt": ""})
	s := f.set(t)

	out := read(t, s, tools.ReadRequest{Path: "empty.txt"})
	if out != "empty.txt: empty file\n" {
		t.Errorf("got %q; an empty result must not look like a failed read", out)
	}
}

func TestReadBinaryFile(t *testing.T) {
	f := newFixture(t, map[string]string{
		"nul.bin":     "MZ\x00\x00binary",
		"badutf8.txt": "ok\n\xff\xfe not utf-8\n",
	})
	s := f.set(t)

	for _, name := range []string{"nul.bin", "badutf8.txt"} {
		t.Run(name, func(t *testing.T) {
			_, err := s.ReadFile(context.Background(), tools.ReadRequest{Path: name})
			if !errors.Is(err, tools.ErrBinaryFile) {
				t.Fatalf("got %v, want ErrBinaryFile", err)
			}
			wantFault(t, err, tools.FaultTask)
		})
	}
}

func TestReadOversizedFileIsRefusedNotClipped(t *testing.T) {
	f := newFixture(t, map[string]string{"big.txt": strings.Repeat("x\n", 100)})
	s := f.set(t)
	s.Limits.MaxFileBytes = 32

	_, err := s.ReadFile(context.Background(), tools.ReadRequest{Path: "big.txt"})
	if !errors.Is(err, tools.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	wantFault(t, err, tools.FaultTask)
	// The refusal has to be actionable, or the model burns the turn.
	for _, want := range []string{"200 bytes", "limit is 32", "grep"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

func TestReadStatesTheLineBound(t *testing.T) {
	f := newFixture(t, map[string]string{"ten.txt": strings.Repeat("line\n", 10)})
	s := f.set(t)
	s.Limits.MaxLines = 3

	out := read(t, s, tools.ReadRequest{Path: "ten.txt"})
	if got := len(body(t, out)); got != 3 {
		t.Errorf("got %d lines, want 3", got)
	}
	notice := lines(out)[1]
	for _, want := range []string{"showing lines 1-3 of 10", "max_lines=3", "offset=4"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice %q does not mention %q — a bound nobody stated is a truncation", notice, want)
		}
	}

	// And paging with the stated offset reaches the rest.
	rest := read(t, s, tools.ReadRequest{Path: "ten.txt", Offset: 4})
	if !strings.Contains(lines(rest)[1], "showing lines 4-6 of 10") {
		t.Errorf("paging notice = %q", lines(rest)[1])
	}
}

func TestReadLineRange(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": sample})
	s := f.set(t)

	t.Run("a window inside the file states its extent", func(t *testing.T) {
		out := read(t, s, tools.ReadRequest{Path: "main.go", Offset: 3, Limit: 2})
		if got := len(body(t, out)); got != 2 {
			t.Fatalf("got %d lines, want 2", got)
		}
		if got := lines(out)[1]; !strings.Contains(got, "showing lines 3-4 of 7") {
			t.Errorf("notice = %q", got)
		}
	})

	t.Run("a window reaching the end needs no continuation", func(t *testing.T) {
		out := read(t, s, tools.ReadRequest{Path: "main.go", Offset: 6})
		notice := lines(out)[1]
		if !strings.Contains(notice, "showing lines 6-7 of 7") {
			t.Errorf("notice = %q", notice)
		}
		if strings.Contains(notice, "offset=") {
			t.Errorf("notice offers more when there is none: %q", notice)
		}
	})

	t.Run("the whole file carries no notice", func(t *testing.T) {
		out := read(t, s, tools.ReadRequest{Path: "main.go"})
		if strings.Contains(out, "showing lines") {
			t.Errorf("an unbounded read should not claim a window:\n%s", out)
		}
	})

	t.Run("a limit above the bound is reduced and said so", func(t *testing.T) {
		s.Limits.MaxLines = 2
		defer func() { s.Limits = tools.DefaultLimits() }()
		out := read(t, s, tools.ReadRequest{Path: "main.go", Limit: 50})
		if got := lines(out)[1]; !strings.Contains(got, "max_lines=2") {
			t.Errorf("notice = %q", got)
		}
	})

	t.Run("an offset past the end names the line count", func(t *testing.T) {
		_, err := s.ReadFile(context.Background(), tools.ReadRequest{Path: "main.go", Offset: 99})
		wantFault(t, err, tools.FaultTask)
		if !strings.Contains(err.Error(), "7 line file") {
			t.Errorf("message %q should name the file's length", err)
		}
	})

	t.Run("a negative offset is refused", func(t *testing.T) {
		_, err := s.ReadFile(context.Background(), tools.ReadRequest{Path: "main.go", Offset: -1})
		wantFault(t, err, tools.FaultTask)
	})
}

// --- ordinary refusals ----------------------------------------------------

func TestReadRefusals(t *testing.T) {
	f := newFixture(t, map[string]string{"sub/b.txt": "b\n"})
	s := f.set(t)
	ctx := context.Background()

	t.Run("a missing file", func(t *testing.T) {
		_, err := s.ReadFile(ctx, tools.ReadRequest{Path: "nope.txt"})
		wantFault(t, err, tools.FaultTask)
		if !strings.Contains(err.Error(), "no such file") {
			t.Errorf("message = %q", err)
		}
	})

	t.Run("a directory points at list_dir", func(t *testing.T) {
		_, err := s.ReadFile(ctx, tools.ReadRequest{Path: "sub"})
		if !errors.Is(err, tools.ErrNotRegular) {
			t.Fatalf("got %v, want ErrNotRegular", err)
		}
		if !strings.Contains(err.Error(), "list_dir") {
			t.Errorf("message %q should say what to use instead", err)
		}
	})
}

// TestReadCancelled checks the context is honoured and, when it is not the
// model's doing, classified as internal rather than charged to the model.
func TestReadCancelled(t *testing.T) {
	f := newFixture(t, map[string]string{"main.go": sample})
	s := f.set(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.ReadFile(ctx, tools.ReadRequest{Path: "main.go"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	wantFault(t, err, tools.FaultInternal)
}
