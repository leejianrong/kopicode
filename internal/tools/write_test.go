package tools_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// readBack is the assertion that matters for a write: what is on disk, read
// outside the tool that put it there. Asserting the tool's own result would let
// a write that never happened pass.
func readBack(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back %s: %v", path, err)
	}
	return string(b)
}

// --- the happy paths ------------------------------------------------------

func TestWriteFileCreates(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	res, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path:    "new.txt",
		Content: "one\ntwo\n",
	})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readBack(t, filepath.Join(f.root, "new.txt")); got != "one\ntwo\n" {
		t.Errorf("content = %q, want %q", got, "one\ntwo\n")
	}
	if res.Replaced {
		t.Error("Replaced is true for a file that did not exist")
	}
	if res.Bytes != 8 || res.Lines != 2 {
		t.Errorf("Bytes/Lines = %d/%d, want 8/2", res.Bytes, res.Lines)
	}
	if got, want := lines(res.Output)[0], "write_file new.txt: created, 2 lines, 8 bytes"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
}

// TestWriteFileWritesContentVerbatim is the anchoring contract: read_file
// derives anchors over the bytes on disk, so a write_file that helpfully added
// a trailing newline would hand the model anchors for a file it did not send.
func TestWriteFileWritesContentVerbatim(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	const body = "no trailing newline"
	if _, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path: "a.txt", Content: body,
	}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readBack(t, filepath.Join(f.root, "a.txt")); got != body {
		t.Errorf("content = %q, want %q — nothing may be appended or trimmed", got, body)
	}
}

func TestWriteFileEmptyContent(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	res, err := s.WriteFile(context.Background(), tools.WriteRequest{Path: "empty.txt"})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readBack(t, filepath.Join(f.root, "empty.txt")); got != "" {
		t.Errorf("content = %q, want empty", got)
	}
	if !strings.Contains(res.Output, "created, empty") {
		t.Errorf("output does not say the file is empty:\n%s", res.Output)
	}
}

// TestWriteFileOverwriteIsDeclared is the decision this card had to make
// visible: the model may never have read what it just destroyed, so a result
// that reads the same as creating a new file is not good enough.
func TestWriteFileOverwriteIsDeclared(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "the old content, 24 chars"})
	s := f.set(t)

	res, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path: "a.txt", Content: "new\n",
	})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readBack(t, filepath.Join(f.root, "a.txt")); got != "new\n" {
		t.Errorf("content = %q, want %q — the file must be truncated, not appended to", got, "new\n")
	}
	if !res.Replaced {
		t.Error("Replaced is false for a file that existed")
	}
	if res.ReplacedBytes != 25 {
		t.Errorf("ReplacedBytes = %d, want 25", res.ReplacedBytes)
	}
	if !strings.Contains(res.Output, "replaced") {
		t.Errorf("output does not say the file was replaced:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "25 bytes of previous content were overwritten") {
		t.Errorf("output does not name what was lost:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "edit_file") {
		t.Errorf("output does not point at the tool that changes part of a file:\n%s", res.Output)
	}
}

// TestWriteFileCreatesParents covers the decision on missing parents, and that
// the creation is stated rather than silent.
func TestWriteFileCreatesParents(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	res, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path: "x/y/z/new.txt", Content: "deep\n",
	})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readBack(t, filepath.Join(f.root, "x", "y", "z", "new.txt")); got != "deep\n" {
		t.Errorf("content = %q", got)
	}
	want := []string{"x", "x/y", "x/y/z"}
	if len(res.CreatedDirs) != len(want) {
		t.Fatalf("CreatedDirs = %v, want %v", res.CreatedDirs, want)
	}
	for i, d := range want {
		if res.CreatedDirs[i] != d {
			t.Errorf("CreatedDirs[%d] = %q, want %q (outermost first, slash form)", i, res.CreatedDirs[i], d)
		}
	}
	if !strings.Contains(res.Output, "created directories x, x/y, x/y/z") {
		t.Errorf("output does not name the directories it created:\n%s", res.Output)
	}
}

// TestWriteFileExistingParentIsNotReported keeps the note honest: a directory
// that was already there was not created by this call.
func TestWriteFileExistingParentIsNotReported(t *testing.T) {
	f := newFixture(t, map[string]string{"sub/b.txt": "b\n"})
	s := f.set(t)

	res, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path: "sub/new.txt", Content: "n\n",
	})
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(res.CreatedDirs) != 0 {
		t.Errorf("CreatedDirs = %v, want none — sub already existed", res.CreatedDirs)
	}
	if strings.Contains(res.Output, "created directory") {
		t.Errorf("output claims a directory it did not create:\n%s", res.Output)
	}
}

