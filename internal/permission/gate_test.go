package permission_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
)

// dirs is the fixture the containment cases are judged against: a repo root, a
// sibling directory outside it, and a symlink inside the root pointing at that
// sibling.
type dirs struct {
	root    string
	outside string
}

func newDirs(t *testing.T) dirs {
	t.Helper()
	// t.TempDir is itself under /tmp, which is a symlink to /private/tmp on
	// macOS. Resolving it up front keeps the expected paths comparable with
	// what the resolver returns.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}
	d := dirs{root: filepath.Join(base, "repo"), outside: filepath.Join(base, "outside")}
	for _, p := range []string{d.root, d.outside} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("creating %s: %v", p, err)
		}
	}
	if err := os.Symlink(d.outside, filepath.Join(d.root, "escape")); err != nil {
		t.Fatalf("linking escape hatch: %v", err)
	}
	return d
}

// verdictAsker is a surface that always answers the same way, standing in for
// the human the REPL will put behind [permission.Asker].
type verdictAsker struct {
	verdict permission.Verdict
	err     error

	mu   sync.Mutex
	seen []permission.Request
}

func (a *verdictAsker) Ask(_ context.Context, req permission.Request) (permission.Verdict, error) {
	a.mu.Lock()
	a.seen = append(a.seen, req)
	a.mu.Unlock()
	return a.verdict, a.err
}

func (a *verdictAsker) requests() []permission.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]permission.Request(nil), a.seen...)
}

func mustAsk(t *testing.T, v permission.Verdict) permission.Policy {
	t.Helper()
	p, err := permission.NewAsk(&verdictAsker{verdict: v})
	if err != nil {
		t.Fatalf("building ask policy: %v", err)
	}
	return p
}

func mustBench(t *testing.T, worktree string) permission.Policy {
	t.Helper()
	p, err := permission.NewBench(worktree, fsResolver{})
	if err != nil {
		t.Fatalf("building bench policy: %v", err)
	}
	return p
}

func mustGate(t *testing.T, root string, r permission.Resolver, p permission.Policy) *permission.Gate {
	t.Helper()
	g, err := permission.New(root, r, p)
	if err != nil {
		t.Fatalf("building gate: %v", err)
	}
	return g
}

// assertInvariants holds the contract every call site depends on: the error is
// nil exactly when the action is allowed, a refusal is always an [ErrDenied],
// and nothing is recorded as needing consent unless it was actually asked.
func assertInvariants(t *testing.T, out permission.Outcome, err error) {
	t.Helper()
	if out.Allowed != (err == nil) {
		t.Errorf("Allowed = %t but err = %v; the two must agree", out.Allowed, err)
	}
	if err != nil && !errors.Is(err, permission.ErrDenied) {
		t.Errorf("refusal %v does not wrap ErrDenied", err)
	}
	// Request holds a slice and so is not comparable; its kind and id are
	// enough to tell a populated one from the zero value.
	if !out.Required && (out.Request.Kind != permission.KindUnspecified || out.Request.ID != "" || out.Decision != (permission.Decision{})) {
		t.Errorf("Required is false but the outcome carries a request or a decision: %+v", out)
	}
}

