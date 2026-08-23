package permission_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
)

func mustAllowlist(t *testing.T, root string, allow [][]string) *permission.AllowlistPolicy {
	t.Helper()
	p, err := permission.NewAllowlist(root, fsResolver{}, allow)
	if err != nil {
		t.Fatalf("building allowlist policy: %v", err)
	}
	return p
}

// TestAllowlistDecidesExactArgvAndDeclaredRoot is the safety property
// AllowlistPolicy exists for (ADR-0011 decision 1): a shell command matches
// the declared allowlist exactly or it is refused, and a write matches the
// declared root or it is refused — no partial credit for either.
func TestAllowlistDecidesExactArgvAndDeclaredRoot(t *testing.T) {
	d := newDirs(t)
	allowed := [][]string{
		{"go", "test", "./..."},
		{"make", "test"},
	}
	policy := mustAllowlist(t, d.root, allowed)

	inside := filepath.Join(d.root, "pkg", "new.go")
	outside := filepath.Join(d.outside, "loot.txt")

	tests := []struct {
		name string
		req  permission.Request
		want permission.Verdict
	}{
		{
			name: "an exact-match allowed command",
			req: permission.Request{
				Kind:   permission.KindRunShell,
				Action: permission.Action{Command: []string{"go", "test", "./..."}, Dir: d.root},
			},
			want: permission.VerdictAllow,
		},
		{
			name: "a second exact-match allowed command",
			req: permission.Request{
				Kind:   permission.KindRunShell,
				Action: permission.Action{Command: []string{"make", "test"}, Dir: d.root},
			},
			want: permission.VerdictAllow,
		},
		{
			name: "an extra argument is not the same command",
			req: permission.Request{
				Kind:   permission.KindRunShell,
				Action: permission.Action{Command: []string{"go", "test", "./...", "-v"}, Dir: d.root},
			},
			want: permission.VerdictDeny,
		},
		{
			name: "a missing argument is not the same command",
			req: permission.Request{
				Kind:   permission.KindRunShell,
				Action: permission.Action{Command: []string{"go", "test"}, Dir: d.root},
			},
			want: permission.VerdictDeny,
		},
		{
			name: "reordered arguments are not the same command",
			req: permission.Request{
				Kind:   permission.KindRunShell,
				Action: permission.Action{Command: []string{"test", "go", "./..."}, Dir: d.root},
			},
			want: permission.VerdictDeny,
		},
		{
			name: "an unrelated command",
			req: permission.Request{
				Kind:   permission.KindRunShell,
				Action: permission.Action{Command: []string{"rm", "-rf", "/"}, Dir: d.root},
			},
			want: permission.VerdictDeny,
		},
		{
			name: "shell with no argv at all",
			req:  permission.Request{Kind: permission.KindRunShell, Action: permission.Action{Dir: d.root}},
			want: permission.VerdictDeny,
		},
		{
			name: "write inside the declared root",
			req:  permission.Request{Kind: permission.KindWriteOutsideRoot, Action: permission.Action{Path: inside}},
			want: permission.VerdictAllow,
		},
		{
			name: "write outside the declared root",
			req:  permission.Request{Kind: permission.KindWriteOutsideRoot, Action: permission.Action{Path: outside}},
			want: permission.VerdictDeny,
		},
		{
			name: "write through dot dot",
			req: permission.Request{
				Kind:   permission.KindWriteOutsideRoot,
				Action: permission.Action{Path: filepath.Join(d.root, "..", "outside", "loot.txt")},
			},
			want: permission.VerdictDeny,
		},
		{
			name: "write through the escaping symlink",
			req: permission.Request{
				Kind:   permission.KindWriteOutsideRoot,
				Action: permission.Action{Path: filepath.Join(d.root, "escape", "loot.txt")},
			},
			want: permission.VerdictDeny,
		},
		{
			name: "write with no path",
			req:  permission.Request{Kind: permission.KindWriteOutsideRoot, Action: permission.Action{}},
			want: permission.VerdictDeny,
		},
		{
			name: "a request with no kind",
			req:  permission.Request{Action: permission.Action{Dir: d.root}},
			want: permission.VerdictDeny,
		},
		{
			name: "a kind nobody wrote an allowlist rule for",
			req:  permission.Request{Kind: permission.Kind(42), Action: permission.Action{Dir: d.root}},
			want: permission.VerdictDeny,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := policy.Decide(t.Context(), tc.req)
			if err != nil {
				t.Fatalf("Decide failed: %v", err)
			}
			if dec.Verdict != tc.want {
				t.Fatalf("verdict = %s, want %s (reason: %q)", dec.Verdict, tc.want, dec.Reason)
			}
			// The guard-seen-red case: KindUnspecified and the undeclared kind
			// are the fail-closed default-deny path, proven denied here rather
			// than trusted from reading the switch statement.
			if dec.Source != permission.SourcePolicy {
				t.Errorf("source = %s, want policy: an auto-approval must never look like a human's consent", dec.Source)
			}
			if dec.Verdict == permission.VerdictDeny && dec.Reason == "" {
				t.Error("a refusal with no reason tells the failure classifier nothing")
			}
		})
	}
}

