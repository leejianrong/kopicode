package permission_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
)

// TestAllowCommandsMatchesWhatTheModelActuallyTypes is issue #157: the model
// wraps and varies the command line, and an exact-match entry never lines up.
// The three lines are the ones the reporter saw refused. Each must now be
// permitted by the same short declaration — and the rows after them must still
// be refused, because a list that permits everything passes the first group.
func TestAllowCommandsMatchesWhatTheModelActuallyTypes(t *testing.T) {
	d := newDirs(t)
	p, err := permission.NewAllowlistCommands(d.root, fsResolver{}, [][]string{},
		[][]string{{"uv", "run", "pytest"}, {"ls"}, {"cat"}, {"git", "status"}, {"git", "log"}, {"head"}, {"tail"}})
	if err != nil {
		t.Fatalf("NewAllowlistCommands: %v", err)
	}
	out := d.outside

	tests := []struct {
		name string
		line string
		deny bool
	}{
		// From the report.
		{"reported: cd then test", "cd " + d.root + " && uv run pytest -v", false},
		{"reported: plain", "uv run pytest -v", false},
		{"reported: stderr merged and piped to head", "cd " + d.root + " && uv run pytest -v 2>&1 | head -100", false},

		// The same declaration, ordinary variations.
		{"bare entry", "uv run pytest", false},
		{"more flags", "uv run pytest -k 'a or b' tests/ --maxfail=1", false},
		{"assignment prefix", "LC_ALL=C ls", false},
		{"chain of listed commands", "git status && git log --oneline -5 | head", false},
		{"list with a semicolon", "ls; ls -la", false},
		{"redirect inside the root", "uv run pytest > results.txt", false},
		{"redirect to dev null", "git status 2>/dev/null", false},
		{"substitution of a listed command", "cat $(ls | head -1)", false},
		{"command by path", "/usr/bin/git status", false},

		// A listed command cannot carry an unlisted one.
		{"unlisted command after a listed one", "uv run pytest && rm -rf build", true},
		{"unlisted command after a semicolon", "uv run pytest; make install", true},
		{"unlisted command in a pipe", "uv run pytest | tee out.txt", true},
		{"unlisted command in a substitution", "ls $(make)", true},
		{"unlisted command in backticks", "ls `make`", true},
		{"unlisted command on a new line", "ls\nmake", true},
		{"unlisted command in a subshell", "(make)", true},
		{"unlisted command through env", "env make", true},
		{"unlisted command through xargs", "ls | xargs make", true},
		{"unlisted command in sh -c", "sh -c 'make'", true},
		{"listed command in sh -c is still a shell", "sh -c 'ls'", true},
		{"eval", "eval 'ls'", true},
		{"a prefix that is not a token prefix", "uv run pytestx", true},
		{"a different subcommand", "git push origin main", true},
		{"missing the subcommand", "uv", true},
		{"computed command name", "$CMD status", true},
		{"computed word where an entry word belongs", "git $SUB", true},
		{"unlisted first word of a listed second", "make ls", true},

		// The never-allow rules still run on top of an allowlist.
		{"sudo is refused even if its command is listed", "sudo ls", true},
		{"rm outside the root", "rm -rf " + out, true},
		{"redirect outside the root", "ls > " + filepath.Join(out, "f"), true},
		{"redirect through the escaping symlink", "ls > escape/f", true},
		{"cd outside the root", "cd " + out + " && ls", true},
		{"cd through dot dot", "cd .. && ls", true},
		{"cd to a computed place", "cd $X && ls", true},
		{"bare cd", "cd && ls", true},
		{"cd then a relative redirect outside", "cd .. && ls > f", true},

		// Fail closed.
		{"heredoc", "cat <<EOF\nhi\nEOF", true},
		{"unterminated quote", "ls 'oops", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := p.Decide(context.Background(), shellReq(d.root, tc.line))
			if err != nil {
				t.Fatalf("Decide(%q): %v", tc.line, err)
			}
			if dec.Source != permission.SourcePolicy {
				t.Errorf("source = %s, want policy", dec.Source)
			}
			want := permission.VerdictAllow
			if tc.deny {
				want = permission.VerdictDeny
			}
			if dec.Verdict != want {
				t.Errorf("Decide(%q) = %s (%s), want %s", tc.line, dec.Verdict, dec.Reason, want)
			}
		})
	}
}

// TestAllowCommandsCdInsideTheRootIsImplicit: a cd to a subdirectory of the
// declared root needs no entry, and the relative paths after it are judged from
// there.
func TestAllowCommandsCdInsideTheRootIsImplicit(t *testing.T) {
	d := newDirs(t)
	p, err := permission.NewAllowlistCommands(d.root, fsResolver{}, nil, [][]string{{"ls"}})
	if err != nil {
		t.Fatalf("NewAllowlistCommands: %v", err)
	}
	for line, deny := range map[string]bool{
		"cd pkg && ls":    false,
		"cd ./pkg/ && ls": false,
		// Conservative by design: cd is tracked as a set of possible directories
		// (a subshell's cd may not persist), so backing out of a subdirectory is
		// judged from the starting directory too and leaves the root.
		"cd pkg && cd .. && ls": true,
		"cd pkg/../.. && ls":    true,
	} {
		dec, err := p.Decide(context.Background(), shellReq(d.root, line))
		if err != nil {
			t.Fatalf("Decide(%q): %v", line, err)
		}
		if got := dec.Verdict == permission.VerdictDeny; got != deny {
			t.Errorf("Decide(%q) denied=%v (%s), want %v", line, got, dec.Reason, deny)
		}
	}
}

