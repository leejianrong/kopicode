package tools_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/tools"
)

// fixture is a temporary repository plus a sibling directory outside it. Every
// escape test needs somewhere real to escape *to* — a check that only ever sees
// paths that do not exist proves nothing about containment.
type fixture struct {
	base    string
	root    string
	outside string
}

// newFixture writes files into a temp repository. Keys are slash paths relative
// to the repository root; parent directories are created as needed.
func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{
		base:    base,
		root:    filepath.Join(base, "repo"),
		outside: filepath.Join(base, "outside"),
	}
	mkdir(t, f.root)
	mkdir(t, f.outside)
	write(t, filepath.Join(f.outside, "secret.txt"), "secret\n")
	for name, body := range files {
		f.write(t, name, body)
	}
	return f
}

func (f *fixture) write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(name))
	mkdir(t, filepath.Dir(p))
	write(t, p, body)
	return p
}

// symlink creates a link inside the repository, skipping the test where the
// platform will not have it. Windows needs a privilege that CI does not always
// grant, and a symlink test that silently passes because no link was made is
// worse than one that is honestly skipped.
func (f *fixture) symlink(t *testing.T, target, name string) {
	t.Helper()
	link := filepath.Join(f.root, filepath.FromSlash(name))
	mkdir(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, fs.ErrPermission) || errors.Is(err, errors.ErrUnsupported) {
			t.Skipf("symlinks unavailable on this platform: %v", err)
		}
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// set opens the tool set on the fixture's repository root.
func (f *fixture) set(t *testing.T) *tools.Set {
	t.Helper()
	s, err := tools.NewSet(f.root)
	if err != nil {
		t.Fatalf("NewSet(%s): %v", f.root, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// lines splits tool output for assertions, dropping the trailing empty element
// every tool's newline-terminated output produces.
func lines(out string) []string {
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// wantFault asserts the classification the journal's ToolResult.ErrorKind is
// built from. Getting this wrong is not cosmetic: ADR-0006 §3 reads "internal"
// as a harness failure and "task" as nothing at all.
func wantFault(t *testing.T, err error, want tools.Fault) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error with fault %q, got nil", want)
	}
	if got := tools.FaultOf(err); got != want {
		t.Errorf("fault = %q, want %q (err: %v)", got, want, err)
	}
}