// TestAllowlistNeverGrantsForTheSession mirrors TestBenchNeverGrantsForTheSession:
// there is no human here to have given standing consent to, so every check on
// an approved command answers on its own merits.
func TestAllowlistNeverGrantsForTheSession(t *testing.T) {
	d := newDirs(t)
	policy := mustAllowlist(t, d.root, [][]string{{"go", "test"}})
	g := mustGate(t, d.root, fsResolver{}, policy)
	a := permission.Action{ID: "1", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"go", "test"}, Dir: d.root}

	for i := range 3 {
		out, err := g.Check(t.Context(), a)
		if err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		if out.Decision.Verdict != permission.VerdictAllow {
			t.Fatalf("check %d verdict = %s, want a plain allow", i, out.Decision.Verdict)
		}
		if out.Decision.Source != permission.SourcePolicy {
			t.Fatalf("check %d source = %s, want policy", i, out.Decision.Source)
		}
	}
}

// TestAllowlistRefusesConstructionWithoutARoot mirrors
// TestBenchRefusesConstructionWithoutAWorktree: a policy with no declared root
// has nothing to confine writes to.
func TestAllowlistRefusesConstructionWithoutARoot(t *testing.T) {
	if _, err := permission.NewAllowlist("", fsResolver{}, nil); err == nil {
		t.Error("NewAllowlist(\"\") succeeded")
	}
	if _, err := permission.NewAllowlist("/somewhere", nil, nil); err == nil {
		t.Error("NewAllowlist with no resolver succeeded")
	}
	d := newDirs(t)
	if _, err := permission.NewAllowlist(filepath.Join(d.root, "nope"), resolveOnly{only: d.root}, nil); err == nil {
		t.Error("NewAllowlist with an unresolvable root succeeded")
	}
}

// TestAllowlistRefusesAnEmptyArgvEntry: a declared command with no argv[0] is
// not a command anything could ever exact-match, so it is refused at
// construction rather than silently never matching.
func TestAllowlistRefusesAnEmptyArgvEntry(t *testing.T) {
	d := newDirs(t)
	if _, err := permission.NewAllowlist(d.root, fsResolver{}, [][]string{{}}); err == nil {
		t.Error("NewAllowlist with an empty argv entry succeeded")
	}
}

// TestAllowlistAcceptsAnEmptyDeclaredAllowlist: unlike internal/harness's
// `verify` key, an empty allow list is a legitimate declaration — "never
// approve a shell command" — not a misconfiguration.
func TestAllowlistAcceptsAnEmptyDeclaredAllowlist(t *testing.T) {
	d := newDirs(t)
	policy := mustAllowlist(t, d.root, nil)

	dec, err := policy.Decide(t.Context(), permission.Request{
		Kind:   permission.KindRunShell,
		Action: permission.Action{Command: []string{"echo", "hi"}, Dir: d.root},
	})
	if err != nil {
		t.Fatalf("Decide failed: %v", err)
	}
	if dec.Verdict != permission.VerdictDeny {
		t.Errorf("verdict = %s, want deny: nothing is declared allowed", dec.Verdict)
	}

	// A write inside the root is still permitted; the empty allow list only
	// closes off shell, not the write scope.
	dec, err = policy.Decide(t.Context(), permission.Request{
		Kind:   permission.KindWriteOutsideRoot,
		Action: permission.Action{Path: filepath.Join(d.root, "new.go")},
	})
	if err != nil {
		t.Fatalf("Decide failed: %v", err)
	}
	if dec.Verdict != permission.VerdictAllow {
		t.Errorf("verdict = %s, want allow: an empty shell allowlist says nothing about the write scope", dec.Verdict)
	}
}

// TestAllowlistDoesNotConfineShellToTheRoot: unlike BenchPolicy, a shell
// command's Dir is not checked against the declared root — the two mechanisms
// ADR-0011 decision 1 lists (a closed argv set, and a write-confinement root)
// stay apart, so an approved command running outside the declared root is
// still approved on argv alone. Containing what a running command does is
// explicitly out of scope (ADR-0011 decision 4).
func TestAllowlistDoesNotConfineShellToTheRoot(t *testing.T) {
	d := newDirs(t)
	policy := mustAllowlist(t, d.root, [][]string{{"go", "test"}})

	dec, err := policy.Decide(t.Context(), permission.Request{
		Kind:   permission.KindRunShell,
		Action: permission.Action{Command: []string{"go", "test"}, Dir: d.outside},
	})
	if err != nil {
		t.Fatalf("Decide failed: %v", err)
	}
	if dec.Verdict != permission.VerdictAllow {
		t.Errorf("verdict = %s, want allow: argv is the only thing gating a shell command here", dec.Verdict)
	}
}

// TestAllowlistSurfacesAResolverFailure mirrors
// TestBenchSurfacesAResolverFailure: a write whose path this policy cannot
// resolve is not a write it can approve.
func TestAllowlistSurfacesAResolverFailure(t *testing.T) {
	d := newDirs(t)
	p, err := permission.NewAllowlist(d.root, resolveOnly{only: d.root}, nil)
	if err != nil {
		t.Fatalf("building allowlist policy: %v", err)
	}
	dec, err := p.Decide(t.Context(), permission.Request{
		Kind:   permission.KindWriteOutsideRoot,
		Action: permission.Action{Path: filepath.Join(d.root, "pkg", "new.go")},
	})
	if !errors.Is(err, errUnresolvable) {
		t.Fatalf("err = %v, want the resolver's failure", err)
	}
	if dec.Verdict == permission.VerdictAllow {
		t.Fatal("approved a path it could not resolve")
	}
}
