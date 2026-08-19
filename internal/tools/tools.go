// Package tools implements the tools the model calls to inspect and change a
// repository: read_file, list_dir and grep (KAN-780), run_shell (KAN-782),
// write_file (KAN-781), edit_file (KAN-784) and its fuzzy fallback
// edit_file_fuzzy (KAN-785), all on the same [Root] and the same [Limits].
//
// Three rules shape everything here.
//
// **Anchors come from read_file and nowhere else.** read_file renders every
// line through [anchor.Render], so the model can name a region without ever
// reproducing its content, and [Set.EditFile] re-derives those anchors at apply
// time (docs/adr/0006-hash-anchored-edits-and-failure-attribution.md). grep
// deliberately does *not* emit anchors: ADR-0006 rests on anchors being
// obtainable only from a read, which is what makes an edit into a region the
// model was never shown structurally impossible rather than merely discouraged.
// TestNoToolButReadFileEmitsAnchors holds that across the whole tool set, so a
// tool added later cannot leak one by accident. edit_file_fuzzy is the sharpest
// case of it: its near-miss report quotes file content the model may never have
// read, and it renders line numbers without anchors for exactly that reason.
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
// opens a file by a path the caller supplied. run_shell is the stated exception
// and the reason the permission gate exists: its working directory is resolved
// like any other path, but the command it runs is a shell and a shell goes
// where it likes. Containment is not something run_shell can offer, so it is
// not claimed — SLICE-1 §M1 has the engine ask before every one.
//
// This package does not import internal/journal. Tools return results; the
// engine journals them. It also does not decide permission: a path outside the
// root is refused with [ErrOutsideRoot] for the engine's policy layer to act on,
// never prompted for here.
package tools

