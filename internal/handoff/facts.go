package handoff

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"

	"github.com/leejianrong/kopicode/internal/journal"
)

// Facts is what the journal says happened, in the form a successor needs. None of
// it is the model's account: every field is derived from events.
type Facts struct {
	// FilesWritten and FilesDeleted are repository paths whose write or delete
	// call succeeded, sorted and unique. A path written and then deleted appears
	// in both, since both happened.
	FilesWritten []string
	FilesDeleted []string

	// RejectedEdits counts edits the anchored edit tool refused.
	RejectedEdits int

	// Verification is the last forced verification, or nil when none was
	// recorded. It is a fact about the tree as it was then; it does not carry
	// over (see [Document]).
	Verification *Verification

	// Denied lists calls the permission gate refused, in first-seen order and
	// without repeats, at most [MaxDenied] of them; DeniedMore counts the rest.
	Denied     []Denied
	DeniedMore int

	// Turns is the highest turn number seen, Requests the provider requests made,
	// and the token fields the sums the provider reported.
	Turns      int
	Requests   int
	Prompt     int
	Completion int
	Total      int
}

// Verification is one VerificationRun, reduced.
type Verification struct {
	Command  []string
	Source   string
	ExitCode int
	// Skip is why it did not conclude, or "".
	Skip string
}

// Ran reports that the command concluded, so that ExitCode means something.
func (v Verification) Ran() bool { return v.Skip == "" && v.Source != "none" && v.ExitCode >= 0 }

// Passed reports that it ran and exited 0.
func (v Verification) Passed() bool { return v.Ran() && v.ExitCode == 0 }

// Denied is one refused call.
type Denied struct {
	Tool   string
	Detail string
}

// MaxDenied bounds the denied list. A handoff is a summary and the journal has
// every denial in full; the count past this says how many were left out.
const MaxDenied = 10

// maxDetail bounds one denied call's detail to a line a person can read.
const maxDetail = 160

// FactsFrom derives Facts from a session's events, oldest first.
//
// It reads events and nothing else, which is the rule the journal exists to
// enforce: a fact that is not on the record is not a fact this package can state.
func FactsFrom(events []journal.Event) Facts {
	var f Facts

	type parsed struct{ tool, path string }
	calls := map[string]parsed{}
	requested := map[string]journal.PermissionRequested{}
	seenDenied := map[Denied]bool{}
	written, deleted := map[string]bool{}, map[string]bool{}

	for _, ev := range events {
		f.Turns = max(f.Turns, ev.Turn)

		switch p := ev.Payload.(type) {
		case journal.ToolCallParsed:
			calls[p.CallID] = parsed{tool: p.Tool, path: pathArg(p.Args)}

		case journal.ToolResult:
			c, ok := calls[p.CallID]
			if !ok || p.ErrorKind != "" || c.path == "" {
				break
			}
			switch c.tool {
			case "write_file":
				written[c.path] = true
			case "delete_file":
				deleted[c.path] = true
			}

		case journal.EditApplied:
			written[p.Path] = true

		case journal.EditRejected:
			f.RejectedEdits++

		case journal.VerificationRun:
			f.Verification = &Verification{
				Command: slices.Clone(p.Command), Source: p.Source, ExitCode: p.ExitCode, Skip: p.Skip,
			}

		case journal.PermissionRequested:
			requested[p.RequestID] = p

		case journal.PermissionDecided:
			if p.Decision != "deny" {
				break
			}
			req := requested[p.RequestID]
			d := Denied{Tool: req.Tool, Detail: oneLine(req.Detail.Inline)}
			if seenDenied[d] {
				break
			}
			seenDenied[d] = true
			if len(f.Denied) < MaxDenied {
				f.Denied = append(f.Denied, d)
			} else {
				f.DeniedMore++
			}

		case journal.ProviderResponse:
			f.Requests++
			f.Prompt += p.Tokens.Prompt
			f.Completion += p.Tokens.Completion
			f.Total += p.Tokens.Total
		}
	}

	f.FilesWritten = sortedKeys(written)
	f.FilesDeleted = sortedKeys(deleted)
	return f
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, cmp.Compare[string])
	return out
}

// pathArg reads the "path" argument of a call, or "".
func pathArg(args []byte) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	return a.Path
}

// oneLine reduces s to its first line, clipped for reading.
func oneLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	r := []rune(s)
	if len(r) > maxDetail {
		return string(r[:maxDetail]) + "…"
	}
	return s
}
