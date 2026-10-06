package permission_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
)

// TestReadOnlyGateRefusesEveryWriteWithoutAsking is ADR-0019's whole rule: a
// write is denied inside the root and outside it, with ErrReadOnly, and the
// policy is never consulted, because a policy that could answer allow would turn
// the declaration into a request. A read and a shell action are unchanged.
func TestReadOnlyGateRefusesEveryWriteWithoutAsking(t *testing.T) {
	d := newDirs(t)
	asker := &verdictAsker{verdict: permission.VerdictAllow}
	policy, err := permission.NewAsk(asker, permission.SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	g := mustGate(t, d.root, fsResolver{}, policy)
	g.SetReadOnly(true)

	for name, path := range map[string]string{
		"inside the root":   filepath.Join(d.root, "a.go"),
		"outside the root":  filepath.Join(d.outside, "a.go"),
		"through a symlink": filepath.Join(d.root, "escape", "a.go"),
	} {
		t.Run(name, func(t *testing.T) {
			out, err := g.Check(context.Background(), permission.Action{
				ID: "w", Tool: "write_file", Operation: permission.OperationWrite, Path: path,
			})
			assertInvariants(t, out, err)
			if !errors.Is(err, permission.ErrReadOnly) || out.Allowed {
				t.Fatalf("write was not refused as read-only: allowed=%t err=%v", out.Allowed, err)
			}
		})
	}
	if n := len(asker.requests()); n != 0 {
		t.Errorf("the policy was asked %d time(s); a read-only refusal must not reach it", n)
	}

	out, err := g.Check(context.Background(), permission.Action{
		ID: "r", Tool: "read_file", Operation: permission.OperationRead, Path: filepath.Join(d.root, "a.go"),
	})
	if err != nil || !out.Allowed {
		t.Errorf("a read was refused in a read-only session: %v", err)
	}
	out, err = g.Check(context.Background(), permission.Action{
		ID: "s", Tool: "run_shell", Operation: permission.OperationShell, Command: []string{"/bin/sh", "-c", "true"},
	})
	if err != nil || !out.Allowed || !out.Required {
		t.Errorf("shell must still go through the policy untouched: allowed=%t required=%t err=%v", out.Allowed, out.Required, err)
	}
}

// TestGateIsWritableByDefault: SetReadOnly is opt-in, so the zero value of a
// gate behaves exactly as before ADR-0019.
func TestGateIsWritableByDefault(t *testing.T) {
	d := newDirs(t)
	g := mustGate(t, d.root, fsResolver{}, mustAsk(t, permission.VerdictDeny))
	out, err := g.Check(context.Background(), permission.Action{
		ID: "w", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.root, "a.go"),
	})
	if err != nil || !out.Allowed {
		t.Errorf("an in-root write was refused by a default gate: %v", err)
	}
}