// TestWriteFileThroughLinkInsideTheRoot keeps the containment guard from being
// a blunt "no symlinks" rule. A relative link that stays inside is ordinary in
// a repository, and if this failed the escape tests below would be passing for
// the wrong reason.
func TestWriteFileThroughLinkInsideTheRoot(t *testing.T) {
	f := newFixture(t, map[string]string{"sub/b.txt": "b\n"})
	f.symlink(t, "sub", "link")
	s := f.set(t)

	res, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path: "link/new.txt", Content: "inside\n",
	})
	if err != nil {
		t.Fatalf("WriteFile through an in-root link: %v", err)
	}
	if got := readBack(t, filepath.Join(f.root, "sub", "new.txt")); got != "inside\n" {
		t.Errorf("content = %q, want it written through the link into sub/", got)
	}
	if want := "sub/new.txt"; res.Path != want {
		t.Errorf("Path = %q, want %q — the result names where the bytes went", res.Path, want)
	}
}

// TestWriteFilePreservesModeOfAnExistingFile: os.WriteFile applies perm only on
// create, and a rewrite that stripped the executable bit off a script would be
// a silent change to something the model never asked about.
func TestWriteFilePreservesModeOfAnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	f := newFixture(t, map[string]string{"run.sh": "#!/bin/sh\n"})
	if err := os.Chmod(filepath.Join(f.root, "run.sh"), 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s := f.set(t)

	if _, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path: "run.sh", Content: "#!/bin/sh\necho hi\n",
	}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(filepath.Join(f.root, "run.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want 0755 — a rewrite must not strip the executable bit", info.Mode().Perm())
	}
}

// --- containment: the card ------------------------------------------------

// TestWriteFileRejectsEscapes is SLICE-1's Test Plan line "path resolution
// rejects ../ escapes and symlinks pointing outside the repo root", for the
// tool where the target does not exist yet and so cannot simply be stat'ed.
// Both guards were confirmed red before being confirmed green — see the PR.
func TestWriteFileRejectsEscapes(t *testing.T) {
	cases := []struct {
		name string
		// given is built per-case because it needs the fixture's paths.
		given func(f *fixture) string
		// links are created inside the repository before the call.
		links map[string]string // name -> target
	}{
		{
			name:  "dot-dot out of the root",
			given: func(*fixture) string { return "../outside/new.txt" },
		},
		{
			// A pile of ".." that climbs above the root and comes back down.
			// It stays inside the fixture so that a *broken* guard lands its
			// bytes somewhere assertNothingOutside can see, rather than
			// somewhere on the real machine.
			name: "a pile of dot-dots that climbs out and comes back",
			given: func(f *fixture) string {
				return "sub/../../" + filepath.Base(f.base) + "/outside/new.txt"
			},
		},
		{
			name:  "dot-dot in the middle of a path",
			given: func(*fixture) string { return "sub/../../outside/new.txt" },
		},
		{
			name:  "an absolute path outside",
			given: func(f *fixture) string { return filepath.Join(f.outside, "new.txt") },
		},
		{
			// The escape the card names: a symlinked parent directory. The
			// target does not exist, so there is nothing to resolve at the
			// tail; containment has to be decided on the parent.
			name:  "a symlinked parent directory pointing outside",
			given: func(*fixture) string { return "escape/new.txt" },
			links: map[string]string{"escape": "@outside"},
		},
		{
			name:  "a relative symlinked parent pointing outside",
			given: func(*fixture) string { return "escape-rel/new.txt" },
			links: map[string]string{"escape-rel": "../outside"},
		},
		{
			// The lexical trap: cleaning this string before following the link
			// gives repo/outside/new.txt, which is inside. Following the link
			// first gives the sibling directory, which is not.
			name:  "dot-dot after a symlink",
			given: func(*fixture) string { return "escape/../outside/new.txt" },
			links: map[string]string{"escape": "@outside"},
		},
		{
			// The target itself already exists, as a link pointing out. The
			// write must not follow it.
			name:  "the target is a symlink to a file outside",
			given: func(*fixture) string { return "s" },
			links: map[string]string{"s": "../outside/secret.txt"},
		},
		{
			name:  "the target is a symlink to a file outside that does not exist yet",
			given: func(*fixture) string { return "s" },
			links: map[string]string{"s": "../outside/not-there.txt"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"sub/b.txt": "b\n"})
			for name, target := range tc.links {
				if strings.HasPrefix(target, "@") {
					target = filepath.Join(f.base, strings.TrimPrefix(target, "@"))
				}
				f.symlink(t, target, name)
			}
			s := f.set(t)
			given := tc.given(f)

			res, err := s.WriteFile(context.Background(), tools.WriteRequest{
				Path: given, Content: "PWNED\n",
			})
			if !errors.Is(err, tools.ErrOutsideRoot) {
				t.Fatalf("WriteFile(%q) = %+v, %v; want ErrOutsideRoot", given, res, err)
			}
			// A refusal is a task failure and not a harness one. Getting this
			// wrong buckets a model's bad path as a harness defect (ADR-0006 §3).
			wantFault(t, err, tools.FaultTask)

			var te *tools.Error
			if !errors.As(err, &te) {
				t.Fatalf("error is not a *tools.Error: %v", err)
			}
			if te.Tool != tools.ToolWriteFile {
				t.Errorf("Tool = %q, want %q", te.Tool, tools.ToolWriteFile)
			}
			if te.Path != given {
				t.Errorf("Path = %q, want %q", te.Path, given)
			}
			// The permission gate has to be able to name what it is being
			// asked about, so the refusal carries where the path really went.
			if te.Resolved == "" {
				t.Error("Resolved is empty; a permission prompt has nothing to name")
			}

			assertNothingOutside(t, f)
		})
	}
}

