package engine

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
)

// ADR-0039: after a passing verification, tell the model once if a name its own
// edits removed is still in the tree. The names come from the journal's edits,
// not from the task text; the note never blocks a stop.

const (
	// leftoverMinEdits is how many applied edits must remove a name from a line
	// before it counts as removed on purpose and not as one edit's incidental text.
	leftoverMinEdits = 2
	// leftoverMaxNames and leftoverMaxLocations bound the note.
	leftoverMaxNames     = 3
	leftoverMaxLocations = 5
)

// identRE is a name: a letter or underscore and at least three more word characters.
var identRE = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{3,}`)

// leftoverStop is words that are removed and added in ordinary code and say
// nothing about a rename. A name is also dropped when any applied edit added it,
// which covers most of them; this covers the few that an edit only ever removes
// (deleting a block of `return` lines).
var leftoverStop = map[string]bool{
	"return": true, "self": true, "None": true, "True": true, "False": true, "import": true,
	"from": true, "with": true, "else": true, "elif": true, "print": true, "pass": true,
	"class": true, "func": true, "type": true, "const": true, "this": true, "null": true,
	"nil": true, "true": true, "false": true, "var": true, "let": true, "def": true,
	"if": true, "for": true, "while": true, "try": true, "except": true, "raise": true,
	"lambda": true, "yield": true, "async": true, "await": true, "export": true, "default": true,
	"switch": true, "case": true, "break": true, "continue": true, "package": true, "struct": true,
	"string": true, "int": true, "error": true, "bool": true, "float": true,
}

// leftoverTracker accumulates, over one prompt's applied edits, the names they
// removed and the names they added.
type leftoverTracker struct {
	removed map[string]int // name -> applied edits that removed it from a line
	added   map[string]bool
	noted   map[string]bool
}

func newLeftoverTracker() *leftoverTracker {
	return &leftoverTracker{removed: map[string]int{}, added: map[string]bool{}, noted: map[string]bool{}}
}

// observe folds one applied edit's unified diff in. The two header lines are
// skipped by position, not by prefix, so a removed line that itself starts "--"
// is still a removed line.
func (t *leftoverTracker) observe(diff string) {
	lines := strings.Split(diff, "\n")
	if len(lines) > 2 && strings.HasPrefix(lines[0], "--- ") && strings.HasPrefix(lines[1], "+++ ") {
		lines = lines[2:]
	}

	gone := map[string]bool{}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "-"):
			for _, n := range identRE.FindAllString(l[1:], -1) {
				gone[n] = true
			}
		case strings.HasPrefix(l, "+"):
			for _, n := range identRE.FindAllString(l[1:], -1) {
				t.added[n] = true
			}
		}
	}
	for n := range gone {
		t.removed[n]++
	}
}

// candidates are the names to look for: removed by enough edits, never added by
// any, not a stop word, and not noted already, most-removed first.
func (t *leftoverTracker) candidates() []string {
	var out []string
	for n, c := range t.removed {
		if c >= leftoverMinEdits && !t.added[n] && !leftoverStop[n] && !t.noted[n] {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if t.removed[out[i]] != t.removed[out[j]] {
			return t.removed[out[i]] > t.removed[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// hitRE reads one grep result line, `path:line: text`.
var hitRE = regexp.MustCompile(`^(\S.*?):(\d+): `)

// leftoverNote looks for the removed names in the tree after a passing
// verification and returns the text to add to the observation ("" for none),
// journaling what it found. The search is the grep tool's, with its walk and its
// skips.
func (e *Engine) leftoverNote(ctx context.Context, turn int) (string, error) {
	t := e.leftover
	if t == nil || !e.cfg.Selection.Config.LeftoverNote {
		return "", nil
	}
	var found []journal.LeftoverName
	var skipped string
	for _, name := range t.candidates() {
		if len(found) == leftoverMaxNames {
			break
		}
		out, err := e.cfg.Tools.Grep(ctx, tools.GrepRequest{Pattern: `\b` + regexp.QuoteMeta(name) + `\b`})
		if err != nil {
			skipped = fmt.Sprintf("searching for %s: %v", name, err)
			break
		}
		var locs []string
		for _, l := range strings.Split(out, "\n") {
			if m := hitRE.FindStringSubmatch(l); m != nil && len(locs) < leftoverMaxLocations {
				locs = append(locs, m[1]+":"+m[2])
			}
		}
		if len(locs) == 0 {
			continue
		}
		t.noted[name] = true
		found = append(found, journal.LeftoverName{Name: name, Edits: t.removed[name], Locations: locs})
	}
	if len(found) == 0 && skipped == "" {
		return "", nil
	}
	if _, err := e.append(ctx, turn, journal.LeftoverNoted{Names: found, Skipped: skipped}); err != nil {
		return "", err
	}
	return leftoverText(found), nil
}

// leftoverText is what the model is shown. Empty when nothing was found (a
// skipped search says nothing to the model; the journal says it was skipped).
func leftoverText(found []journal.LeftoverName) string {
	if len(found) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range found {
		fmt.Fprintf(&b, "kopicode: `%s` was removed from lines by %d of your edits and still appears at %s. "+
			"If this change should be complete, update it; if it should stay, say why in your closing summary.\n",
			n.Name, n.Edits, strings.Join(n.Locations, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}
