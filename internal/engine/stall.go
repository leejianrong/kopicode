package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The stall detector (KAN-1971). A weak model that is stuck does not stop; it
// repeats. Two shapes cover most of it, and both are cheap to see from what the
// loop already knows:
//
//   - the same call, with the same result, again, and nothing in the tree changed
//     in between: the model is asking a question it has already been answered;
//   - edits to one file that keep failing: the model is patching against a view
//     of the file that is no longer true.
//
// When either reaches Config.StallThreshold in a row, a short note is added after
// that call's result telling the model so and what to do instead. Nothing else
// changes: the call ran, the full result is in the journal, and the model is
// free to carry on. Like ADR-0012 decision 1's collapsed denial, only the string
// handed to the assembler differs from the journal, and it is a pure function of
// the sequence of calls and results, so a replay produces the same text.
//
// A threshold of 0 turns it off, which is every registered arm: this changes
// what the model is told, so it is a harness value in the hash (Config.
// StallThreshold), never a constant here.
type stallTracker struct {
	// last is the previous dispatch's identity, and same is how many dispatches
	// in a row, counting that one, were byte-identical in tool, arguments and
	// result.
	last stallCall
	same int

	// failedEdits counts consecutive failed edits per path. It is a slice of
	// pairs and not a map so that nothing here depends on iteration order.
	failedEdits []pathCount
}

type stallCall struct {
	tool   string
	args   []byte
	output string
}

type pathCount struct {
	path string
	n    int
}

// editTools are the tools whose failure is a failed edit, and writeTools those
// whose success definitely changed the tree. run_shell is in neither: it may
// change the tree and usually does not, and re-running an unchanged failing test
// is exactly the circle this exists to name.
var (
	editTools  = map[string]bool{"edit_file": true, "edit_file_fuzzy": true}
	writeTools = map[string]bool{"edit_file": true, "edit_file_fuzzy": true, "write_file": true, "delete_file": true}
)

// observe records one dispatched call and returns the note to add after its
// result, or "". failed says the call did not do what it was asked; denied says
// the permission gate refused it (ADR-0012 decision 1 already shortens those).
func (s *stallTracker) observe(threshold int, tool string, args []byte, output string, failed, denied bool) string {
	if threshold <= 0 {
		return ""
	}

	// A write that landed answers every pending question: the same call may now
	// give a different result, and a failing edit may now succeed.
	if writeTools[tool] && !failed {
		s.last, s.same, s.failedEdits = stallCall{}, 0, nil
		return ""
	}

	var note string

	cur := stallCall{tool: tool, args: args, output: output}
	if s.same > 0 && s.last.tool == cur.tool && bytes.Equal(s.last.args, cur.args) && s.last.output == cur.output {
		s.same++
	} else {
		s.same = 1
	}
	s.last = cur
	if s.same >= threshold && !denied {
		note = fmt.Sprintf("kopicode: you have made this exact %s call %d times and got the same result each time. "+
			"Repeating it will not change the answer. Do something different: read a different file, change the "+
			"code, or say what is blocking you.", tool, s.same)
		s.same = 0
	}

	if editTools[tool] && failed {
		path := editPath(args)
		if path != "" {
			if n := s.bumpFailedEdit(path); n >= threshold && note == "" {
				note = fmt.Sprintf("kopicode: %d edits to %s in a row have failed. The file may not look the way you think "+
					"it does. Read it again with read_file to get fresh anchors before the next edit, and if the "+
					"change still will not apply, rethink it.", n, path)
				s.resetFailedEdit(path)
			}
		}
	}
	return note
}

func (s *stallTracker) bumpFailedEdit(path string) int {
	for i := range s.failedEdits {
		if s.failedEdits[i].path == path {
			s.failedEdits[i].n++
			return s.failedEdits[i].n
		}
	}
	s.failedEdits = append(s.failedEdits, pathCount{path: path, n: 1})
	return 1
}

func (s *stallTracker) resetFailedEdit(path string) {
	for i := range s.failedEdits {
		if s.failedEdits[i].path == path {
			s.failedEdits[i].n = 0
			return
		}
	}
}

// editPath reads the path argument of an edit call, or "" when there is none.
func editPath(args []byte) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	return a.Path
}