// assertNothingOutside is what makes the escape tests mean something. A refusal
// that returned the right error while the bytes landed on the far side of the
// root would pass every assertion above.
func assertNothingOutside(t *testing.T, f *fixture) {
	t.Helper()
	if got := readBack(t, filepath.Join(f.outside, "secret.txt")); got != "secret\n" {
		t.Errorf("the file outside the root was modified: %q", got)
	}
	entries, err := os.ReadDir(f.outside)
	if err != nil {
		t.Fatalf("read outside dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "secret.txt" {
			t.Errorf("write_file created %q outside the repository root", e.Name())
		}
	}
}

// TestWriteFileEscapeMessageNamesTheSymlink checks the refusal is actionable. A
// model that wrote a plausible relative path cannot tell why it was refused
// unless the message says a link took it out.
func TestWriteFileEscapeMessageNamesTheSymlink(t *testing.T) {
	f := newFixture(t, nil)
	f.symlink(t, f.outside, "escape")
	s := f.set(t)

	_, err := s.WriteFile(context.Background(), tools.WriteRequest{
		Path: "escape/new.txt", Content: "x\n",
	})
	var te *tools.Error
	if !errors.As(err, &te) {
		t.Fatalf("want *tools.Error, got %v", err)
	}
	if want := "a symbolic link in the path leads outside"; !strings.Contains(te.Detail, want) {
		t.Errorf("Detail = %q, want it to mention %q", te.Detail, want)
	}
}

// --- targets that are not a file ------------------------------------------

func TestWriteFileRefusesNonFiles(t *testing.T) {
	cases := []struct {
		name  string
		given string
		// want is the sentinel the refusal must carry, nil where the cause is
		// the OS's own (a path under a file is ENOTDIR, and dressing that up in
		// a sentinel of ours would lose which component was the file).
		want error
	}{
		{"a directory", "sub", tools.ErrNotRegular},
		{"the repository root", ".", tools.ErrNotRegular},
		{"the repository root as an empty path", "", tools.ErrNotRegular},
		{"under a file, not a directory", "sub/b.txt/nested.txt", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"sub/b.txt": "b\n"})
			s := f.set(t)

			_, err := s.WriteFile(context.Background(), tools.WriteRequest{
				Path: tc.given, Content: "x\n",
			})
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("WriteFile(%q) = %v, want %v", tc.given, err, tc.want)
			}
			// The classification is the load-bearing part either way: a bad
			// path is the model's to fix, never a harness failure (ADR-0006 §3).
			wantFault(t, err, tools.FaultTask)
			if got := readBack(t, filepath.Join(f.root, "sub", "b.txt")); got != "b\n" {
				t.Errorf("an existing file was clobbered: %q", got)
			}
		})
	}
}

// --- cancellation ---------------------------------------------------------

// TestWriteFileCancellationIsAResult pins the half of KAN-808's convention that
// is specific to this tool: the result still arrives, saying nothing was
// written and leaving no file behind. The classification that comes with it is
// tabled across all five tools in cancel_test.go.
func TestWriteFileCancellationIsAResult(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := s.WriteFile(ctx, tools.WriteRequest{Path: "new.txt", Content: "x\n"})
	if !res.Cancelled {
		t.Error("Cancelled is false on a cancelled context")
	}
	if got := tools.FaultOf(err); got != tools.FaultCancelled {
		t.Errorf("fault = %q, want %q — a cancellation is nobody's failure, "+
			"and a nil error would read as a clean stop", got, tools.FaultCancelled)
	}
	if !strings.Contains(res.Output, "cancelled") {
		t.Errorf("output does not say the call was cancelled:\n%s", res.Output)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "new.txt")); err == nil {
		t.Error("a cancelled write created the file anyway")
	}
}