// TestPolicyMatrix is the card's matrix: operation × location × policy. Each
// row states what classification must conclude; each column states how a given
// policy answers what is left.
//
// A row whose operation nobody classified is a refusal, not a default — see the
// last three rows, and TestEveryOperationIsClassified in the internal test for
// the guard that a *newly added* operation cannot slip through unclassified.
func TestPolicyMatrix(t *testing.T) {
	d := newDirs(t)

	const (
		askYes = "ask-yes"
		askNo  = "ask-no"
		bench  = "bench"
	)

	tests := []struct {
		name         string
		action       permission.Action
		wantRequired bool
		wantKind     permission.Kind
		wantErr      error
		// allow is keyed by policy name. Absent means false.
		allow map[string]bool
	}{
		{
			name:   "read inside the root never asks",
			action: permission.Action{ID: "1", Tool: "read_file", Operation: permission.OperationRead, Path: filepath.Join(d.root, "main.go")},
			allow:  map[string]bool{askYes: true, askNo: true, bench: true},
		},
		{
			// Reads never ask, wherever they point. Whether a read may leave
			// the root at all is the tool layer's containment check; consent
			// is not the mechanism for it.
			name:   "read outside the root still never asks",
			action: permission.Action{ID: "2", Tool: "read_file", Operation: permission.OperationRead, Path: filepath.Join(d.outside, "secrets")},
			allow:  map[string]bool{askYes: true, askNo: true, bench: true},
		},
		{
			name:   "write inside the root never asks",
			action: permission.Action{ID: "3", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.root, "main.go")},
			allow:  map[string]bool{askYes: true, askNo: true, bench: true},
		},
		{
			name:   "write to a file that does not exist yet, inside, never asks",
			action: permission.Action{ID: "4", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.root, "new", "deep", "file.go")},
			allow:  map[string]bool{askYes: true, askNo: true, bench: true},
		},
		{
			name:         "write escaping via dot dot asks",
			action:       permission.Action{ID: "5", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.root, "..", "outside", "loot.txt")},
			wantRequired: true,
			wantKind:     permission.KindWriteOutsideRoot,
			allow:        map[string]bool{askYes: true},
		},
		{
			name:         "write escaping through a symlink asks",
			action:       permission.Action{ID: "6", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.root, "escape", "loot.txt")},
			wantRequired: true,
			wantKind:     permission.KindWriteOutsideRoot,
			allow:        map[string]bool{askYes: true},
		},
		{
			name:         "write to a file that does not exist yet, outside, asks",
			action:       permission.Action{ID: "7", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.outside, "never", "made", "yet.txt")},
			wantRequired: true,
			wantKind:     permission.KindWriteOutsideRoot,
			allow:        map[string]bool{askYes: true},
		},
		{
			// A path whose prefix matches the root but which is not under it.
			// The string-comparison version of containment gets this wrong.
			name:         "write to a sibling sharing the root's prefix asks",
			action:       permission.Action{ID: "8", Tool: "write_file", Operation: permission.OperationWrite, Path: d.root + "-backup/x"},
			wantRequired: true,
			wantKind:     permission.KindWriteOutsideRoot,
			allow:        map[string]bool{askYes: true},
		},
		{
			name:         "shell inside the root still asks",
			action:       permission.Action{ID: "9", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"go", "test", "./..."}, Dir: d.root},
			wantRequired: true,
			wantKind:     permission.KindRunShell,
			allow:        map[string]bool{askYes: true, bench: true},
		},
		{
			name:         "shell outside the root asks and bench refuses",
			action:       permission.Action{ID: "10", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"rm", "-rf", "."}, Dir: d.outside},
			wantRequired: true,
			wantKind:     permission.KindRunShell,
			allow:        map[string]bool{askYes: true},
		},
		{
			// No working directory means the runner's own, which every task in
			// a corpus shares. Bench refuses; a human may still say yes.
			name:         "shell with no working directory asks and bench refuses",
			action:       permission.Action{ID: "11", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"ls"}},
			wantRequired: true,
			wantKind:     permission.KindRunShell,
			allow:        map[string]bool{askYes: true},
		},
		{
			name:    "shell with no command is refused before anyone is asked",
			action:  permission.Action{ID: "12", Tool: "run_shell", Operation: permission.OperationShell, Dir: d.root},
			wantErr: permission.ErrInvalidAction,
		},
		{
			name:    "write with no path is refused before anyone is asked",
			action:  permission.Action{ID: "13", Tool: "write_file", Operation: permission.OperationWrite},
			wantErr: permission.ErrInvalidAction,
		},
		{
			name:    "an action with no operation is refused",
			action:  permission.Action{ID: "14", Tool: "mystery"},
			wantErr: permission.ErrUnknownOperation,
		},
		{
			name:    "an operation nobody declared is refused",
			action:  permission.Action{ID: "15", Tool: "mystery", Operation: permission.Operation(200), Path: filepath.Join(d.root, "main.go")},
			wantErr: permission.ErrUnknownOperation,
		},
	}

	policies := map[string]func(*testing.T) permission.Policy{
		askYes: func(t *testing.T) permission.Policy { return mustAsk(t, permission.VerdictAllow) },
		askNo:  func(t *testing.T) permission.Policy { return mustAsk(t, permission.VerdictDeny) },
		bench:  func(t *testing.T) permission.Policy { return mustBench(t, d.root) },
	}

	for _, tc := range tests {
		for _, name := range []string{askYes, askNo, bench} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				g := mustGate(t, d.root, fsResolver{}, policies[name](t))
				out, err := g.Check(t.Context(), tc.action)
				assertInvariants(t, out, err)

				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want one wrapping %v", err, tc.wantErr)
				}
				if want := tc.allow[name]; out.Allowed != want {
					t.Fatalf("Allowed = %t, want %t (err: %v)", out.Allowed, want, err)
				}
				if out.Required != tc.wantRequired {
					t.Fatalf("Required = %t, want %t", out.Required, tc.wantRequired)
				}
				if !tc.wantRequired {
					return
				}
				if out.Request.Kind != tc.wantKind {
					t.Errorf("Kind = %s, want %s", out.Request.Kind, tc.wantKind)
				}
				if out.Request.ID != tc.action.ID {
					t.Errorf("Request.ID = %q, want the action's %q so the journal can pair the events", out.Request.ID, tc.action.ID)
				}
				if out.Request.Reason == "" {
					t.Error("Request.Reason is empty; the journal records why consent was required")
				}
				if out.Request.Detail == "" {
					t.Error("Request.Detail is empty; nothing states what was consented to")
				}
			})
		}
	}
}

