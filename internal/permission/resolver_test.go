package permission_test

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

// fsResolver is the test's stand-in for the path-resolution helper landing in
// internal/tools (KAN-780), which this package deliberately does not import.
//
// It is a real resolver rather than a fake returning canned answers, because
// the properties under test — that "../" escapes, that a symlink escapes, that
// a file which does not exist yet still resolves — are properties of a real
// filesystem. A stub that answered "inside: true" would let the containment
// tests pass while containment was broken.
type fsResolver struct{}

// Resolve makes path absolute, follows every symlink, and tolerates a target
// that does not exist by resolving its nearest existing ancestor and
// re-appending the remainder.
func (fsResolver) Resolve(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	cur, rest := filepath.Clean(abs), ""
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if rest == "" {
				return real, nil
			}
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// TestResolverDouble checks the test double before anything relies on it. A
// resolver that quietly failed to follow symlinks would make every containment
// assertion below vacuous.
func TestResolverDouble(t *testing.T) {
	dirs := newDirs(t)

	tests := []struct {
		name string
		path string
		want string
	}{
		{"plain file inside", filepath.Join(dirs.root, "main.go"), filepath.Join(dirs.root, "main.go")},
		{"dot dot escapes", filepath.Join(dirs.root, "..", "outside", "loot.txt"), filepath.Join(dirs.outside, "loot.txt")},
		{"symlink escapes", filepath.Join(dirs.root, "escape", "loot.txt"), filepath.Join(dirs.outside, "loot.txt")},
		{"path that does not exist yet", filepath.Join(dirs.root, "new", "deep", "file.go"), filepath.Join(dirs.root, "new", "deep", "file.go")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := fsResolver{}.Resolve(tc.path)
			if err != nil {
				t.Fatalf("Resolve(%q) failed: %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("Resolve(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// errUnresolvable stands for whatever stops a real resolver answering: a
// directory it may not traverse, a symlink loop, a filesystem that went away.
var errUnresolvable = errors.New("cannot resolve")

// resolveOnly answers for exactly one path — the root, so a gate can still be
// built — and fails for every other. It is how the "a path that cannot be
// resolved denies" rule gets exercised without breaking construction first.
type resolveOnly struct{ only string }

func (r resolveOnly) Resolve(path string) (string, error) {
	if path == r.only {
		return r.only, nil
	}
	return "", errUnresolvable
}
