package permission_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/tools"
)

// The production adapter satisfies the interface. This is the assertion the
// package went without: internal/permission declared [permission.Resolver] and
// named internal/tools as its implementation, both packages were green, and
// nothing connected them (KAN-810). A signature change on either side now fails
// to compile here rather than at whatever call site wires the engine.
var _ permission.Resolver = tools.Resolver{}

// fsResolver is a second, independent implementation of the same interface,
// kept because two implementations that agree is a stronger statement than one
// that passes its own tests. It also answers for paths with no [tools.Root]
// behind them, which is what the construction-failure cases need.
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

// realResolver builds the production adapter over root.
func realResolver(t *testing.T, root string) permission.Resolver {
	t.Helper()
	r, err := tools.OpenRoot(root)
	if err != nil {
		t.Fatalf("opening tools root %q: %v", root, err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("closing tools root: %v", err)
		}
	})
	return r.Resolver()
}

// resolverImpls is every implementation the shared table below runs against.
// The production one is in the list, which is the point: a table exercising
// only the double proves the double.
func resolverImpls(t *testing.T, root string) map[string]permission.Resolver {
	t.Helper()
	return map[string]permission.Resolver{
		"tools.Resolver": realResolver(t, root),
		"fsResolver":     fsResolver{},
	}
}

// TestResolversAgree pins the [permission.Resolver] contract on both
// implementations at once. Every case is one of the three the interface's
// documentation says a correct resolver must survive, and each is a way a gate
// goes wrong: a "../" that is not followed makes an escape look contained, an
// unfollowed symlink does the same more quietly, and a refusal to resolve a
// path that does not exist yet would deny every new file.
func TestResolversAgree(t *testing.T) {
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

	for impl, resolver := range resolverImpls(t, dirs.root) {
		t.Run(impl, func(t *testing.T) {
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
		})
	}
}

// TestResolversRefuseAnEmptyPath: "" is a missing argument, not a request about
// the current directory. Answering it with a path — the repo root, say — would
// hand the gate something that looks contained, so both implementations refuse.
func TestResolversRefuseAnEmptyPath(t *testing.T) {
	dirs := newDirs(t)

	for impl, resolver := range resolverImpls(t, dirs.root) {
		t.Run(impl, func(t *testing.T) {
			got, err := resolver.Resolve("")
			if err == nil {
				t.Fatalf(`Resolve("") = %q, want an error: an empty path must not resolve to anything`, got)
			}
		})
	}
}

// TestZeroResolverRefuses: the zero value has no root behind it. A gate built
// on one must fail loudly at construction rather than panicking on the first
// write the model attempts.
func TestZeroResolverRefuses(t *testing.T) {
	if _, err := (tools.Resolver{}).Resolve("main.go"); err == nil {
		t.Fatal("the zero tools.Resolver resolved a path; it has no root and must refuse")
	}
	if _, err := permission.New("/repo", tools.Resolver{}, fixedPolicy{}); err == nil {
		t.Fatal("a gate was built on the zero tools.Resolver; construction must fail")
	}
}

// TestProductionResolverDrivesTheGate is the end-to-end the card turns on: the
// gate's classification rules running on the real adapter rather than on a
// double. It is the case a naive adapter fails — one delegating to
// tools.Root.Resolve, which refuses a path outside the root, would turn "a write
// outside the repo root asks" into "a write outside the repo root is denied
// without asking", and every double in this package would stay green.
func TestProductionResolverDrivesTheGate(t *testing.T) {
	dirs := newDirs(t)
	loot := filepath.Join(dirs.outside, "loot.txt")

	tests := []struct {
		name     string
		path     string
		required bool
		resolved string
	}{
		{"write inside the root never asks", filepath.Join(dirs.root, "main.go"), false, ""},
		{"write to a new file inside the root never asks", filepath.Join(dirs.root, "new", "deep", "file.go"), false, ""},
		{"write through dot dot asks, naming where it went", filepath.Join(dirs.root, "..", "outside", "loot.txt"), true, loot},
		{"write through a symlink asks, naming where it went", filepath.Join(dirs.root, "escape", "loot.txt"), true, loot},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			asker := &verdictAsker{verdict: permission.VerdictAllow}
			policy, err := permission.NewAsk(asker)
			if err != nil {
				t.Fatalf("building ask policy: %v", err)
			}
			g := mustGate(t, dirs.root, realResolver(t, dirs.root), policy)

			out, err := g.Check(context.Background(), permission.Action{
				ID:        "call-1",
				Tool:      "write_file",
				Operation: permission.OperationWrite,
				Path:      tc.path,
			})
			assertInvariants(t, out, err)
			if err != nil {
				t.Fatalf("Check(%q) denied: %v", tc.path, err)
			}
			if out.Required != tc.required {
				t.Fatalf("Check(%q) Required = %t, want %t", tc.path, out.Required, tc.required)
			}
			if !tc.required {
				if n := len(asker.requests()); n != 0 {
					t.Errorf("the user was asked %d times about a write inside the root", n)
				}
				return
			}
			if out.Request.Kind != permission.KindWriteOutsideRoot {
				t.Errorf("Kind = %s, want %s", out.Request.Kind, permission.KindWriteOutsideRoot)
			}
			// The user consents to where the write lands, not to how the model
			// spelled it.
			if out.Request.Resolved != tc.resolved {
				t.Errorf("Resolved = %q, want %q", out.Request.Resolved, tc.resolved)
			}
			if n := len(asker.requests()); n != 1 {
				t.Errorf("the user was asked %d times, want once", n)
			}
		})
	}
}

// TestProductionResolverDrivesTheBenchPolicy is the same wiring on the headless
// side, where there is nobody to catch a wrong answer. The bench policy judges
// containment against the task worktree, which is not the tools root the
// resolver was built on — a resolver that answered only about its own root
// could not serve this at all.
func TestProductionResolverDrivesTheBenchPolicy(t *testing.T) {
	dirs := newDirs(t)
	resolver := realResolver(t, dirs.root)

	policy, err := permission.NewBench(dirs.root, resolver)
	if err != nil {
		t.Fatalf("building bench policy: %v", err)
	}
	g := mustGate(t, dirs.root, resolver, policy)

	tests := []struct {
		name    string
		dir     string
		allowed bool
	}{
		{"shell inside the worktree is approved", dirs.root, true},
		{"shell outside the worktree is refused", dirs.outside, false},
		{"shell through a symlink out of the worktree is refused", filepath.Join(dirs.root, "escape"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := g.Check(context.Background(), permission.Action{
				ID:        "call-1",
				Tool:      "run_shell",
				Operation: permission.OperationShell,
				Command:   []string{"go", "test", "./..."},
				Dir:       tc.dir,
			})
			assertInvariants(t, out, err)
			if out.Allowed != tc.allowed {
				t.Fatalf("Check(dir=%q) Allowed = %t (err %v), want %t", tc.dir, out.Allowed, err, tc.allowed)
			}
			if !out.Required {
				t.Error("run_shell must always require consent")
			}
			if out.Decision.Source != permission.SourcePolicy {
				t.Errorf("Source = %s, want %s: a bench approval is never a user's", out.Decision.Source, permission.SourcePolicy)
			}
			if !tc.allowed && !errors.Is(err, permission.ErrDenied) {
				t.Errorf("refusal did not wrap ErrDenied: %v", err)
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