// TestWriteRequestShowsTheResolvedPath is the reason resolution happens before
// the question rather than after the answer: consenting to "../../etc/hosts" and
// consenting to "/etc/hosts" are the same act, and only one of them reads like
// it.
func TestWriteRequestShowsTheResolvedPath(t *testing.T) {
	d := newDirs(t)
	g := mustGate(t, d.root, fsResolver{}, mustAsk(t, permission.VerdictAllow))

	out, err := g.Check(t.Context(), permission.Action{
		ID: "1", Tool: "write_file", Operation: permission.OperationWrite,
		Path: filepath.Join(d.root, "escape", "loot.txt"),
	})
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	want := filepath.Join(d.outside, "loot.txt")
	if out.Request.Resolved != want {
		t.Errorf("Resolved = %q, want %q", out.Request.Resolved, want)
	}
	if out.Request.Detail != want {
		t.Errorf("Detail = %q, want the resolved path %q, not the model's spelling", out.Request.Detail, want)
	}
}

// TestZeroOutcomeIsDenial pins the property that lets a caller drop the outcome
// on an error path without opening a hole.
func TestZeroOutcomeIsDenial(t *testing.T) {
	var out permission.Outcome
	if out.Allowed {
		t.Fatal("the zero Outcome is allowed; a dropped value must fail closed")
	}
}

