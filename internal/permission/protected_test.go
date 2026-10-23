package permission_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
)

// protectedDirs is newDirs plus a tests directory, a lookalike directory whose
// name starts the same, and a symlink into tests.
func protectedDirs(t *testing.T) dirs {
	t.Helper()
	d := newDirs(t)
	for _, p := range []string{"tests", "tests_old", "src"} {
		if err := os.MkdirAll(filepath.Join(d.root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(d.root, "tests"), filepath.Join(d.root, "alias")); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestProtectedGateRefusesAWriteUnderAProtectedPath is ADR-0037's file-tool
// rule: a write under a protected path is denied with ErrProtectedPath, by the
// gate itself and not by a question to a policy that could say yes, and the
// refusal tells the model to stop and say so rather than work around it.
func TestProtectedGateRefusesAWriteUnderAProtectedPath(t *testing.T) {
	d := protectedDirs(t)
	asker := &verdictAsker{verdict: permission.VerdictAllow}
	policy, err := permission.NewAsk(asker, permission.SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	g := mustGate(t, d.root, fsResolver{}, policy)
	if _, err := g.SetProtected([]string{filepath.Join(d.root, "tests")}); err != nil {
		t.Fatalf("SetProtected: %v", err)
	}

	refused := map[string]string{
		"a file under it":           filepath.Join(d.root, "tests", "test_a.py"),
		"a new file deeper down":    filepath.Join(d.root, "tests", "new", "deeper", "t.py"),
		"the directory itself":      filepath.Join(d.root, "tests"),
		"a dotted detour":           filepath.Join(d.root, "src", "..", "tests", "x.py"),
		"through a symlink into it": filepath.Join(d.root, "alias", "x.py"),
	}
	for name, path := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			out, err := g.Check(context.Background(), permission.Action{
				ID: "w", Tool: "edit_file", Operation: permission.OperationWrite, Path: path,
			})
			assertInvariants(t, out, err)
			if !errors.Is(err, permission.ErrProtectedPath) || out.Allowed {
				t.Fatalf("write was not refused as protected: allowed=%t err=%v", out.Allowed, err)
			}
			if !strings.Contains(err.Error(), "stop and say so") || !strings.Contains(err.Error(), "do not work around it") {
				t.Errorf("the refusal does not tell the model what to do instead: %v", err)
			}
		})
	}
	if n := len(asker.requests()); n != 0 {
		t.Errorf("the policy was asked %d time(s); a protected-path refusal must not reach it", n)
	}

	allowed := map[string]string{
		"a lookalike sibling": filepath.Join(d.root, "tests_old", "a.py"),
		"ordinary source":     filepath.Join(d.root, "src", "a.py"),
	}
	for name, path := range allowed {
		t.Run("allows "+name, func(t *testing.T) {
			out, err := g.Check(context.Background(), permission.Action{
				ID: "w", Tool: "write_file", Operation: permission.OperationWrite, Path: path,
			})
			if err != nil || !out.Allowed {
				t.Errorf("a write outside the protected path was refused: %v", err)
			}
		})
	}

	out, err := g.Check(context.Background(), permission.Action{
		ID: "r", Tool: "read_file", Operation: permission.OperationRead, Path: filepath.Join(d.root, "tests", "test_a.py"),
	})
	if err != nil || !out.Allowed {
		t.Errorf("a read of a protected path was refused: %v", err)
	}
}

func TestSetProtectedRefusesWhatItCannotHonour(t *testing.T) {
	d := protectedDirs(t)
	g := mustGate(t, d.root, fsResolver{}, mustAsk(t, permission.VerdictDeny))
	for name, paths := range map[string][]string{
		"empty":          {""},
		"outside":        {d.outside},
		"through escape": {filepath.Join(d.root, "escape", "x")},
		"the root":       {d.root},
	} {
		if _, err := g.SetProtected(paths); err == nil {
			t.Errorf("%s: a protected path that cannot be honoured was accepted", name)
		}
	}
}

func TestAGateWithNoProtectedPathsIsUnchanged(t *testing.T) {
	d := protectedDirs(t)
	g := mustGate(t, d.root, fsResolver{}, mustAsk(t, permission.VerdictDeny))
	out, err := g.Check(context.Background(), permission.Action{
		ID: "w", Tool: "write_file", Operation: permission.OperationWrite, Path: filepath.Join(d.root, "tests", "a.py"),
	})
	if err != nil || !out.Allowed {
		t.Errorf("an unprotected session refused a write: %v", err)
	}
}

// protectedLines is the shell half of ADR-0037: what the tokenizer can see.
// The allow rows matter as much as the deny rows, because a guard that refuses
// every line passes every deny row.
var protectedLines = []struct {
	name string
	line string
	deny bool
}{
	{"run the tests", "python3 -m unittest discover -s tests", false},
	{"list the directory", "ls tests && cat tests/test_a.py | head -5", false},
	{"grep it", "grep -rn double tests/", false},
	{"write elsewhere in the root", "echo hi > src/out.txt && touch src/new.py", false},
	{"write to a lookalike sibling", "echo hi > tests_old/x && rm tests_old/x", false},
	{"sed without -i prints", "sed 's/a/b/' tests/test_a.py", false},
	{"copy out of it", "cp tests/test_a.py src/copy.py", false},
	{"cat a protected file into a pipe", "cat tests/test_a.py | wc -l", false},

	{"redirect into it", "echo x > tests/test_a.py", true},
	{"append into it", "echo x >> tests/test_a.py", true},
	{"heredoc into it", "cat > tests/new.py <<EOF\nx\nEOF", true},
	{"sed -i", "sed -i 's/5/4/' tests/test_a.py", true},
	{"sed -i with a suffix", "sed -i.bak 's/5/4/' tests/test_a.py", true},
	{"sed combined flags", "sed -ni 's/5/4/p' tests/test_a.py", true},
	{"sed --in-place", "sed --in-place=.bak 's/5/4/' tests/test_a.py", true},
	{"tee into it", "echo x | tee tests/test_a.py", true},
	{"touch in it", "touch tests/new.py", true},
	{"truncate it", "truncate -s 0 tests/test_a.py", true},
	{"rm a file in it", "rm tests/test_a.py", true},
	{"rm -rf it", "rm -rf tests", true},
	{"rm -rf a directory that holds it", "rm -rf .", true},
	{"mv it away", "mv tests tests_gone", true},
	{"mv something over it", "mv src/a.py tests/test_a.py", true},
	{"cp into it", "cp src/a.py tests/test_a.py", true},
	{"cp -t into it", "cp -t tests src/a.py", true},
	{"cd then write relative", "cd tests && touch x.py", true},
	{"through env", "env FOO=1 sed -i s/a/b/ tests/test_a.py", true},
	{"through sh -c", "sh -c 'echo x > tests/a.py'", true},
	{"inside a substitution", "echo $(touch tests/a.py)", true},
	{"after a separator", "go test ./... ; rm tests/test_a.py", true},
	{"a target computed at run time", "sed -i s/a/b/ $F", true},
}

func TestAutoRefusesShellWritesUnderAProtectedPath(t *testing.T) {
	d := protectedDirs(t)
	p, err := permission.NewAuto(d.root, fsResolver{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := mustGate(t, d.root, fsResolver{}, p)
	abs, err := g.SetProtected([]string{filepath.Join(d.root, "tests")})
	if err != nil {
		t.Fatal(err)
	}
	permission.ProtectShell(p, abs)

	for _, tc := range protectedLines {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := p.Decide(context.Background(), shellReq(d.root, tc.line))
			if err != nil {
				t.Fatal(err)
			}
			if got := dec.Verdict == permission.VerdictDeny; got != tc.deny {
				t.Fatalf("%q: denied=%t, want %t (reason: %s)", tc.line, got, tc.deny, dec.Reason)
			}
			if tc.deny && !strings.Contains(dec.Reason, "protected") && !strings.Contains(dec.Reason, "cannot be") {
				t.Errorf("%q: the reason does not say why: %s", tc.line, dec.Reason)
			}
		})
	}
}

// TestProtectedShellRulesAreOffWhenNothingIsProtected: the same lines are the
// auto policy's own business when no path is protected, so a session that
// declares nothing behaves exactly as before ADR-0037.
func TestProtectedShellRulesAreOffWhenNothingIsProtected(t *testing.T) {
	d := protectedDirs(t)
	p, err := permission.NewAuto(d.root, fsResolver{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"sed -i s/a/b/ tests/test_a.py", "echo x > tests/a.py", "rm -rf tests"} {
		dec, err := p.Decide(context.Background(), shellReq(d.root, line))
		if err != nil || dec.Verdict != permission.VerdictAllow {
			t.Errorf("%q was refused with nothing protected: %v %s", line, err, dec.Reason)
		}
	}
}

func TestProtectedPathsReachTheAllowlistAndTheSwitch(t *testing.T) {
	d := protectedDirs(t)
	g := mustGate(t, d.root, fsResolver{}, mustAsk(t, permission.VerdictDeny))
	abs, err := g.SetProtected([]string{filepath.Join(d.root, "tests")})
	if err != nil {
		t.Fatal(err)
	}
	line := "sed -i s/5/4/ tests/test_a.py"

	al, err := permission.NewAllowlistCommands(d.root, fsResolver{}, nil, [][]string{{"sed"}, {"echo"}})
	if err != nil {
		t.Fatal(err)
	}
	permission.ProtectShell(al, abs)
	if dec, _ := al.Decide(context.Background(), shellReq(d.root, line)); dec.Verdict != permission.VerdictDeny {
		t.Errorf("the allowlist policy let a protected write through: %s", dec.Reason)
	}
	if dec, _ := al.Decide(context.Background(), shellReq(d.root, "echo hi")); dec.Verdict != permission.VerdictAllow {
		t.Errorf("the allowlist policy refused an allowed, unprotected command: %s", dec.Reason)
	}

	auto, err := permission.NewAuto(d.root, fsResolver{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sw, err := permission.NewSwitch(mustAsk(t, permission.VerdictDeny), auto, true)
	if err != nil {
		t.Fatal(err)
	}
	permission.ProtectShell(sw, abs)
	if dec, _ := sw.Decide(context.Background(), shellReq(d.root, line)); dec.Verdict != permission.VerdictDeny {
		t.Errorf("a switch in auto let a protected write through: %s", dec.Reason)
	}
}
