package permission_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
)

// TestBenchRefusesOutsideTheWorktree is the safety property the bench policy
// exists for, tested against the policy directly rather than through a gate
// whose root happens to be the same directory.
//
// The tempting headless policy — "there is nobody to ask, so approve" — passes
// every other test in this file. It fails these rows, which is the point: a
// task allowed to write or run outside its worktree can reach the next task's
// checkout, and paired scoring over tasks that can touch each other is not
// measuring what it claims to.
func TestBenchRefusesOutsideTheWorktree(t *testing.T) {
	d := newDirs(t)
	policy, err := permission.NewBench(d.root, fsResolver{})
	if err != nil {
		t.Fatalf("building bench policy: %v", err)
	}

	inside := filepath.Join(d.root, "pkg", "new.go")
	outside := filepath.Join(d.outside, "loot.txt")

	tests := []struct {
		name string
		req  permission.Request
		want permission.Verdict
	}{
		{
			name: "shell in the worktree",
			req:  permission.Request{Kind: permission.KindRunShell, Action: permission.Action{Dir: d.root}},
			want: permission.VerdictAllow,
		},
		{
			name: "shell in a subdirectory of the worktree",
			req:  permission.Request{Kind: permission.KindRunShell, Action: permission.Action{Dir: filepath.Join(d.root, "pkg")}},
			want: permission.VerdictAllow,
		},
		{
			name: "shell outside the worktree",
			req:  permission.Request{Kind: permission.KindRunShell, Action: permission.Action{Dir: d.outside}},
			want: permission.VerdictDeny,
		},
		{
			name: "shell reaching out with dot dot",
			req:  permission.Request{Kind: permission.KindRunShell, Action: permission.Action{Dir: filepath.Join(d.root, "..", "outside")}},
			want: permission.VerdictDeny,
		},
		{
			name: "shell through the escaping symlink",
			req:  permission.Request{Kind: permission.KindRunShell, Action: permission.Action{Dir: filepath.Join(d.root, "escape")}},
			want: permission.VerdictDeny,
		},
		{
			name: "shell with no working directory at all",
			req:  permission.Request{Kind: permission.KindRunShell, Action: permission.Action{}},
			want: permission.VerdictDeny,
		},
		{
			name: "write inside the worktree",
			req:  permission.Request{Kind: permission.KindWriteOutsideRoot, Action: permission.Action{Path: inside}},
			want: permission.VerdictAllow,
		},
		{
			name: "write outside the worktree",
			req:  permission.Request{Kind: permission.KindWriteOutsideRoot, Action: permission.Action{Path: outside}},
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
			name: "a kind nobody wrote a bench rule for",
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
			if dec.Source != permission.SourcePolicy {
				t.Errorf("source = %s, want policy: an auto-approval must never look like a human's consent", dec.Source)
			}
			if dec.Verdict == permission.VerdictDeny && dec.Reason == "" {
				t.Error("a refusal with no reason tells the failure classifier nothing")
			}
		})
	}
}

// TestBenchNeverGrantsForTheSession: standing consent is a convenience for a
// human being asked repeatedly, and answering each request on its own merits is
// what puts a decision in the journal for every gated action.
func TestBenchNeverGrantsForTheSession(t *testing.T) {
	d := newDirs(t)
	g := mustGate(t, d.root, fsResolver{}, mustBench(t, d.root))
	a := permission.Action{ID: "1", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"go", "test"}, Dir: d.root}

	for i := range 3 {
		out, err := g.Check(t.Context(), a)
		if err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		if out.Decision.Verdict != permission.VerdictAllow {
			t.Fatalf("check %d verdict = %s, want a plain allow", i, out.Decision.Verdict)
		}
		if !out.Required {
			t.Fatalf("check %d stopped requiring consent", i)
		}
	}
}

// TestBenchRefusesConstructionWithoutAWorktree: a bench policy with no worktree
// has nothing to confine to, and the only default it could pick is "everywhere".
func TestBenchRefusesConstructionWithoutAWorktree(t *testing.T) {
	if _, err := permission.NewBench("", fsResolver{}); err == nil {
		t.Error("NewBench(\"\") succeeded")
	}
	if _, err := permission.NewBench("/somewhere", nil); err == nil {
		t.Error("NewBench with no resolver succeeded")
	}
	d := newDirs(t)
	if _, err := permission.NewBench(filepath.Join(d.root, "nope"), resolveOnly{only: d.root}); err == nil {
		t.Error("NewBench with an unresolvable worktree succeeded")
	}
}