// TestFailClosed covers every way the gate can fail to establish an answer. All
// of them refuse. This is the guard the card asks to be seen red: invert any
// branch below in gate.go and the corresponding case reports an allowed action.
func TestFailClosed(t *testing.T) {
	d := newDirs(t)
	shell := permission.Action{ID: "1", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"ls"}, Dir: d.root}

	tests := []struct {
		name    string
		gate    func(*testing.T) *permission.Gate
		ctx     func(*testing.T) context.Context
		action  permission.Action
		wantErr error
	}{
		{
			name:    "a policy that decides nothing",
			gate:    func(t *testing.T) *permission.Gate { return mustGate(t, d.root, fsResolver{}, fixedPolicy{}) },
			action:  shell,
			wantErr: permission.ErrNoDecision,
		},
		{
			name: "an allow attributed to no one",
			gate: func(t *testing.T) *permission.Gate {
				return mustGate(t, d.root, fsResolver{}, fixedPolicy{dec: permission.Decision{Verdict: permission.VerdictAllow}})
			},
			action:  shell,
			wantErr: permission.ErrNoDecision,
		},
		{
			name: "a verdict outside the declared set",
			gate: func(t *testing.T) *permission.Gate {
				return mustGate(t, d.root, fsResolver{}, fixedPolicy{dec: permission.Decision{Verdict: permission.Verdict(9), Source: permission.SourceUser}})
			},
			action:  shell,
			wantErr: permission.ErrNoDecision,
		},
		{
			name: "a policy that errors",
			gate: func(t *testing.T) *permission.Gate {
				return mustGate(t, d.root, fsResolver{}, fixedPolicy{err: errors.New("stdin closed")})
			},
			action:  shell,
			wantErr: permission.ErrDenied,
		},
		{
			name: "a path that cannot be resolved",
			gate: func(t *testing.T) *permission.Gate {
				return mustGate(t, d.root, resolveOnly{only: d.root}, mustAsk(t, permission.VerdictAllow))
			},
			action:  permission.Action{ID: "2", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.root, "main.go")},
			wantErr: errUnresolvable,
		},
		{
			name: "a context cancelled before the question",
			gate: func(t *testing.T) *permission.Gate {
				return mustGate(t, d.root, fsResolver{}, mustAsk(t, permission.VerdictAllow))
			},
			ctx:    cancelled,
			action: shell,
			// A cancelled turn must not be the turn that runs the command,
			// however willing the policy is.
			wantErr: context.Canceled,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.ctx != nil {
				ctx = tc.ctx(t)
			}
			out, err := tc.gate(t).Check(ctx, tc.action)
			assertInvariants(t, out, err)
			if out.Allowed {
				t.Fatalf("allowed: %+v", out)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want one wrapping %v", err, tc.wantErr)
			}
		})
	}
}

func cancelled(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// fixedPolicy answers the same thing every time, including the answers a
// correct policy never gives.
type fixedPolicy struct {
	dec permission.Decision
	err error
}

func (p fixedPolicy) Decide(context.Context, permission.Request) (permission.Decision, error) {
	return p.dec, p.err
}

// TestConstructionRefusesMissingParts covers the arguments a gate cannot invent
// a safe default for.
func TestConstructionRefusesMissingParts(t *testing.T) {
	d := newDirs(t)
	policy := mustAsk(t, permission.VerdictAllow)

	tests := []struct {
		name     string
		root     string
		resolver permission.Resolver
		policy   permission.Policy
	}{
		{"no root", "", fsResolver{}, policy},
		{"no resolver", d.root, nil, policy},
		{"no policy", d.root, fsResolver{}, nil},
		{"a root that will not resolve", filepath.Join(d.root, "nope"), resolveOnly{only: d.root}, policy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := permission.New(tc.root, tc.resolver, tc.policy); err == nil {
				t.Fatal("New succeeded; a gate missing a part has no safe default")
			}
		})
	}
}

// TestRootIsResolvedThroughTheResolver: a repo reached by a symlinked path must
// not make every write inside it look like a write outside it.
func TestRootIsResolvedThroughTheResolver(t *testing.T) {
	d := newDirs(t)
	link := filepath.Join(filepath.Dir(d.root), "link-to-repo")
	if err := os.Symlink(d.root, link); err != nil {
		t.Fatalf("linking: %v", err)
	}

	g := mustGate(t, link, fsResolver{}, mustAsk(t, permission.VerdictDeny))
	if g.Root() != d.root {
		t.Fatalf("Root() = %q, want the resolved %q", g.Root(), d.root)
	}

	out, err := g.Check(t.Context(), permission.Action{
		ID: "1", Tool: "write_file", Operation: permission.OperationWrite,
		Path: filepath.Join(link, "main.go"),
	})
	assertInvariants(t, out, err)
	if !out.Allowed || out.Required {
		t.Fatalf("a write inside the root asked for consent: %+v (err %v)", out, err)
	}
}