// TestAllowCommandsKeepsExactMatchWorking: the exact-match key is untouched, and
// both may be declared together.
func TestAllowCommandsKeepsExactMatchWorking(t *testing.T) {
	d := newDirs(t)
	p, err := permission.NewAllowlistCommands(d.root, fsResolver{},
		[][]string{{"/bin/sh", "-c", "make install"}}, [][]string{{"ls"}})
	if err != nil {
		t.Fatalf("NewAllowlistCommands: %v", err)
	}
	for line, deny := range map[string]bool{
		"make install":     false, // exact entry: allowed although make is not a token entry
		"make install ":    true,  // a different byte string, not on the list
		"ls -la":           false,
		"make install; ls": true, // the exact entry covers one whole line only
	} {
		dec, err := p.Decide(context.Background(), shellReq(d.root, line))
		if err != nil {
			t.Fatalf("Decide(%q): %v", line, err)
		}
		if got := dec.Verdict == permission.VerdictDeny; got != deny {
			t.Errorf("Decide(%q) denied=%v (%s), want %v", line, got, dec.Reason, deny)
		}
	}
}

// TestAllowCommandsOnlyAnalysesTheShellWrapper: a shell argv that is not
// /bin/sh -c <line> has no command line to tokenize and is refused.
func TestAllowCommandsOnlyAnalysesTheShellWrapper(t *testing.T) {
	d := newDirs(t)
	p, err := permission.NewAllowlistCommands(d.root, fsResolver{}, nil, [][]string{{"ls"}})
	if err != nil {
		t.Fatalf("NewAllowlistCommands: %v", err)
	}
	for _, argv := range [][]string{
		{"ls"},
		{"/bin/sh", "-c"},
		{"/bin/bash", "-c", "ls"},
		{"/bin/sh", "-ec", "ls"},
		{"/bin/sh", "-c", "ls", "extra"},
	} {
		dec, err := p.Decide(context.Background(), permission.Request{
			Kind: permission.KindRunShell, Action: permission.Action{Command: argv, Dir: d.root},
		})
		if err != nil {
			t.Fatalf("Decide(%q): %v", argv, err)
		}
		if dec.Verdict != permission.VerdictDeny {
			t.Errorf("Decide(%q) = %s, want deny", argv, dec.Verdict)
		}
	}
	// And no working directory means no way to judge relative paths.
	dec, _ := p.Decide(context.Background(), permission.Request{
		Kind: permission.KindRunShell, Action: permission.Action{Command: []string{"/bin/sh", "-c", "ls"}},
	})
	if dec.Verdict != permission.VerdictDeny {
		t.Errorf("no working directory: verdict = %s, want deny", dec.Verdict)
	}
}

func TestAllowCommandsRefusesEntriesThatRunOtherCommands(t *testing.T) {
	d := newDirs(t)
	for _, entry := range [][]string{
		{"env"}, {"xargs", "ls"}, {"sh"}, {"bash", "-c"}, {"eval"}, {"sudo"}, {"find"}, {"timeout"},
		{"nice"}, {"/usr/bin/env"}, {"source"}, {"."}, {"cd"}, {"exec"}, {}, {""},
	} {
		if _, err := permission.NewAllowlistCommands(d.root, fsResolver{}, nil, [][]string{entry}); err == nil {
			t.Errorf("entry %q accepted", entry)
		} else if len(entry) > 0 && entry[0] != "" && !strings.Contains(err.Error(), "allow_commands") {
			t.Errorf("entry %q: error %q does not name the key", entry, err)
		}
	}
}

// TestAllowCommandsThroughTheGate: an allowed line reaches the journal as a
// policy decision with its own reason, and a refusal names what was not listed.
func TestAllowCommandsThroughTheGate(t *testing.T) {
	d := newDirs(t)
	p, err := permission.NewAllowlistCommands(d.root, fsResolver{}, nil, [][]string{{"uv", "run", "pytest"}})
	if err != nil {
		t.Fatalf("NewAllowlistCommands: %v", err)
	}
	g := mustGate(t, d.root, fsResolver{}, p)
	out, err := g.Check(context.Background(), permission.Action{
		ID: "c1", Tool: "run_shell", Operation: permission.OperationShell,
		Command: []string{"/bin/sh", "-c", "cd " + d.root + " && uv run pytest -v"}, Dir: d.root,
	})
	if err != nil || !out.Allowed || out.Decision.Source != permission.SourcePolicy {
		t.Fatalf("allowed line: out=%+v err=%v", out, err)
	}
	out, err = g.Check(context.Background(), permission.Action{
		ID: "c2", Tool: "run_shell", Operation: permission.OperationShell,
		Command: []string{"/bin/sh", "-c", "uv run pytest && make"}, Dir: d.root,
	})
	if err == nil || out.Allowed || !strings.Contains(out.Decision.Reason, "make") {
		t.Errorf("refused line: out=%+v err=%v, want a refusal naming make", out, err)
	}
}
