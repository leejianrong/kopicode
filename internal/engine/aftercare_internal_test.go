package engine

import (
	"slices"
	"strings"
	"testing"
)

// The diffs below are the journaled ones: Codex-driven s1 and s2 (seq 33 and 16 of
// docs/dogfood-runs/2026-10-26-codex-driver-s1-s5) and the `def`-eaten shape of
// `runs/main-3`.
const (
	diffS1    = "--- a/shop/pagination.py\n+++ b/shop/pagination.py\n@@ -1,7 +1,16 @@\n \"\"\"Pagination helpers for the product listing.\"\"\"\n \n \n-def page_count(total_items: int, per_page: int) -> int:\n+\"\"\"Pagination helpers for the product listing.\"\"\"\n+\n+import math\n+\n+\n+def page_count(total_items: int, per_page: int) -> int:\n+    \"\"\"How many pages are needed to show total_items, per_page at a time.\"\"\"\n+    if per_page <= 0:\n+        raise ValueError(\"per_page must be positive\")\n+    return math.ceil(total_items / per_page)\n     \"\"\"How many pages are needed to show total_items, per_page at a time.\"\"\"\n     if per_page <= 0:\n         raise ValueError(\"per_page must be positive\")\n"
	diffS2    = "--- a/textutil/lists.py\n+++ b/textutil/lists.py\n@@ -6,6 +6,15 @@\n     return items[0] if items else default\n \n \n-def last(items: list, default=None):\n+def last(items: list, default=None):\n+    \"\"\"The last item, or default for an empty list.\"\"\"\n+    return items[-1] if items else default\n+\n+\n+def chunk(items: list, size: int) -> list:\n+    \"\"\"Split a list into consecutive lists of at most size items.\"\"\"\n+    if size <= 0:\n+        raise ValueError(\"size must be positive\")\n+    return [items[i:i + size] for i in range(0, len(items), size)]\n     \"\"\"The last item, or default for an empty list.\"\"\"\n     return items[-1] if items else default\n"
	diffMain3 = "--- a/accounts/report.py\n+++ b/accounts/report.py\n@@ -1,5 +1,5 @@\n from accounts import store\n \n \n-def names(ids: list) -> list:\n+    return [store.get_user(i) for i in ids]\n     return [store.fetch_user(i) for i in ids]\n"
)

func pairs(d []deadLine) [][2]int {
	var out [][2]int
	for _, x := range d {
		out = append(out, [2]int{x.line, x.next})
	}
	return out
}

func TestDeadAfterExitFindsTheRecordedCases(t *testing.T) {
	// Line numbers are the edited file's, counted from each hunk's `+N` start.
	cases := []struct {
		name, path, diff string
		want             [][2]int
	}{
		{"codex s1: the old docstring after the new return", "shop/pagination.py", diffS1, [][2]int{{13, 14}}},
		{"codex s2: the old body of last() after chunk()'s return", "textutil/lists.py", diffS2, [][2]int{{18, 19}}},
		{"main-3: the def line eaten", "accounts/report.py", diffMain3, [][2]int{{4, 5}}},
	}
	for _, c := range cases {
		if got := pairs(deadAfterExit(c.path, c.diff)); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDeadAfterExitLeavesOrdinaryEditsAlone(t *testing.T) {
	plain := func(lines ...string) string {
		return "--- a/f.py\n+++ b/f.py\n@@ -1,1 +1,1 @@\n" + strings.Join(lines, "\n") + "\n"
	}
	cases := []struct{ name, path, diff string }{
		{"a normal replacement", "f.py", plain("-def f():", "+def g():", "     return 1")},
		{"an if/else around returns", "f.py", plain("+    if x:", "+        return 1", "+    else:", "+        return 2")},
		{"a comment after a return", "f.py", plain("+    return 1", "+    # end of f")},
		{"the end of a multi-line return", "f.py", plain("+    return (", "+        1", "+    )")},
		{"a dedent after a return", "f.py", plain("     if x:", "+        return 1", "+    y = 2")},
		{"a deeper line after a return", "f.py", plain("+    return foo(", "+        1)")},
		{"old dead code the edit did not touch", "f.py", plain("     return 1", "     print('old')", "+    z = 3")},
		{"not python", "f.go", plain("+\treturn 1", "+\tx := 2")},
		{"except after a raise", "f.py", plain("+        raise ValueError()", "+    except ValueError:", "+        pass")},
	}
	for _, c := range cases {
		if got := deadAfterExit(c.path, c.diff); len(got) != 0 {
			t.Errorf("%s: flagged %v", c.name, pairs(got))
		}
	}
}

func TestDeadAfterExitSeesPastBlankLinesAndNumbersFromTheHunk(t *testing.T) {
	d := "--- a/f.py\n+++ b/f.py\n@@ -8,2 +20,4 @@\n     x = 1\n+    return x\n+\n+    y = 2\n"
	got := deadAfterExit("f.py", d)
	if len(got) != 1 || got[0].line != 21 || got[0].next != 23 || got[0].nextText != "y = 2" {
		t.Errorf("got %+v, want return at 21 and dead line at 23", got)
	}
}

func TestTheNoteNamesAtMostThreePairsAndClipsLongLines(t *testing.T) {
	var d []deadLine
	for i := range 5 {
		d = append(d, deadLine{line: i + 1, next: i + 2, lineText: "return " + strings.Repeat("x", 100), nextText: "y"})
	}
	note := aftercareNote(d)
	if strings.Count(note, "can never run") != maxDeadReported {
		t.Errorf("note names %d pairs, want %d:\n%s", strings.Count(note, "can never run"), maxDeadReported, note)
	}
	if strings.Contains(note, strings.Repeat("x", 80)) {
		t.Error("a long line was not clipped")
	}
	if aftercareNote(nil) != "" {
		t.Error("a note for no dead lines")
	}
}
