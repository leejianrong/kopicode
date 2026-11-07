package engine

import (
	"slices"
	"testing"
)

func claimPaths(text string, touched ...string) []string {
	held := map[string]bool{}
	for _, p := range touched {
		held[p] = true
	}
	var out []string
	for _, f := range unbackedClaims(text, held) {
		out = append(out, f.Path)
	}
	return out
}

func TestUnbackedClaims(t *testing.T) {
	list := "I renamed it everywhere:\n" +
		"1. **README.md** - Updated the documentation reference\n" +
		"2. `accounts/store.py` - Renamed the definition\n" +
		"3. `tests/test_accounts.py` - Updated all test calls\n"
	cases := []struct {
		name    string
		text    string
		touched []string
		want    []string
	}{
		{"every claimed file but the one edited", list, []string{"README.md"},
			[]string{"accounts/store.py", "tests/test_accounts.py"}},
		{"all backed", list, []string{"README.md", "accounts/store.py", "tests/test_accounts.py"}, nil},
		{"a base name matches a longer touched path", "`store.py` - Renamed the definition", []string{"accounts/store.py"}, nil},
		{"a longer name matches a shorter touched path", "`accounts/store.py` - Renamed the definition", []string{"store.py"}, nil},
		{"the file as subject, verb first, is no claim",
			"I fixed the failing test in `tests/test_pagination.py` by correcting `shop/pagination.py`.", []string{"shop/pagination.py"}, nil},
		{"named without a change verb", "The code lives in accounts/store.py and nowhere else.", nil, nil},
		{"the verb is on another line", "accounts/store.py\nwas updated", nil, nil},
		{"a verb too far after the name", "accounts/store.py " + string(make([]byte, 0)) +
			"has many things in it that are described at some length before anything: updated", nil, nil},
		{"an absolute path is not a repository file", "updated /etc/hosts.txt and /tmp/x/store.py - updated", nil, nil},
		{"a version number is not a file", "bumped to v1.2.3 and updated the docs", nil, nil},
		{"a dotted-path prefix is trimmed", "./accounts/store.py - updated", []string{"accounts/store.py"}, nil},
		{"a suggestion is not a claim",
			"The fix belongs in `tests/test_ops.py`: line 12 should assert `double(2) == 4` (or be deleted).", nil, nil},
		{"a plain past-tense claim still is one", "`tests/test_ops.py` was deleted", nil, []string{"tests/test_ops.py"}},
		{"duplicates count once", "store.py - updated; store.py - updated again", nil, []string{"store.py"}},
	}
	for _, c := range cases {
		if got := claimPaths(c.text, c.touched...); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestUnbackedClaimsAreCapped(t *testing.T) {
	var text string
	for i := range 14 {
		text += string(rune('a'+i)) + ".py - updated\n"
	}
	if got := claimPaths(text); len(got) != maxClaimedFiles {
		t.Errorf("got %d files, want the cap %d", len(got), maxClaimedFiles)
	}
}

func TestTheSpanIsTheNameThroughTheVerb(t *testing.T) {
	got := unbackedClaims("1. **accounts/store.py** - Updated the definition", nil)
	if len(got) != 1 || got[0].Span != "accounts/store.py** - Updated" {
		t.Errorf("got %+v, want the span from the name through the verb", got)
	}
}

func TestAShellTurnWroteOnlyWhenTheTreeSaysSo(t *testing.T) {
	cases := []struct {
		name  string
		steps []func(c *claimTracker)
		want  bool
	}{
		{"first shell turn has no tree to compare", []func(c *claimTracker){shellTurn("t1")}, true},
		{"same tree after a shell turn: nothing written", []func(c *claimTracker){editTurn("t0"), shellTurn("t0")}, false},
		{"a different tree after a shell turn", []func(c *claimTracker){editTurn("t0"), shellTurn("t1")}, true},
		{"an edit and a shell in one turn cannot be told apart", []func(c *claimTracker){
			editTurn("t0"),
			func(c *claimTracker) { c.noteCall("edit_file"); c.noteCall("run_shell"); c.noteSnapshot("t0") },
		}, true},
		{"no snapshotter", []func(c *claimTracker){editTurn("t0"), shellTurn("")}, true},
		{"edits alone never mark a shell write", []func(c *claimTracker){editTurn("t0"), editTurn("t1")}, false},
	}
	for _, c := range cases {
		tr := &claimTracker{}
		tr.begin()
		for _, s := range c.steps {
			s(tr)
		}
		if tr.shellWrote != c.want {
			t.Errorf("%s: shellWrote = %v, want %v", c.name, tr.shellWrote, c.want)
		}
	}
}

func shellTurn(tree string) func(c *claimTracker) {
	return func(c *claimTracker) { c.noteCall("run_shell"); c.noteSnapshot(tree) }
}

func editTurn(tree string) func(c *claimTracker) {
	return func(c *claimTracker) { c.noteCall("edit_file"); c.noteSnapshot(tree) }
}
