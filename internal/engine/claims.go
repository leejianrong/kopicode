package engine

import (
	"context"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/tools"
)

// claimTracker is ADR-0041's memory of one exchange: which files this prompt's
// calls changed, and whether a shell may have changed others. It reads the
// journal's own facts and never the task, and nothing it learns is shown to the
// model.
//
// Whether a shell wrote is read from the tree, not from the command: a turn that
// ran a shell and no write tool is taken to have written only if its snapshot's
// tree differs from the previous one. A turn with no previous snapshot to compare
// with, or with a write tool beside the shell, or with no snapshotter, is taken
// to have written, because a check that guessed the other way would call a true
// claim false.
type claimTracker struct {
	touched    map[string]bool
	shellWrote bool

	turnShell bool
	turnWrote bool
	// prevTree outlives an exchange: the tree a later exchange starts from is the
	// one the last snapshot saw.
	prevTree string

	// message and text are the last AssistantMessage's Seq and content.
	message uint64
	text    string
}

// begin starts an exchange. A nil tracker is a tracker for a session that never
// runs the check, so every method is safe on it.
func (c *claimTracker) begin() {
	if c == nil {
		return
	}
	c.touched = map[string]bool{}
	c.shellWrote = false
	c.turnShell, c.turnWrote = false, false
	c.message, c.text = 0, ""
}

// touch records a file a call changed.
func (c *claimTracker) touch(p string) {
	if c == nil || c.touched == nil || p == "" {
		return
	}
	c.touched[cleanClaimPath(p)] = true
}

// noteCall records that one call ran in the current turn. A denied shell did not
// run and is not noted.
func (c *claimTracker) noteCall(tool string) {
	if c == nil {
		return
	}
	switch tool {
	case tools.ToolRunShell:
		c.turnShell = true
	case tools.ToolWriteFile, tools.ToolDeleteFile, tools.ToolEditFile, tools.ToolEditFileFuzzy:
		c.turnWrote = true
	}
}

// noteSnapshot closes a mutating turn. tree is "" when no snapshot was taken.
func (c *claimTracker) noteSnapshot(tree string) {
	if c == nil {
		return
	}
	if c.turnShell {
		switch {
		case tree == "", c.prevTree == "", c.turnWrote:
			c.shellWrote = true
		case tree != c.prevTree:
			c.shellWrote = true
		}
	}
	if tree != "" {
		c.prevTree = tree
	}
	c.turnShell, c.turnWrote = false, false
}

// note keeps the latest assistant message, the one a stop would be judged on.
func (c *claimTracker) note(seq uint64, text string) {
	if c == nil {
		return
	}
	c.message, c.text = seq, text
}

// claimCheck compares the final message with the exchange's touched files and
// journals what it found (ADR-0041 decisions 1 and 2). It adds no turn, sends the
// model nothing and changes no stop; it is called only on a stop that is final.
func (e *Engine) claimCheck(ctx context.Context, turn int) error {
	c := e.claims
	if c == nil || c.text == "" {
		return nil
	}
	files := unbackedClaims(c.text, c.touched)
	if len(files) == 0 {
		return nil
	}
	if c.shellWrote {
		_, err := e.append(ctx, turn, journal.ClaimUnchecked{
			Message: c.message,
			Reason:  "a shell command may have written files",
			Files:   files,
		})
		return err
	}
	_, err := e.append(ctx, turn, journal.ClaimUnbacked{Message: c.message, Files: files})
	return err
}

// claimExts are the file name endings a message is read for. A short list: a
// version number or a sentence's full stop is not a file, and a miss here is a
// claim not seen, which is today's behaviour.
const claimExts = `py|go|md|txt|toml|json|yaml|yml|sh|js|ts|rs|cfg|ini|html|css`

var (
	claimFileRE = regexp.MustCompile(`(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_-][A-Za-z0-9_.-]*\.(?:` + claimExts + `)\b`)
	claimVerbRE = regexp.MustCompile(`(?i)\b(?:updated|renamed|modified|changed|edited|replaced|rewrote|rewritten|fixed|added|removed|deleted)\b`)
)

// claimHedgeRE finds a word between a file name and its verb that makes the verb a
// suggestion or a condition, not a report: "the fix belongs in tests/x.py (or be
// deleted)".
var claimHedgeRE = regexp.MustCompile(`(?i)\b(?:should|could|would|might|must|may|need|needs|please|if|be|to)\b`)

// claimWindow is how far after a file name, on the same line, a change verb still
// belongs to it.
const claimWindow = 60

// maxClaimedFiles caps one event.
const maxClaimedFiles = 10

// unbackedClaims returns the files text names as changed that touched does not
// hold. A file counts as named as changed when a change verb follows it within
// claimWindow bytes on the same line, which leaves out "I fixed the failing test
// in tests/x.py", where the file is the subject and the verb came first, and a
// verb a hedge word sits before ("should be updated").
func unbackedClaims(text string, touched map[string]bool) []journal.ClaimedFile {
	var out []journal.ClaimedFile
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		for _, m := range claimFileRE.FindAllStringIndex(line, -1) {
			if m[0] > 0 && strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_/.-", rune(line[m[0]-1])) {
				continue // the tail of a longer token, or an absolute path
			}
			name := cleanClaimPath(line[m[0]:m[1]])
			if seen[name] || claimTouched(name, touched) {
				continue
			}
			end := min(m[1]+claimWindow, len(line))
			v := claimVerbRE.FindStringIndex(line[m[1]:end])
			if v == nil || claimHedgeRE.MatchString(line[m[1]:m[1]+v[0]]) {
				continue
			}
			seen[name] = true
			out = append(out, journal.ClaimedFile{Path: name, Span: line[m[0] : m[1]+v[1]]})
			if len(out) == maxClaimedFiles {
				return out
			}
		}
	}
	return out
}

// claimTouched reports whether name is one of the touched files. A message that
// names a file by its base name, or a touched path by a longer one, still matches.
func claimTouched(name string, touched map[string]bool) bool {
	for p := range touched {
		if p == name || strings.HasSuffix(p, "/"+name) || strings.HasSuffix(name, "/"+p) {
			return true
		}
	}
	return false
}

func cleanClaimPath(p string) string {
	return strings.TrimPrefix(path.Clean(filepath.ToSlash(p)), "./")
}

// claimText is a message's claim, as a person reads it: the files named as
// changed, as written.
func claimText(files []journal.ClaimedFile) string {
	spans := make([]string, 0, len(files))
	for _, f := range files {
		spans = append(spans, "`"+f.Span+"`")
	}
	return "final message says " + strings.Join(spans, ", ")
}

func claimedPaths(files []journal.ClaimedFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}
