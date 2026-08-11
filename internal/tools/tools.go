// Package tools implements the tools the model calls to inspect and change a
// repository. This slice holds the read half — read_file, list_dir and grep
// (KAN-780); write_file, run_shell and edit_file land beside them (KAN-781,
// KAN-782, KAN-784) on the same [Root] and the same [Limits].
//
// Three rules shape everything here.
//
// **Anchors come from read_file and nowhere else.** read_file renders every
// line through [anchor.Render], so the model can name a region without ever
// reproducing its content, and edit_file re-derives those anchors at apply time
// (docs/adr/0006-hash-anchored-edits-and-failure-attribution.md). grep
// deliberately does *not* emit anchors: ADR-0006 rests on anchors being
// obtainable only from a read, which is what makes an edit into a region the
// model was never shown structurally impossible rather than merely discouraged.
//
// **Bounds are declared, never silent.** CLAUDE.md forbids clipping the
// diagnostic output that justifies a fix. Where a bound is unavoidable — a file
// too large to hold in a context window, a directory with fifty thousand
// entries — the tool either refuses outright with a message naming the size, or
// returns a window and *says in the output* that it did, with the argument that
// fetches the rest. There is no path here that quietly returns less than it
// found.
//
// **Every path argument goes through [Root.Resolve].** Nothing in this package
// opens a file by a path the caller supplied.
//
// This package does not import internal/journal. Tools return results; the
// engine journals them. It also does not decide permission: a path outside the
// root is refused with [ErrOutsideRoot] for the engine's policy layer to act on,
// never prompted for here.
package tools

import "github.com/leejianrong/kopicode/internal/anchor"

// Tool names, as the model calls them and as journal.ToolResult records them.
const (
	ToolReadFile = "read_file"
	ToolListDir  = "list_dir"
	ToolGrep     = "grep"
)

// Limits are the declared bounds the tools apply. Every one of them is stated
// in the output when it binds, so a bounded result is never mistaken for a
// complete one.
//
// They are fields rather than constants so a test can shrink them to something
// a fixture can exercise cheaply. Production uses [DefaultLimits].
type Limits struct {
	// MaxFileBytes is the largest file read_file will read and grep will
	// search. Above it read_file refuses with [ErrTooLarge] rather than
	// returning part of a file: anchoring needs the whole file in memory,
	// because a line's anchor depends on its neighbours, and half a file
	// yields wrong anchors at the seam. grep skips such files and reports
	// how many it skipped.
	MaxFileBytes int64

	// MaxLines is how many lines one read_file call returns. Beyond it the
	// call returns the first MaxLines lines of the requested window and says
	// so, naming the offset that fetches the next. This is the one place a
	// window is returned rather than the whole thing, and it exists because
	// the binding constraint is the model's context, not the journal's.
	MaxLines int

	// MaxEntries is how many entries one list_dir call returns.
	MaxEntries int

	// MaxMatches is how many matching lines one grep call returns.
	MaxMatches int
}

// DefaultLimits are the production bounds.
//
// MaxFileBytes is 4 MiB — roughly a hundred times a large source file, chosen
// so that hitting it means something is wrong with the request rather than with
// the limit. MaxLines is 2000: rendered with anchors that is around 90 KB of
// prompt, already more than a turn should spend on one file, and ADR-0006 §7
// measures the anchor's own overhead at 24.9% on top of line numbers.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes: 4 << 20,
		MaxLines:     2000,
		MaxEntries:   1000,
		MaxMatches:   200,
	}
}

// Set is the tool set bound to one repository root.
//
// It holds the root handle and the limits so that every tool resolves paths the
// same way and bounds output the same way. The engine builds one per session and
// closes it; the zero value is not usable.
//
// Context is never stored here. It is the first parameter of every call.
type Set struct {
	// Root resolves and guards every path argument.
	Root *Root
	// Limits are the bounds this set applies.
	Limits Limits
}

// NewSet opens dir as a repository root and returns the tools bound to it. The
// caller closes the result.
func NewSet(dir string) (*Set, error) {
	root, err := OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Set{Root: root, Limits: DefaultLimits()}, nil
}

// Close releases the root handle.
func (s *Set) Close() error { return s.Root.Close() }

// AnchorVersion is the anchor contract read_file's output is rendered under. It
// is re-exported here so the engine can put it in the system prompt and in
// SessionStarted's harness config hash without importing internal/anchor for
// one constant — a bump is an experiment-series boundary (ADR-0006 §7).
const AnchorVersion = anchor.Version
