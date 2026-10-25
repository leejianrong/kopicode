package engine

import (
	"slices"
	"testing"
)

func diffOf(removed, added string) string {
	return "--- a/f\n+++ b/f\n@@ -1 +1 @@\n-" + removed + "\n+" + added + "\n"
}

func TestLeftoverCandidates(t *testing.T) {
	tr := newLeftoverTracker()
	tr.observe(diffOf("x = fetch_user(1)", "x = get_user(1)"))
	tr.observe(diffOf("y = fetch_user(2) # return", "y = get_user(2)"))
	tr.observe(diffOf("keep(old_helper)", "keep(old_helper)  # still here"))
	tr.observe(diffOf("keep(old_helper)", "keep(other)"))
	if got := tr.candidates(); !slices.Equal(got, []string{"fetch_user"}) {
		t.Errorf("candidates = %v, want only fetch_user: old_helper and keep were added back, return is a stop word", got)
	}
}

func TestADiffHeaderIsSkippedByPositionNotByPrefix(t *testing.T) {
	tr := newLeftoverTracker()
	// A removed line that itself starts "--" is a removed line.
	tr.observe("--- a/f\n+++ b/f\n@@ -1 +1 @@\n--note_name text\n+kept\n")
	tr.observe("--- a/f\n+++ b/f\n@@ -1 +1 @@\n--note_name more\n+kept\n")
	if got := tr.candidates(); !slices.Contains(got, "note_name") {
		t.Errorf("candidates = %v, want note_name: a removed line starting with dashes was read as a header", got)
	}
}

func TestANotedNameIsNotNotedAgain(t *testing.T) {
	tr := newLeftoverTracker()
	tr.observe(diffOf("fetch_user", "get_user"))
	tr.observe(diffOf("fetch_user", "get_user"))
	tr.noted["fetch_user"] = true
	if got := tr.candidates(); len(got) != 0 {
		t.Errorf("candidates = %v after noting, want none", got)
	}
}
