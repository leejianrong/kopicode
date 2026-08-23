package tools_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// --- the happy path --------------------------------------------------------

// TestDeleteFileRemovesAnExistingFile is the assertion that matters: what is
// on disk, read outside the tool that removed it. Asserting the tool's own
// result would let a delete that never happened pass.
func TestDeleteFileRemovesAnExistingFile(t *testing.T) {
	f := newFixture(t, map[string]string{"test_bug.js": "console.log(1);\n"})
	s := f.set(t)

	res, err := s.DeleteFile(context.Background(), tools.DeleteRequest{Path: "test_bug.js"})
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(f.root, "test_bug.js")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the file is still there after delete_file: stat err = %v", serr)
	}
	if want := "test_bug.js"; res.Path != want {
		t.Errorf("Path = %q, want %q", res.Path, want)
	}
	if !strings.Contains(res.Output, "deleted") {
		t.Errorf("output does not say the file was deleted:\n%s", res.Output)
	}
}

// TestDeleteFileThroughLinkInsideTheRoot keeps the containment guard from
// being a blunt "no symlinks" rule, mirroring
// TestWriteFileThroughLinkInsideTheRoot.
func TestDeleteFileThroughLinkInsideTheRoot(t *testing.T) {
	f := newFixture(t, map[string]string{"sub/b.txt": "b\n"})
	f.symlink(t, "sub", "link")
	s := f.set(t)

	res, err := s.DeleteFile(context.Background(), tools.DeleteRequest{Path: "link/b.txt"})
	if err != nil {
		t.Fatalf("DeleteFile through an in-root link: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(f.root, "sub", "b.txt")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("sub/b.txt is still there: stat err = %v", serr)
	}
	if want := "sub/b.txt"; res.Path != want {
		t.Errorf("Path = %q, want %q — the result names what was actually removed", res.Path, want)
	}
}

// --- a path that does not exist --------------------------------------------

// TestDeleteFileRefusesMissingPath is a normal, model-observable outcome —
// the model asking to delete something already gone — and must be a task
// failure, not a harness one.
func TestDeleteFileRefusesMissingPath(t *testing.T) {
	f := newFixture(t, nil)
	s := f.set(t)

	res, err := s.DeleteFile(context.Background(), tools.DeleteRequest{Path: "nope.txt"})
	if err == nil {
		t.Fatalf("DeleteFile(missing) = %+v, nil; want an error", res)
	}
	wantFault(t, err, tools.FaultTask)
	var te *tools.Error
	if !errors.As(err, &te) {
		t.Fatalf("error is not a *tools.Error: %v", err)
	}
	if !strings.Contains(te.Detail, "nothing to delete") {
		t.Errorf("Detail = %q, want it to explain there is nothing to delete", te.Detail)
	}
}

// --- targets that are not a file -------------------------------------------

func TestDeleteFileRefusesNonFiles(t *testing.T) {
	cases := []struct {
		name  string
		given string
		want  error
	}{
		{"a directory", "sub", tools.ErrNotRegular},
		{"the repository root", ".", tools.ErrNotRegular},
		{"the repository root as an empty path", "", tools.ErrNotRegular},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, map[string]string{"sub/b.txt": "b\n"})
			s := f.set(t)

			_, err := s.DeleteFile(context.Background(), tools.DeleteRequest{Path: tc.given})
			if !errors.Is(err, tc.want) {
				t.Fatalf("DeleteFile(%q) = %v, want %v", tc.given, err, tc.want)
			}
			wantFault(t, err, tools.FaultTask)
			if _, serr := os.Stat(filepath.Join(f.root, "sub", "b.txt")); serr != nil {
				t.Errorf("an existing file was removed: %v", serr)
			}
		})
	}
}

// --- containment: the escape case ------------------------------------------

// TestDeleteFileRejectsEscapeThroughSymlink is the exact case write.go's own
// doc comment describes: a symlinked directory already on disk taking a
// lexically-inside path somewhere else. It must be refused with
// [tools.ErrOutsideRoot] and must not touch anything outside the root.
func TestDeleteFileRejectsEscapeThroughSymlink(t *testing.T) {
	f := newFixture(t, nil)
	f.symlink(t, f.outside, "safe")
	s := f.set(t)

	res, err := s.DeleteFile(context.Background(), tools.DeleteRequest{Path: "safe/secret.txt"})
	if !errors.Is(err, tools.ErrOutsideRoot) {
		t.Fatalf("DeleteFile(escape) = %+v, %v; want ErrOutsideRoot", res, err)
	}
	wantFault(t, err, tools.FaultTask)

	var te *tools.Error
	if !errors.As(err, &te) {
		t.Fatalf("error is not a *tools.Error: %v", err)
	}
	if te.Tool != tools.ToolDeleteFile {
		t.Errorf("Tool = %q, want %q", te.Tool, tools.ToolDeleteFile)
	}
	if te.Resolved == "" {
		t.Error("Resolved is empty; a permission prompt has nothing to name")
	}

	// The guard is unproven until it is seen refusing to touch the file it
	// was aimed at: confirm secret.txt is still there, outside the root.
	if got := readBack(t, filepath.Join(f.outside, "secret.txt")); got != "secret\n" {
		t.Errorf("the file outside the root was modified or removed: %q", got)
	}
}

// --- cancellation ------------------------------------------------------

// TestDeleteFileCancellationIsAResult pins the half of KAN-808's convention
// that is specific to this tool: the result still arrives, saying nothing was
// deleted and leaving the file in place. The classification that comes with
// it is tabled across every tool in cancel_test.go.
func TestDeleteFileCancellationIsAResult(t *testing.T) {
	f := newFixture(t, map[string]string{"test_bug.js": "x\n"})
	s := f.set(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := s.DeleteFile(ctx, tools.DeleteRequest{Path: "test_bug.js"})
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
	if _, serr := os.Lstat(filepath.Join(f.root, "test_bug.js")); serr != nil {
		t.Errorf("a cancelled delete removed the file anyway: stat err = %v", serr)
	}
}
