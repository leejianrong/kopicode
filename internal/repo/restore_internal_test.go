package repo

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These tests exercise the tar-extraction guards directly, without a git
// repository at all — the same reasoning as guard_internal_test.go: a guard
// against a specific escape is worth proving over a hand-built input that
// triggers exactly that escape, rather than only over whatever a real `git
// archive` happens to produce.

// tarEntry is one entry to write into a test tar stream.
type tarEntry struct {
	name     string
	typeflag byte
	mode     int64
	content  []byte
	linkname string
}

// buildTar renders entries into a tar byte stream, the input extractTar
// consumes.
func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     e.mode,
			Size:     int64(len(e.content)),
			Linkname: e.linkname,
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header for %q: %v", e.name, err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("writing tar content for %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	return buf.Bytes()
}

// extractInto opens dest as an os.Root and runs extractTar against it,
// closing the root before returning.
func extractInto(t *testing.T, dest string, data []byte) error {
	t.Helper()
	root, err := os.OpenRoot(dest)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", dest, err)
	}
	defer func() { _ = root.Close() }()
	return extractTar(bytes.NewReader(data), root, dest)
}

// TestExtractTarWritesFilesDirectoriesAndSymlinks is the happy path: all
// three supported entry types land where the tar stream says.
func TestExtractTarWritesFilesDirectoriesAndSymlinks(t *testing.T) {
	dest := t.TempDir()
	data := buildTar(t,
		tarEntry{name: "dir/", typeflag: tar.TypeDir, mode: 0o755},
		tarEntry{name: "dir/file.txt", typeflag: tar.TypeReg, content: []byte("hello\n")},
		tarEntry{name: "link.txt", typeflag: tar.TypeSymlink, linkname: "dir/file.txt"},
	)
	if err := extractInto(t, dest, data); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "dir", "file.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Errorf("dir/file.txt = %q, %v", got, err)
	}
	link, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil || link != "dir/file.txt" {
		t.Errorf("link.txt -> %q, %v, want dir/file.txt", link, err)
	}
}

// TestExtractTarOverwritesAnExistingSymlink — a re-run over a destination
// that already holds a symlink at the same path must replace it rather than
// failing on os.Symlink's EEXIST.
func TestExtractTarOverwritesAnExistingSymlink(t *testing.T) {
	dest := t.TempDir()
	if err := os.Symlink("old-target.txt", filepath.Join(dest, "link.txt")); err != nil {
		t.Skipf("symlinks are not supported here: %v", err)
	}

	data := buildTar(t, tarEntry{name: "link.txt", typeflag: tar.TypeSymlink, linkname: "new-target.txt"})
	if err := extractInto(t, dest, data); err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	got, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil || got != "new-target.txt" {
		t.Errorf("link.txt -> %q, %v, want new-target.txt", got, err)
	}
}

// TestExtractTarRejectsPathTraversal covers the classic archive-extraction
// escape: an entry name that walks above the destination.
func TestExtractTarRejectsPathTraversal(t *testing.T) {
	for _, name := range []string{"../escape.txt", "a/../../escape.txt", "a/b/../../../escape.txt"} {
		data := buildTar(t, tarEntry{name: name, typeflag: tar.TypeReg, content: []byte("x")})
		err := extractInto(t, t.TempDir(), data)
		if !errors.Is(err, ErrRestoreUnsafeEntry) {
			t.Errorf("extractTar(name=%q) = %v, want ErrRestoreUnsafeEntry", name, err)
		}
	}
}

// TestExtractTarRejectsAnAbsolutePath — an absolute tar entry name is never
// "inside dest" no matter how it is joined.
func TestExtractTarRejectsAnAbsolutePath(t *testing.T) {
	data := buildTar(t, tarEntry{name: "/etc/passwd", typeflag: tar.TypeReg, content: []byte("x")})
	err := extractInto(t, t.TempDir(), data)
	if !errors.Is(err, ErrRestoreUnsafeEntry) {
		t.Errorf("extractTar(name=/etc/passwd) = %v, want ErrRestoreUnsafeEntry", err)
	}
}

// TestExtractTarRejectsADotGitPathComponent — git itself refuses to track a
// path literally called ".git" through `add`/`commit`, but Restore accepts
// any tree this repository can resolve, including one built by hand through
// low-level plumbing that bypasses that guard. Case folding is checked too,
// since the filesystem this lands on may not be case-sensitive.
func TestExtractTarRejectsADotGitPathComponent(t *testing.T) {
	for _, name := range []string{".git", ".git/config", ".GIT/hooks/pre-commit", "nested/.git", "a/.Git/x"} {
		data := buildTar(t, tarEntry{name: name, typeflag: tar.TypeReg, content: []byte("x")})
		err := extractInto(t, t.TempDir(), data)
		if !errors.Is(err, ErrRestoreUnsafeEntry) {
			t.Errorf("extractTar(name=%q) = %v, want ErrRestoreUnsafeEntry", name, err)
		}
	}
}

