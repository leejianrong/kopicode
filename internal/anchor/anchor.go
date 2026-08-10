// Package anchor derives the per-line content anchors that hash-anchored edits
// are built on (docs/adr/0006-hash-anchored-edits-and-failure-attribution.md).
//
// An anchor names a line without the model ever reproducing its content:
// read_file renders one per line, edit_file references them, and every
// referenced anchor is re-derived from disk at apply time so a drifted file is
// rejected rather than corrupted.
//
// The anchor format is a model-facing contract. It appears in every prompt, in
// every recorded provider fixture, and in the journal, so it is versioned
// ([Version]) and the version is mixed into the hash preimage — a bump changes
// every anchor, which makes a stale fixture fail loudly instead of subtly. What
// a bump obliges is written into ADR-0006 §7.
//
// Rendering lives here rather than in the read_file tool on purpose. The
// derivation and the rendering are one contract; splitting them across two
// packages is how the two halves drift apart.
package anchor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// Version identifies the anchor contract. It is mixed into the hash
	// preimage, journaled on SessionStarted as part of the harness config
	// hash, and stated in the system prompt. Changing it invalidates every
	// recorded provider fixture and opens a new experiment series
	// (ADR-0005, ADR-0006 §7).
	Version = "kopicode-anchor-v1"

	// Length is the anchor width in lowercase hex characters. 8 characters is
	// 32 bits: about one spurious collision per two thousand 2000-line files,
	// which is five orders of magnitude below the rate at which real source
	// repeats a whole three-line window. Widening it would buy nothing and
	// cost tokens on every line of every read. See ADR-0006 §7.
	Length = 8

	// Radius is how many lines either side of a line enter its preimage. A
	// pure content hash gives every `}` and every blank line the same anchor;
	// one line of context each side takes the median file from a third of its
	// lines ambiguous to about one fiftieth, and keeps the staleness blast
	// radius of an applied edit at plus or minus one line.
	Radius = 1

	// sep separates the anchor prefix from the line content in rendered
	// output. Content may contain it; nothing machine-parses a rendered line,
	// so that is a legibility question and not a parsing one.
	sep = "|"

	// missing stands in for a neighbour that falls outside the file, so that
	// line 1 of a file beginning with a blank line does not hash the same as
	// a blank line with a blank neighbour.
	missing = -1
)

// ErrUnknownAnchor is returned when no line in the file carries an anchor.
// It covers both a drifted file and an anchor the model invented rather than
// read; nothing here can tell those apart, and both are rejections.
var ErrUnknownAnchor = errors.New("anchor matches no line in the file")

// AmbiguousError is returned when more than one line carries the same anchor,
// which happens when a file repeats a whole three-line window verbatim. Per
// ADR-0006 the edit is refused, never guessed at; Lines carries the candidates
// so the caller can report them and the model can pick an unambiguous anchor
// nearby.
type AmbiguousError struct {
	Anchor string
	Lines  []int // 1-based line numbers, ascending
}

func (e *AmbiguousError) Error() string {
	parts := make([]string, len(e.Lines))
	for i, n := range e.Lines {
		parts[i] = strconv.Itoa(n)
	}
	return fmt.Sprintf("anchor %s is ambiguous: it matches lines %s",
		e.Anchor, strings.Join(parts, ", "))
}

// Split cuts file bytes into the lines anchors are derived from.
//
// Line terminators are not part of a line: the split is on "\n" and one
// trailing "\r" is dropped, so the same file checked out with CRLF and with LF
// yields identical anchors. Without that, a fixture recorded on one platform
// would reject every edit on another. A single trailing newline is a
// terminator rather than an empty final line, so a file with and without one
// give the same anchors. An empty file has no lines.
//
// Preserving the original terminators when writing a file back is the edit
// tool's job, not this package's.
func Split(src []byte) []string {
	if len(src) == 0 {
		return nil
	}
	s := strings.TrimSuffix(string(src), "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// Derive returns one anchor per line, in order.
//
// The anchor is a function of the line and its immediate neighbours and of
// nothing else — not of the line number, and not of the file. That is what
// makes it stable: an edit anywhere outside a line's own window leaves its
// anchor byte-identical, so anchors the model read earlier stay valid.
func Derive(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, len(lines))
	buf := make([]byte, 0, 256)
	var enc [Length]byte
	for i := range lines {
		buf = preimage(buf[:0], lines, i)
		sum := sha256.Sum256(buf)
		// Length is even, so this encodes whole bytes of the digest.
		hex.Encode(enc[:], sum[:Length/2])
		out[i] = string(enc[:])
	}
	return out
}

// preimage appends the hash input for the window centred on lines[i].
//
// Each field is length-prefixed rather than delimiter-separated, so the
// encoding is injective for any bytes at all — including the NUL and newline
// bytes a binary file will contain. A delimiter scheme lets two different
// windows produce one preimage, and that is a collision no hash can undo.
func preimage(b []byte, lines []string, i int) []byte {
	b = strconv.AppendInt(b, int64(len(Version)), 10)
	b = append(b, ':')
	b = append(b, Version...)
	for d := -Radius; d <= Radius; d++ {
		j := i + d
		if j < 0 || j >= len(lines) {
			b = strconv.AppendInt(b, missing, 10)
			b = append(b, ':')
			continue
		}
		b = strconv.AppendInt(b, int64(len(lines[j])), 10)
		b = append(b, ':')
		b = append(b, lines[j]...)
	}
	return b
}

// Resolve finds the single line carrying a, returning its 0-based index.
//
// It fails closed, which is the whole point of the scheme: no match is
// [ErrUnknownAnchor] and more than one match is an [AmbiguousError]. There is
// no path that picks one of several candidates. A line number is never a
// tiebreaker — a shifted file still matches on a repeated window, so accepting
// one would reintroduce exactly the silent misapplication ADR-0006 exists to
// prevent.
func Resolve(anchors []string, a string) (int, error) {
	var hits []int
	for i, got := range anchors {
		if got == a {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 0:
		return 0, fmt.Errorf("%q: %w", a, ErrUnknownAnchor)
	case 1:
		return hits[0], nil
	default:
		nums := make([]int, len(hits))
		for i, h := range hits {
			nums[i] = h + 1
		}
		return 0, &AmbiguousError{Anchor: a, Lines: nums}
	}
}

// Render formats lines as read_file presents them, one rendered line per source
// line:
//
//	a3f21b09   1| package main
//	5c0e77d2   2|
//	9b2d4a10   3| import "fmt"
//
// The anchor comes first, at a fixed width in column zero, because it is the
// only field the model has to copy back and it should never move. The line
// number's width varies with the file, so anything placed after it would.
//
// Line numbers are for humans, for grep output and for stack traces. They are
// *not* part of the edit contract: edit_file takes anchors and would ignore a
// line number, because on a shifted file a repeated window still matches at the
// wrong number and using it as a selector would fail open.
//
// A blank line renders with no trailing whitespace. Rendered output round-trips
// through recorded provider fixtures, and an invisible trailing byte is the
// thing most likely to be silently mangled on the way.
func Render(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	anchors := Derive(lines)
	width := len(strconv.Itoa(len(lines)))
	out := make([]string, len(lines))
	for i, l := range lines {
		prefix := fmt.Sprintf("%s %*d%s", anchors[i], width, i+1, sep)
		if l == "" {
			out[i] = prefix
			continue
		}
		out[i] = prefix + " " + l
	}
	return out
}