// TestSessionGrantIsExactMatch pins the scope of "allow for this session". A
// grant that widened to a directory or a command prefix would convert one
// considered yes into standing consent for things nobody saw.
func TestSessionGrantIsExactMatch(t *testing.T) {
	d := newDirs(t)
	asker := &verdictAsker{verdict: permission.VerdictAllowSession}
	policy, err := permission.NewAsk(asker)
	if err != nil {
		t.Fatalf("building ask policy: %v", err)
	}
	g := mustGate(t, d.root, fsResolver{}, policy)

	test := permission.Action{ID: "1", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"go", "test", "./..."}, Dir: d.root}
	other := permission.Action{ID: "2", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"go", "test", "./...", ";", "curl", "evil"}, Dir: d.root}
	write := permission.Action{ID: "3", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.outside, "loot.txt")}

	for _, a := range []permission.Action{test, test, other, write, test} {
		out, err := g.Check(t.Context(), a)
		assertInvariants(t, out, err)
		if !out.Allowed {
			t.Fatalf("action %s refused: %v", a.ID, err)
		}
		if !out.Required {
			t.Fatalf("action %s stopped requiring consent; every gated action must still be journaled", a.ID)
		}
	}

	asked := asker.requests()
	if len(asked) != 3 {
		t.Fatalf("the human was asked %d times, want 3: the repeat of the granted command must not ask, and neither the longer command nor the write may ride on its grant", len(asked))
	}
	if asked[1].Detail == asked[0].Detail {
		t.Errorf("the second question repeated the granted command %q; a grant covering a superstring is a prefix grant", asked[0].Detail)
	}
	if asked[2].Kind != permission.KindWriteOutsideRoot {
		t.Errorf("third question was %s, want a write: a shell grant is not consent for anything else", asked[2].Kind)
	}
}

// TestSessionGrantReplaysAsAPlainAllow: the second and later actions covered by
// a grant are recorded as allowed by policy, not as the user answering again.
func TestSessionGrantReplaysAsAPlainAllow(t *testing.T) {
	d := newDirs(t)
	g := mustGate(t, d.root, fsResolver{}, mustAsk(t, permission.VerdictAllowSession))
	a := permission.Action{ID: "1", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"make", "test"}, Dir: d.root}

	first, err := g.Check(t.Context(), a)
	if err != nil {
		t.Fatalf("first check: %v", err)
	}
	if first.Decision.Verdict != permission.VerdictAllowSession || first.Decision.Source != permission.SourceUser {
		t.Fatalf("first decision = %+v, want the user's allow_session", first.Decision)
	}

	a.ID = "2"
	second, err := g.Check(t.Context(), a)
	if err != nil {
		t.Fatalf("second check: %v", err)
	}
	if second.Decision.Verdict != permission.VerdictAllow || second.Decision.Source != permission.SourcePolicy {
		t.Fatalf("second decision = %+v, want an allow attributed to policy", second.Decision)
	}
	if second.Request.ID != "2" {
		t.Errorf("Request.ID = %q, want the second action's own id", second.Request.ID)
	}
}

// TestConcurrentCheck exists for the race detector: the loop dispatches tools
// concurrently and session grants are shared state.
func TestConcurrentCheck(t *testing.T) {
	d := newDirs(t)
	g := mustGate(t, d.root, fsResolver{}, mustAsk(t, permission.VerdictAllowSession))

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := g.Check(t.Context(), permission.Action{
				ID: "c", Tool: "run_shell", Operation: permission.OperationShell,
				Command: []string{"go", "test", strconv.Itoa(i % 4)}, Dir: d.root,
			})
			if err != nil || !out.Allowed {
				t.Errorf("concurrent check refused: %v", err)
			}
		}()
	}
	wg.Wait()
}