// TestExtractTarRejectsASymlinkEscapingDest — os.Root.Symlink documents that
// it "does not validate oldname", so this package has to before creating the
// link at all. Covers a relative escape, a deeper relative escape and an
// absolute target.
func TestExtractTarRejectsASymlinkEscapingDest(t *testing.T) {
	for _, tc := range []struct{ name, link string }{
		{"escape", "../outside.txt"},
		{"nested/escape", "../../outside.txt"},
		{"abs-escape", "/etc/passwd"},
	} {
		data := buildTar(t, tarEntry{name: tc.name, typeflag: tar.TypeSymlink, linkname: tc.link})
		err := extractInto(t, t.TempDir(), data)
		if !errors.Is(err, ErrRestoreUnsafeEntry) {
			t.Errorf("extractTar(name=%q, link=%q) = %v, want ErrRestoreUnsafeEntry", tc.name, tc.link, err)
		}
	}
}

// TestExtractTarAcceptsASymlinkThatStaysInsideDest — the guard above must not
// be so eager it rejects an ordinary relative symlink that happens to use
// "..", as long as it never leaves dest.
func TestExtractTarAcceptsASymlinkThatStaysInsideDest(t *testing.T) {
	dest := t.TempDir()
	data := buildTar(t,
		tarEntry{name: "a/target.txt", typeflag: tar.TypeReg, content: []byte("ok\n")},
		tarEntry{name: "a/b/link.txt", typeflag: tar.TypeSymlink, linkname: "../target.txt"},
	)
	if err := extractInto(t, dest, data); err != nil {
		t.Fatalf("extractTar rejected a symlink that stays inside dest: %v", err)
	}
	got, err := os.Readlink(filepath.Join(dest, "a", "b", "link.txt"))
	if err != nil || got != "../target.txt" {
		t.Errorf("a/b/link.txt -> %q, %v, want ../target.txt", got, err)
	}
}

// TestExtractTarRejectsUnsupportedEntryTypes — git never produces one of
// these from a real tree object, and a gap here must fail loudly rather than
// silently skip or silently write something unexpected.
func TestExtractTarRejectsUnsupportedEntryTypes(t *testing.T) {
	for _, typeflag := range []byte{tar.TypeChar, tar.TypeBlock, tar.TypeFifo, tar.TypeLink} {
		data := buildTar(t, tarEntry{name: "dev", typeflag: typeflag})
		err := extractInto(t, t.TempDir(), data)
		if !errors.Is(err, ErrRestoreUnsupportedEntry) {
			t.Errorf("extractTar(typeflag=%q) = %v, want ErrRestoreUnsupportedEntry", string(typeflag), err)
		}
	}
}

// TestExtractTarRejectsASymlinkWithNoTarget — a symlink header with an empty
// Linkname is not something a real git tree produces, but nothing should
// crash on it either.
func TestExtractTarRejectsASymlinkWithNoTarget(t *testing.T) {
	data := buildTar(t, tarEntry{name: "link.txt", typeflag: tar.TypeSymlink, linkname: ""})
	err := extractInto(t, t.TempDir(), data)
	if !errors.Is(err, ErrRestoreUnsafeEntry) {
		t.Errorf("extractTar(empty linkname) = %v, want ErrRestoreUnsafeEntry", err)
	}
}

// TestRestoreEntryNameRejectsAnEmptyName is the direct unit case for the one
// input buildTar cannot easily produce: tar.Writer itself refuses to write a
// header with an empty name, so the guard is only reachable by calling the
// function directly.
func TestRestoreEntryNameRejectsAnEmptyName(t *testing.T) {
	if _, err := restoreEntryName(""); !errors.Is(err, ErrRestoreUnsafeEntry) {
		t.Errorf("restoreEntryName(\"\") = %v, want ErrRestoreUnsafeEntry", err)
	}
}

// TestPrepareRestoreDestRefusesInsideGitDir is unit-testable without a real
// repository: only the two path fields matter.
func TestPrepareRestoreDestRefusesInsideGitDir(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Repo{root: root, gitDir: gitDir, commonDir: gitDir}

	for _, dest := range []string{gitDir, filepath.Join(gitDir, "objects")} {
		if _, err := r.prepareRestoreDest(dest); !errors.Is(err, ErrRestoreDestInsideGitDir) {
			t.Errorf("prepareRestoreDest(%q) = %v, want ErrRestoreDestInsideGitDir", dest, err)
		}
	}

	dest := filepath.Join(root, "scratch", "out")
	abs, err := r.prepareRestoreDest(dest)
	if err != nil {
		t.Fatalf("prepareRestoreDest(%q): %v", dest, err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		t.Errorf("prepareRestoreDest did not create %s as a directory (stat err = %v)", abs, err)
	}
}

// TestPrepareRestoreDestRefusesANonDirectory — an existing regular file at
// dest is not something Restore can extract a tree into.
func TestPrepareRestoreDestRefusesANonDirectory(t *testing.T) {
	root := t.TempDir()
	r := &Repo{root: root}
	file := filepath.Join(root, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.prepareRestoreDest(file); err == nil {
		t.Error("prepareRestoreDest accepted a destination that is a regular file")
	}
}
