package tools

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// ReadRequest is one read_file call.
type ReadRequest struct {
	// Path is the file to read, relative to the repository root or absolute.
	Path string
	// Offset is the 1-based first line to return. 0 means the start of the
	// file. It is how the model pages through a file that did not fit.
	Offset int
	// Limit is how many lines to return. 0 means "as many as
	// Limits.MaxLines allows"; a larger value is reduced to it and the
	// reduction is stated in the output.
	Limit int
}

// binaryProbeBytes is how much of a file is scanned for a NUL before the
// UTF-8 check. A NUL in the first few kilobytes is the cheap, decisive signal;
// the UTF-8 check that follows is what actually decides.
const binaryProbeBytes = 8 << 10

// ReadFile returns the file with an anchor on every line.
//
// The output is [anchor.Render]'s, verbatim, under a header naming the file and
// its size:
//
//	internal/tools/read.go: 7 lines, 112 bytes
//	a3f21b09  1| package main
//	5c0e77d2  2|
//	9b2d4a10  3| import "fmt"
//
// Rendering is anchor's and not this package's on purpose. The derivation and
// the rendering are one model-facing contract, and a second formatter here is
// how the two halves drift and a recorded fixture quietly stops matching
// (ADR-0006 §7).
//
// **Anchors are derived over the whole file, never over the window.** A line's
// anchor depends on its neighbours, so anchoring a window would give its first
// and last lines anchors that no other read reproduces — the model would then
// send edit_file an anchor that fails to resolve, and the rejection would look
// like drift in a file nothing had touched.
//
// The four cases the card asks to be decided, and the answers:
//
//   - **Too large.** Above Limits.MaxFileBytes the call is refused with
//     [ErrTooLarge], naming the size and the bound, and pointing at grep. It is
//     not partially read: anchoring needs the whole file, and a partial read
//     produces anchors that are wrong rather than merely incomplete.
//   - **Binary.** Refused with [ErrBinaryFile]. This is a fail-closed decision,
//     not squeamishness: bytes that are not valid UTF-8 are replaced with
//     U+FFFD when the result is JSON-encoded on its way to the provider, so the
//     model would read content that differs from the file and derive anchors
//     that cannot match it. Returning nothing is better than returning
//     something subtly false.
//   - **Empty.** Not an error. The header says the file is empty, so that an
//     empty result cannot be mistaken for a failed read.
//   - **Line range.** Offset and Limit, both optional. Whenever the returned
//     window is not the whole file the output says which lines it holds, out of
//     how many, and — when a bound rather than the request caused it — the
//     offset that fetches the rest. Nothing is ever dropped silently.
func (s *Set) ReadFile(ctx context.Context, req ReadRequest) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", cancelledErr(ToolReadFile, req.Path, err, "nothing was read")
	}

	p, err := s.Root.Resolve(ToolReadFile, req.Path)
	if err != nil {
		return "", err
	}
	if req.Offset < 0 || req.Limit < 0 {
		return "", taskErr(ToolReadFile, req.Path, nil,
			"offset and limit must not be negative")
	}

	info, err := s.stat(ToolReadFile, p)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", taskErr(ToolReadFile, req.Path, ErrNotRegular,
			"it is a directory; use list_dir")
	}
	if !info.Mode().IsRegular() {
		return "", taskErr(ToolReadFile, req.Path, ErrNotRegular,
			fmt.Sprintf("its mode is %s", info.Mode()))
	}
	if info.Size() > s.Limits.MaxFileBytes {
		return "", taskErr(ToolReadFile, req.Path, ErrTooLarge,
			fmt.Sprintf("it is %d bytes and the limit is %d; use grep to locate the region you need",
				info.Size(), s.Limits.MaxFileBytes))
	}

	src, err := s.open(ToolReadFile, p)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", cancelledErr(ToolReadFile, req.Path, err, "nothing was read")
	}
	if looksBinary(src) {
		return "", taskErr(ToolReadFile, req.Path, ErrBinaryFile,
			fmt.Sprintf("it is %d bytes of non-UTF-8 data, which cannot be anchored", len(src)))
	}

	lines := anchor.Split(src)
	if len(lines) == 0 {
		return fmt.Sprintf("%s: empty file\n", p.Slash()), nil
	}

	start, end, notice, err := s.window(req, len(lines))
	if err != nil {
		return "", err
	}

	rendered := anchor.Render(lines)

	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s, %d bytes\n", p.Slash(), plural(len(lines), "line", "lines"), len(src))
	if notice != "" {
		b.WriteString(notice)
		b.WriteByte('\n')
	}
	for _, line := range rendered[start:end] {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// window turns the request into a half-open [start, end) over total lines, plus
// the sentence that declares it when it is not the whole file.
//
// The notice is not decoration. A window returned without one is a truncation,
// and CLAUDE.md's rule is that the tool says when it applied a bound.
func (s *Set) window(req ReadRequest, total int) (start, end int, notice string, err error) {
	start = 0
	if req.Offset > 0 {
		start = req.Offset - 1
	}
	if start >= total {
		return 0, 0, "", taskErr(ToolReadFile, req.Path, nil,
			fmt.Sprintf("offset %d is past the end of a %d line file", req.Offset, total))
	}

	limit := req.Limit
	capped := false
	if limit <= 0 || limit > s.Limits.MaxLines {
		if limit > s.Limits.MaxLines {
			capped = true
		}
		limit = s.Limits.MaxLines
	}

	end = min(start+limit, total)
	if start == 0 && end == total {
		return start, end, "", nil
	}

	var n strings.Builder
	fmt.Fprintf(&n, "showing lines %d-%d of %d", start+1, end, total)
	switch {
	case end < total && (capped || req.Limit == 0):
		fmt.Fprintf(&n, "; bounded at max_lines=%d, read again with offset=%d for the rest",
			s.Limits.MaxLines, end+1)
	case end < total:
		fmt.Fprintf(&n, "; read again with offset=%d for the rest", end+1)
	}
	return start, end, n.String(), nil
}

// looksBinary reports whether src is something read_file must refuse.
//
// A NUL in the first few kilobytes settles it outright. Otherwise the whole
// buffer must be valid UTF-8 — the whole buffer and not a prefix, because the
// reason for the check is that invalid bytes are mangled by JSON encoding on
// the way to the model, and one such byte anywhere in the file is enough to
// make the content the model reads differ from the content on disk. Files are
// already bounded by Limits.MaxFileBytes, so scanning all of it is cheap.
//
// The cost is that a legitimately non-UTF-8 text file — Latin-1 source, say —
// is refused. That is the correct direction to be wrong in here: the
// alternative is anchors the model cannot use, presented as if it could.
func looksBinary(src []byte) bool {
	probe := src
	if len(probe) > binaryProbeBytes {
		probe = probe[:binaryProbeBytes]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return true
	}
	return !utf8.Valid(src)
}
