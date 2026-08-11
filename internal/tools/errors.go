package tools

import (
	"errors"
	"strings"
)

// Fault says which side a tool failure belongs to.
//
// Its string form is exactly journal.ToolResult.ErrorKind's vocabulary, because
// this is where that field is decided. The distinction is what keeps a harness
// bug out of the model's score: ADR-0006 §3 classifies a session that saw an
// internal tool error as a `harness` failure, and one that saw a task-level
// refusal as nothing at all — the model was told what was wrong and gets to try
// again.
type Fault uint8

const (
	// FaultNone is success.
	FaultNone Fault = iota

	// FaultTask means the tool correctly reported a problem with what was
	// asked: a missing file, a path outside the root, a binary file, a regexp
	// that does not compile. The harness worked; the request did not.
	FaultTask

	// FaultInternal means the harness itself broke. This is the expensive
	// bucket, and it is the honest one — an internal failure dressed up as a
	// task failure is a harness defect laundered into a model number.
	FaultInternal
)

var faultText = map[Fault]string{
	FaultNone:     "",
	FaultTask:     "task",
	FaultInternal: "internal",
}

// String returns the wire form, matching journal.ToolResult.ErrorKind.
func (f Fault) String() string {
	if s, ok := faultText[f]; ok {
		return s
	}
	return "internal"
}

// FaultOf classifies err for journal.ToolResult.ErrorKind.
//
// An error this package did not classify counts as [FaultInternal]. That is
// deliberate and it is the direction ADR-0006 asks us to be wrong in: an
// unclassified error means somebody forgot to say which side it came from, and
// guessing "task" would credit the harness's own oversight to the model.
func FaultOf(err error) Fault {
	if err == nil {
		return FaultNone
	}
	var e *Error
	if errors.As(err, &e) && e.Fault != FaultNone {
		return e.Fault
	}
	return FaultInternal
}

// Sentinels a caller can branch on. Compare with errors.Is; the detail is on
// *[Error], reachable with errors.As.
var (
	// ErrOutsideRoot reports a path argument that resolved outside the
	// repository root, whether by "../" or through a symlink.
	//
	// This is a *permission* question, not a verdict. The engine decides
	// policy — SLICE-1 §M1 has writes outside the root ask and reads never —
	// so the tool refuses and hands the caller something to act on rather than
	// prompting or silently allowing. Nothing here imports a permission
	// package.
	ErrOutsideRoot = errors.New("path resolves outside the repository root")

	// ErrTooLarge reports a file above Limits.MaxFileBytes.
	ErrTooLarge = errors.New("file is too large to read")

	// ErrBinaryFile reports a file that is not UTF-8 text.
	ErrBinaryFile = errors.New("file is not UTF-8 text")

	// ErrNotRegular reports a path that is not a regular file where one was
	// required — most often a directory, which is list_dir's job.
	ErrNotRegular = errors.New("path is not a regular file")
)

// Error is a tool failure, classified.
//
// It carries the path as the caller gave it *and* as it resolved, because the
// two differ exactly when the interesting thing happened: a symlink took the
// path somewhere else. A refusal the model cannot act on is a wasted turn, so
// Detail says what to do instead wherever there is something to do.
type Error struct {
	// Tool is the tool that failed, using the constants in this package.
	Tool string
	// Fault is which side the failure belongs to.
	Fault Fault
	// Path is the path argument as the caller gave it, "" when the call had
	// none or never got that far.
	Path string
	// Resolved is the real absolute path, set when resolution produced one.
	// It is what a permission prompt should name.
	Resolved string
	// Detail says what was wrong and, where possible, what to do instead.
	Detail string

	err error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Tool != "" {
		b.WriteString(e.Tool)
		b.WriteString(": ")
	}
	if e.Path != "" {
		b.WriteString(quote(e.Path))
		b.WriteString(": ")
	}
	if e.err != nil {
		b.WriteString(e.err.Error())
	}
	if e.Detail != "" {
		if e.err != nil {
			b.WriteString(": ")
		}
		b.WriteString(e.Detail)
	}
	if b.Len() == 0 {
		return "tools: unspecified failure"
	}
	return b.String()
}

// Unwrap exposes the cause: one of this package's sentinels, or the fs error
// underneath.
func (e *Error) Unwrap() error { return e.err }

func quote(s string) string { return `"` + s + `"` }

// taskErr is the common case: the request was wrong and the model is told how.
func taskErr(tool, path string, cause error, detail string) *Error {
	return &Error{Tool: tool, Fault: FaultTask, Path: path, Detail: detail, err: cause}
}

// internalErr is the expensive case: the harness broke.
func internalErr(tool, path string, cause error, detail string) *Error {
	return &Error{Tool: tool, Fault: FaultInternal, Path: path, Detail: detail, err: cause}
}