// TestBenchSurfacesAResolverFailure: a path it cannot judge is not a path it
// approves. The error reaches the gate, which refuses.
func TestBenchSurfacesAResolverFailure(t *testing.T) {
	d := newDirs(t)
	policy, err := permission.NewBench(d.root, resolveOnly{only: d.root})
	if err != nil {
		t.Fatalf("building bench policy: %v", err)
	}
	dec, err := policy.Decide(t.Context(), permission.Request{
		Kind:   permission.KindRunShell,
		Action: permission.Action{Dir: filepath.Join(d.root, "pkg")},
	})
	if !errors.Is(err, errUnresolvable) {
		t.Fatalf("err = %v, want the resolver's failure", err)
	}
	if dec.Verdict == permission.VerdictAllow {
		t.Fatal("approved a path it could not resolve")
	}
}

// TestAskAttributesToTheUser: whatever the human said, the journal has to be
// able to say a human said it.
func TestAskAttributesToTheUser(t *testing.T) {
	for _, v := range []permission.Verdict{permission.VerdictAllow, permission.VerdictDeny, permission.VerdictAllowSession} {
		t.Run(v.String(), func(t *testing.T) {
			policy, err := permission.NewAsk(&verdictAsker{verdict: v})
			if err != nil {
				t.Fatalf("building ask policy: %v", err)
			}
			dec, err := policy.Decide(t.Context(), permission.Request{Kind: permission.KindRunShell})
			if err != nil {
				t.Fatalf("Decide failed: %v", err)
			}
			if dec.Verdict != v {
				t.Errorf("verdict = %s, want %s", dec.Verdict, v)
			}
			if dec.Source != permission.SourceUser {
				t.Errorf("source = %s, want user", dec.Source)
			}
		})
	}
}

// TestAskSurfacesTheAskerFailure: an unanswerable question is not a yes.
func TestAskSurfacesTheAskerFailure(t *testing.T) {
	sentinel := errors.New("stdin closed")
	policy, err := permission.NewAsk(&verdictAsker{verdict: permission.VerdictAllow, err: sentinel})
	if err != nil {
		t.Fatalf("building ask policy: %v", err)
	}
	dec, err := policy.Decide(t.Context(), permission.Request{Kind: permission.KindRunShell})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the asker's failure", err)
	}
	if dec.Verdict == permission.VerdictAllow {
		t.Fatal("returned an allow the asker never gave")
	}
}

// TestAskRefusesConstructionWithoutAnAsker: an interactive policy with nobody
// to ask can only invent an answer.
func TestAskRefusesConstructionWithoutAnAsker(t *testing.T) {
	if _, err := permission.NewAsk(nil); err == nil {
		t.Fatal("NewAsk(nil) succeeded")
	}
}

// TestAskerSeesTheRequestUnchanged: the surface renders the request, so it has
// to receive the whole of it — including the structured action, so a prompt can
// show argv rather than re-splitting a string.
func TestAskerSeesTheRequestUnchanged(t *testing.T) {
	d := newDirs(t)
	asker := &verdictAsker{verdict: permission.VerdictAllow}
	policy, err := permission.NewAsk(asker)
	if err != nil {
		t.Fatalf("building ask policy: %v", err)
	}
	g := mustGate(t, d.root, fsResolver{}, policy)

	a := permission.Action{ID: "42", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"go", "test", "./..."}, Dir: d.root}
	if _, err := g.Check(t.Context(), a); err != nil {
		t.Fatalf("Check failed: %v", err)
	}

	got := asker.requests()
	if len(got) != 1 {
		t.Fatalf("asked %d times, want 1", len(got))
	}
	if got[0].ID != "42" || got[0].Kind != permission.KindRunShell {
		t.Errorf("request = %+v, want the shell request with the action's id", got[0])
	}
	if got[0].Detail != "go test ./..." {
		t.Errorf("Detail = %q, want the command line", got[0].Detail)
	}
	if len(got[0].Action.Command) != 3 {
		t.Errorf("Action.Command = %v, want the argv unjoined so a surface need not re-split it", got[0].Action.Command)
	}
}

// TestPolicyIsAValue is the seam the card is about, stated as a test: an
// unrelated third policy drops in without this package changing.
func TestPolicyIsAValue(t *testing.T) {
	d := newDirs(t)
	g := mustGate(t, d.root, fsResolver{}, dryRun{})

	out, err := g.Check(t.Context(), permission.Action{
		ID: "1", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"ls"}, Dir: d.root,
	})
	assertInvariants(t, out, err)
	if out.Allowed {
		t.Fatal("the dry-run policy allowed a shell command")
	}
	if out.Decision.Reason != "dry run" {
		t.Errorf("reason = %q, want the policy's own", out.Decision.Reason)
	}
}

// dryRun refuses everything, which is a plausible third policy: a --dry-run
// mode that lets a turn run to the point of its first side effect.
type dryRun struct{}

func (dryRun) Decide(context.Context, permission.Request) (permission.Decision, error) {
	return permission.Decision{Verdict: permission.VerdictDeny, Source: permission.SourcePolicy, Reason: "dry run"}, nil
}