import (
	"time"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// Tool names, as the model calls them and as journal.ToolResult records them.
const (
	ToolReadFile  = "read_file"
	ToolListDir   = "list_dir"
	ToolGrep      = "grep"
	ToolRunShell  = "run_shell"
	ToolWriteFile = "write_file"
	ToolEditFile  = "edit_file"

	// ToolEditFileFuzzy is the fallback of ADR-0006 §2, for when the model
	// cannot produce a usable anchor. It is a separate tool and not a second
	// argument shape on edit_file, because one tool taking either
	// (anchor_start, anchor_end) or (before, after) would have to decide which
	// mode a call meant from which fields arrived — and a call that filled
	// both would have to be adjudicated. That is a place to guess, inside the
	// one path in this package that is allowed to be approximate at all.
	ToolEditFileFuzzy = "edit_file_fuzzy"

	// ToolAsk names the ask tool (docs/adr/0009-ask-tool-contract.md). It is
	// declared here rather than only in internal/engine, the same way every
	// other tool name above is, because TestToolSetMatchesInternalTools and
	// engine.ToolNames() both derive their expectation from this package's
	// Tool* constants by source inspection rather than from a hand-written
	// list — even though ask, unlike every other name here, has no method on
	// [Set]: nothing about it touches a repository root, a file or a
	// subprocess. internal/engine dispatches it directly against the
	// engine's own configured Answerer, never through this package, which is
	// exactly ADR-0009 decision 2's point.
	ToolAsk = "ask"
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

	// ShellTimeout is how long one run_shell call may take before its process
	// group is killed. ShellRequest.Timeout overrides it per call.
	//
	// Note what is *not* here: a bound on captured output. run_shell returns
	// both streams whole, because the output of a failing build is the reason
	// the call was made.
	ShellTimeout time.Duration

	// ShellGrace is how long a signalled process group has to exit before it is
	// killed outright, and how long a finished command's output pipes are given
	// to drain when something it started is still holding them.
	ShellGrace time.Duration

	// FuzzyFloor is the normalised similarity a region must reach before
	// edit_file_fuzzy will consider it a match at all. See [DefaultFuzzyFloor]
	// for the default and the basis for it.
	//
	// It is a **tunable harness parameter**, not a constant, because SLICE-1
	// measures what the harness buys and a number that cannot be varied cannot
	// be measured. A value outside (0, 1] — which includes the zero value of a
	// hand-built Limits — falls back to the default rather than meaning "match
	// anything": a forgotten field must not be the thing that turns the one
	// approximate edit path in this package into an unconditional one.
	FuzzyFloor float64
}

// DefaultLimits are the production bounds.
//
// MaxFileBytes is 4 MiB — roughly a hundred times a large source file, chosen
// so that hitting it means something is wrong with the request rather than with
// the limit. MaxLines is 2000: rendered with anchors that is around 90 KB of
// prompt, already more than a turn should spend on one file, and ADR-0006 §7
// measures the anchor's own overhead at 24.9% on top of line numbers.
//
// ShellTimeout is 120s, from SLICE-1 §Build Plan step 5. It is long enough for
// a Go test suite on a cold cache and short enough that a turn spent on a hung
// command is a minute of a bench run rather than the whole of it.
func DefaultLimits() Limits {
	return Limits{
		MaxFileBytes: 4 << 20,
		MaxLines:     2000,
		MaxEntries:   1000,
		MaxMatches:   200,
		ShellTimeout: 120 * time.Second,
		ShellGrace:   5 * time.Second,
		FuzzyFloor:   DefaultFuzzyFloor,
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
	// Clock is where run_shell's timeout comes from. Nil means the real one.
	Clock Clock

	// Home overrides HOME (and USERPROFILE) in the environment run_shell's
	// child sees. Empty means inherit the ambient one — the REPL path, where a
	// real user's shell must see their real HOME.
	//
	// internal/bench sets this per task to the task's temp HOME, so
	// model-authored shell and the oracle that judges the tree afterwards
	// agree on where dotfiles, toolchain caches and git config live (KAN-874).
	// Without it a task could pass only because its shell reached the
	// operator's real ~/.gitconfig or ~/.cache while the oracle ran against a
	// clean one — a result that would not reproduce on another machine, which
	// is a measurement defect and not (yet) a security one; isolation is
	// slice 3 per ADR-0005.
	//
	// This is not a sandbox: it changes one variable in the child's
	// environment, nothing else, and a command can still reach outside it by
	// naming an absolute path. See childEnv in shell.go.
	Home string
}

// NewSet opens dir as a repository root and returns the tools bound to it. The
// caller closes the result.
func NewSet(dir string) (*Set, error) {
	root, err := OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &Set{Root: root, Limits: DefaultLimits(), Clock: RealClock{}}, nil
}

// Clock is where a timeout comes from.
//
// It is injected because the alternative — a test that waits out the real
// timeout — is either a slow suite or a short timeout that proves nothing about
// the one shipping. With a clock the test can fire the timer and then assert
// what the harness did about it, which is the behaviour under test.
type Clock interface {
	// Now is the current time. Only differences are ever used.
	Now() time.Time
	// NewTimer returns a channel that receives once d has elapsed, and a
	// function that releases the timer. The caller always calls it.
	NewTimer(d time.Duration) (<-chan time.Time, func())
}

// RealClock is the production clock.
type RealClock struct{}

// Now returns the wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// NewTimer wraps time.NewTimer.
func (RealClock) NewTimer(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// clock is the clock this set uses. A Set built by hand rather than by NewSet
// still runs, because a nil clock meaning "no timeout at all" would turn a
// forgotten field into a hung command.
func (s *Set) clock() Clock {
	if s.Clock == nil {
		return RealClock{}
	}
	return s.Clock
}

// Close releases the root handle.
func (s *Set) Close() error { return s.Root.Close() }

// AnchorVersion is the anchor contract read_file's output is rendered under. It
// is re-exported here so the engine can put it in the system prompt and in
// SessionStarted's harness config hash without importing internal/anchor for
// one constant — a bump is an experiment-series boundary (ADR-0006 §7).
const AnchorVersion = anchor.Version
