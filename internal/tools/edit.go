package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// ModeAnchored is the edit mode this tool implements: the model names a region
// by the anchors read_file showed it, and every one of them is re-derived from
// disk before anything is written.
//
// It is a named value rather than a bare string because journal.EditApplied and
// journal.EditRejected both carry a Mode, and ADR-0006 §3 turns that field into
// a bench classification — a session containing one fuzzy edit is
// `unattributed`. [ModeFuzzy] is the second value, landed by KAN-785 as a
// separate tool, and nothing in this file has a path into it —
// TestAnchoredModeCannotReachTheFuzzyFallback proves that over the source
// rather than asserting it in a comment.
const ModeAnchored = "anchored"

// RejectReason says why an edit was refused, in a form a classifier can read.
//
// The vocabulary is deliberately finer than "the edit did not apply", because
// each value implies a *different* recovery and SLICE-1 §9 charges different
// buckets to different causes. A tool that collapsed these into one error
// string would make the distinction unrecoverable after the fact, and would
// also send the model to re-read a file that had not changed — the retry loop
// SLICE-1 risk 2 is about.
type RejectReason string

const (
	// RejectMalformedAnchor is an argument that is not an anchor at all: the
	// wrong length, or not lowercase hexadecimal. It cannot have come from a
	// read_file, so no re-read will help — the model is emitting a line number,
	// a paraphrase or a git sha where an anchor belongs, and what it needs to
	// be told is the shape. Kept apart from drift because the *rate* of it is
	// the direct measurement of SLICE-1 risk 2: whether the model can copy an
	// anchor back at all.
	RejectMalformedAnchor RejectReason = "anchor_malformed"

	// RejectAnchorDrift is a well-formed anchor that matches no line in the
	// file. It covers both a file that changed since the read and an anchor the
	// model invented; [anchor.ErrUnknownAnchor] states plainly that nothing can
	// tell those apart, and it does not matter here because the recovery is the
	// same one — read the file again and anchor against what is actually there.
	// The rejection carries those current anchors so that costs no turn.
	RejectAnchorDrift RejectReason = "anchor_drift"

	// RejectAmbiguousAnchor is an anchor carried by more than one line, which
	// happens when a file repeats a whole three-line window verbatim (ADR-0006
	// §7 measures this at about 2% of lines in the median file). The file is
	// fine and the anchor is real, so re-reading changes nothing: the model
	// must pick a different, unique anchor, and the rejection names the
	// candidate lines so it can.
	RejectAmbiguousAnchor RejectReason = "ambiguous"

	// RejectAnchorOrder is two anchors that both resolve, with the end above
	// the start. Nothing has drifted and nothing is ambiguous; the arguments
	// are swapped. Guessing the model meant the region between them in the
	// other order would be exactly the fail-open reasoning ADR-0006 rejects, so
	// it is refused — but as its own reason, because "read the file again" is
	// the wrong advice and would burn a turn.
	RejectAnchorOrder RejectReason = "anchor_order"
)

// ErrEditRejected is the sentinel under every anchored-mode refusal. A caller
// branches on it with errors.Is; the structured detail is on
// [EditResult].Rejection, which is what the engine journals as EditRejected.
var ErrEditRejected = errors.New("edit rejected")

// rejectContext is how many lines either side of a known position the current
// anchors are returned for. Five is enough for the model to see the region it
// was aiming at and re-anchor without another read.
const rejectContext = 5

// diffContext is how many unchanged lines the returned unified diff carries
// either side of the change, matching diff(1) and git's default.
const diffContext = 3

// EditRequest is one edit_file call.
//
// There is no line number here and there never will be. ADR-0006 §7 rejects a
// line number even as a tiebreaker between equal anchors: on a shifted file a
// repeated window still matches at the wrong number, so the pair would validate
// and apply to the wrong region. Line numbers are rendered for humans.
type EditRequest struct {
	// Path is the file to edit, relative to the repository root or absolute.
	// It must exist: edit_file changes part of a file, write_file makes one.
	Path string

	// AnchorStart and AnchorEnd name the first and last line of the region to
	// replace, inclusive. They may be equal, which replaces one line. Both come
	// from a read_file of this same file.
	AnchorStart string
	AnchorEnd   string

	// NewText is what the region becomes. It is read the way [anchor.Split]
	// reads a file, so a trailing newline is a terminator rather than an extra
	// blank line, and its line terminators are normalised to the file's own —
	// a model that emits "\n" cannot silently convert a CRLF file.
	//
	// **Empty deletes the region outright**, terminator included, so a deletion
	// leaves no blank line behind. Replacing a region *with* a blank line is
	// therefore "\n" and not "": splitting the way a file is split makes the
	// two different by construction, and the alternative — a separate delete
	// flag — is one more argument for a weak model to get wrong on every call.
	NewText string
}

// AnchorLine is one line of the file as read_file shows it.
//
// It carries the machine fields and the rendered form together so the engine
// can journal or re-render without parsing anything: the rendered form is
// [anchor.Render]'s output verbatim, and that format is documented as something
// nothing machine-parses.
type AnchorLine struct {
	// Line is the 1-based line number.
	Line int
	// Anchor is the line's current anchor.
	Anchor string
	// Rendered is the line exactly as read_file would print it.
	Rendered string
}

// EditRejection is a refused edit, in the shape the engine journals as
// journal.EditRejected without having to invent anything: Reason is that
// payload's Reason, Detail is its Detail, and the request's two anchors are its
// AnchorStart and AnchorEnd.
//
// Nothing here imports internal/journal. Tools return data; the engine
// journals.
type EditRejection struct {
	// Reason is the machine-readable kind.
	Reason RejectReason
	// Field names which argument failed: "anchor_start" or "anchor_end", or ""
	// when the failure is about the pair rather than one of them.
	Field string
	// Anchor is the argument that failed, as the model wrote it.
	Anchor string
	// Candidates are the 1-based first line numbers of every region the
	// refusal found more than one of: the lines a [RejectAmbiguousAnchor]
	// anchor matched in anchored mode, and the first line of each region above
	// the floor in fuzzy mode. Ascending. Nil for every other reason.
	Candidates []int

	// Matches are the regions the fuzzy fallback found, and it is nil in
	// anchored mode. On [RejectBelowFloor] it is the near misses, closest
	// first and at most [NearMissCount] of them; on [RejectAmbiguousAnchor] it
	// is **every** region at or above the floor, because the model has to see
	// all of them to pick one — returning two of five would make the file look
	// less repetitive than it is.
	Matches []FuzzyMatch

	// Current is the file's anchors as they are right now, for the region the
	// model was aiming at. It is the whole point of failing closed: the model
	// retries against reality rather than against what it remembers.
	//
	// When no referenced anchor resolved, the region is unknown, so this is the
	// file from its first line, bounded exactly as a read_file is bounded and
	// with the bound declared in Detail.
	Current []AnchorLine

	// Summary is the one-line form, which is also the tool error's detail.
	Summary string
	// Detail is the full model-facing text, Current included. It is what the
	// engine puts in journal.EditRejected.Detail, and it is also
	// [EditResult].Output.
	Detail string
}

// EditResult is what one edit_file call produced.
//
// Exactly one of Rejection and Diff is meaningful: a rejected edit has a
// Rejection and an empty Diff, an applied edit has a Diff and a nil Rejection.
// A cancelled call has neither and did not touch the file.
type EditResult struct {
	// Path is the file, as a slash path relative to the root. On a
	// cancellation it is the argument as given, because nothing was resolved.
	Path string
	// Mode is the edit mode used: [ModeAnchored] from [Set.EditFile],
	// [ModeFuzzy] from [Set.EditFileFuzzy]. It is never empty on a result this
	// package returned with a file argument that resolved, and it is copied
	// straight into journal.EditApplied.Mode and journal.EditRejected.Mode —
	// SLICE-1 §9 classifies a session containing one fuzzy edit as
	// `unattributed`, so an unset mode under-counts that bucket and flatters
	// the harness.
	Mode string
	// AnchorStart and AnchorEnd are the anchors the model referenced, echoed
	// so the engine journals what was asked for rather than what was found.
	AnchorStart string
	AnchorEnd   string
	// AnchorVersion is the anchor contract the anchors were derived under.
	// It is journaled on EditApplied because a bump is an experiment-series
	// boundary (ADR-0006 §7).
	AnchorVersion string

	// StartLine and EndLine are the 1-based inclusive region that was replaced,
	// 0 when nothing was.
	StartLine int
	EndLine   int
	// LinesRemoved and LinesAdded count the region and its replacement.
	LinesRemoved int
	LinesAdded   int
	// BytesBefore and BytesAfter are the file's size either side of the edit.
	BytesBefore int
	BytesAfter  int

	// Diff is the unified diff actually applied, empty when nothing was.
	// SLICE-1 §4 requires it and journal.EditApplied carries it.
	Diff string

	// Rejection is set, and the file byte-identical, when the edit was refused.
	Rejection *EditRejection

	// Fuzzy is set on **every** result [Set.EditFileFuzzy] returns — applied,
	// refused or cancelled — and nil on every result [Set.EditFile] returns.
	//
	// It is a second, independent signal beside Mode and it is deliberate.
	// SLICE-1 §9 marks a whole session `unattributed` if the fuzzy fallback ran
	// at any point, so a caller that misses the signal under-counts the bucket
	// in the flattering direction; a nil-pointer check and a string comparison
	// fail differently, and TestFuzzyModeIsUnmistakable holds both.
	Fuzzy *FuzzyInfo

	// Cancelled reports that the context ended before anything was written.
	// The call also returns a [FaultCancelled] error saying the same thing —
	// this field is for the caller, that one is for the classifier.
	Cancelled bool

	// Output is the model-facing rendering.
	Output string
}

// EditFile replaces the region between two anchors, or refuses.
//
// **The refusal is the product.** Every referenced anchor is re-derived from
// the bytes on disk at apply time and must resolve to exactly one line; any
// mismatch leaves the file byte-identical and returns the region's current
// anchors, so a drifted file cannot be silently corrupted
// (ADR-0006, CLAUDE.md "Edits fail closed"). There is no similarity floor here,
// no nearest match, and no path by which a failed anchor match degrades into an
// approximate one — the fuzzy fallback is KAN-785 and lands as a second mode
// beside this one, never as a fallthrough from it.
//
// **Anchors are obtainable only from a read, and that is structural.** An
// anchor is eight hex characters of SHA-256 over the anchor version and a
// length-prefixed (previous, this, next) window, so producing one requires the
// content it names. read_file is the only tool that emits them — grep and
// list_dir deliberately do not, which tools.go states and
// TestNoToolButReadFileEmitsAnchors holds — so an anchor the model can spend is
// an anchor it was shown. Editing a region it never saw is impossible rather
// than discouraged.
//
// The decisions this tool makes, stated rather than implied:
//
//   - **The region is inclusive of both anchors**, and equal anchors replace one
//     line. Empty NewText deletes the region, including its line terminators, so
//     a deletion does not leave a blank line behind.
//   - **Everything outside the region is preserved byte for byte.** The file is
//     spliced, not rebuilt from its lines: rebuilding would rewrite a CRLF
//     checkout as LF, which is a whole-file change nobody asked for and nothing
//     would show. New lines inside the region take the file's own terminator.
//   - **A file with no trailing newline keeps not having one**, and one with a
//     trailing newline keeps it, because that byte lives outside every region.
//   - **The mode is unchanged**, because the write goes through os.WriteFile
//     semantics on an existing file, so an executable script stays executable.
//   - **A missing file is a task error, not a creation.** write_file creates.
//
// A cancellation is a result *and* a [FaultCancelled] error, the shape KAN-808
// settled across every tool in this package: [FaultInternal] would book each
// Ctrl-C as a harness failure under ADR-0006 §3, and a nil error would let
// SLICE-1 §9's "everything else" arm charge it to the model.
func (s *Set) EditFile(ctx context.Context, req EditRequest) (EditResult, error) {
	if err := ctx.Err(); err != nil {
		return cancelledEdit(req), cancelledErr(ToolEditFile, req.Path, err, "the file was not changed")
	}

	p, err := s.Root.Resolve(ToolEditFile, req.Path)
	if err != nil {
		return EditResult{}, err
	}

	src, err := s.editSource(ToolEditFile, req.Path, p)
	if err != nil {
		return EditResult{}, err
	}

	lines := anchor.Split(src)
	spans := lineSpans(src)
	if err := checkSpans(ToolEditFile, req.Path, src, lines, spans); err != nil {
		return EditResult{}, err
	}
	anchors := anchor.Derive(lines)

	start, end, rej, err := s.locate(req, lines, anchors)
	if err != nil {
		return EditResult{}, err
	}
	if rej != nil {
		res := rejectedEdit(req, p, rej)
		return res, taskErr(ToolEditFile, req.Path, ErrEditRejected, rej.Summary)
	}

	// The last check before the file changes. After the write there is nothing
	// to cancel, and reporting a completed edit as cancelled would leave the
	// model believing a change it made is not there.
	if err := ctx.Err(); err != nil {
		return cancelledEdit(req), cancelledErr(ToolEditFile, req.Path, err, "the file was not changed")
	}

	repl := anchor.Split([]byte(req.NewText))
	next := apply(src, spans, start, end, repl)
	if err := s.Root.dir.WriteFile(p.Rel, next, defaultFileMode); err != nil {
		return EditResult{}, s.editErr(ToolEditFile, req.Path, err)
	}

	res := EditResult{
		Path:          p.Slash(),
		Mode:          ModeAnchored,
		AnchorStart:   req.AnchorStart,
		AnchorEnd:     req.AnchorEnd,
		AnchorVersion: AnchorVersion,
		StartLine:     start + 1,
		EndLine:       end + 1,
		LinesRemoved:  end - start + 1,
		LinesAdded:    len(repl),
		BytesBefore:   len(src),
		BytesAfter:    len(next),
		Diff:          unifiedDiff(p.Slash(), lines, start, end, repl),
	}
	res.Output = res.render()
	return res, nil
}

// cancelledEdit is the result of a call the context ended before it wrote.
func cancelledEdit(req EditRequest) EditResult {
	r := EditResult{
		Path:        req.Path,
		Mode:        ModeAnchored,
		AnchorStart: req.AnchorStart,
		AnchorEnd:   req.AnchorEnd,
		Cancelled:   true,
	}
	r.Output = r.render()
	return r
}

// rejectedEdit is the result of an edit that was refused. The file is
// untouched, and that is asserted by the tests rather than merely intended.
func rejectedEdit(req EditRequest, p Path, rej *EditRejection) EditResult {
	return EditResult{
		Path:          p.Slash(),
		Mode:          ModeAnchored,
		AnchorStart:   req.AnchorStart,
		AnchorEnd:     req.AnchorEnd,
		AnchorVersion: AnchorVersion,
		Rejection:     rej,
		Output:        rej.Detail,
	}
}

// editSource reads the file an edit targets, refusing the same shapes
// read_file refuses and for the same reasons: anchoring needs the whole file in
// memory, and bytes that are not UTF-8 are mangled by the JSON encoding on the
// way to the model, so its anchors could never match the file.
//
// tool and given are passed rather than taken from an [EditRequest] because
// both edit modes read a file the same way and only the name in the message
// differs. The fuzzy fallback matches whole normalised lines, so it needs the
// file whole for the same reason anchoring does.
func (s *Set) editSource(tool, given string, p Path) ([]byte, error) {
	info, err := s.stat(tool, p)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, taskErr(tool, given, ErrNotRegular,
			"it is a directory; "+tool+" edits files")
	}
	if !info.Mode().IsRegular() {
		return nil, taskErr(tool, given, ErrNotRegular,
			fmt.Sprintf("its mode is %s", info.Mode()))
	}
	if info.Size() > s.Limits.MaxFileBytes {
		return nil, taskErr(tool, given, ErrTooLarge,
			fmt.Sprintf("it is %d bytes and the limit is %d; a line's anchor depends on its neighbours, "+
				"so a file that cannot be read whole cannot be anchored", info.Size(), s.Limits.MaxFileBytes))
	}

	src, err := s.open(tool, p)
	if err != nil {
		return nil, err
	}
	if looksBinary(src) {
		return nil, taskErr(tool, given, ErrBinaryFile,
			fmt.Sprintf("it is %d bytes of non-UTF-8 data, which cannot be anchored", len(src)))
	}
	return src, nil
}

// editErr classifies a failure writing the edited file back.
//
// It is separate from [Set.writeErr] because the messages name the tool, and
// separate from [Set.handleErr] because the file demonstrably existed and was
// readable moments ago: a failure now means the tree moved under us. The
// default arm is [FaultInternal] for the reason writeErr's is — Resolve already
// proved this path inside the root, so an unexplained refusal is the harness's
// problem, and calling it a task failure would hide a harness limitation inside
// a model score.
func (s *Set) editErr(tool, given string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return taskErr(tool, given, err,
			"the file disappeared between being read and being written")
	case errors.Is(err, fs.ErrPermission):
		return taskErr(tool, given, err, "the path is not writable")
	default:
		return internalErr(tool, given, err,
			"the root handle refused a path that resolved inside the root")
	}
}

// anchorFault is one anchor argument's failure, before it is dressed up as an
// [EditRejection].
type anchorFault struct {
	reason RejectReason
	// lines are the 1-based candidates of an ambiguous anchor.
	lines []int
}

// locate resolves both anchors against the file's current anchors and returns
// the 0-based region, or the rejection that refuses it.
//
// The order of the checks is the order of the recoveries. A malformed argument
// is reported before a drifted one because "that is not an anchor" and "that
// anchor is stale" send the model to different places, and anchor_start is
// reported before anchor_end because a model that got the first one wrong will
// usually have got both wrong the same way.
//
// The error return is not the rejection path: it is for an unclassified failure
// out of [anchor.Resolve], which is a harness defect and is reported as one
// rather than being folded into a model-facing refusal.
func (s *Set) locate(req EditRequest, lines, anchors []string) (start, end int, rej *EditRejection, err error) {
	startIdx, startFault, err := resolveAnchor(req, req.AnchorStart, anchors)
	if err != nil {
		return 0, 0, nil, err
	}
	endIdx, endFault, err := resolveAnchor(req, req.AnchorEnd, anchors)
	if err != nil {
		return 0, 0, nil, err
	}

	switch {
	case startFault != nil:
		return 0, 0, s.reject(req, lines, anchors, "anchor_start", req.AnchorStart, startFault,
			known(startFault, endFault, startIdx, endIdx)), nil
	case endFault != nil:
		return 0, 0, s.reject(req, lines, anchors, "anchor_end", req.AnchorEnd, endFault,
			known(startFault, endFault, startIdx, endIdx)), nil
	case endIdx < startIdx:
		fault := &anchorFault{reason: RejectAnchorOrder}
		return 0, 0, s.reject(req, lines, anchors, "", "", fault, []int{endIdx, startIdx}), nil
	}
	return startIdx, endIdx, nil, nil
}

// known is the set of line indices the rejection is sure about — the anchors
// that did resolve. It is what the returned current-anchor window is centred
// on, so a model that got one of the two right is shown the region it meant
// rather than the whole file.
func known(startFault, endFault *anchorFault, startIdx, endIdx int) []int {
	var out []int
	if startFault == nil {
		out = append(out, startIdx)
	}
	if endFault == nil {
		out = append(out, endIdx)
	}
	return out
}

// resolveAnchor turns one anchor argument into a line index or a fault.
func resolveAnchor(req EditRequest, a string, anchors []string) (int, *anchorFault, error) {
	if !WellFormedAnchor(a) {
		return 0, &anchorFault{reason: RejectMalformedAnchor}, nil
	}
	i, err := anchor.Resolve(anchors, a)
	var amb *anchor.AmbiguousError
	switch {
	case err == nil:
		return i, nil, nil
	case errors.As(err, &amb):
		return 0, &anchorFault{reason: RejectAmbiguousAnchor, lines: amb.Lines}, nil
	case errors.Is(err, anchor.ErrUnknownAnchor):
		return 0, &anchorFault{reason: RejectAnchorDrift}, nil
	default:
		// Fail closed rather than guessing a reason. An unclassified error out
		// of the anchor package means this switch has fallen behind it, which
		// is a harness defect, and ADR-0006 §3 says to count it as one.
		return 0, nil, internalErr(ToolEditFile, req.Path, err,
			"anchor.Resolve returned an error this tool does not classify")
	}
}

// WellFormedAnchor reports whether a has the shape [anchor.Derive] produces:
// exactly [anchor.Length] lowercase hexadecimal characters.
//
// It is a shape check and not a membership check, and the difference is the
// point. An argument of the wrong shape cannot be an anchor at all — no line in
// any file carries it — so refusing it as [RejectMalformedAnchor] rather than
// as drift tells the model something true and actionable, and keeps the two
// measurable apart.
//
// The width comes from [anchor.Length] rather than a literal, so a version bump
// that changes it does not leave this check quietly wrong.
func WellFormedAnchor(a string) bool {
	if len(a) != anchor.Length {
		return false
	}
	for i := range len(a) {
		c := a[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// reject builds the refusal the model reads and the engine journals.
func (s *Set) reject(req EditRequest, lines, anchors []string, field, arg string,
	fault *anchorFault, resolved []int,
) *EditRejection {
	rej := &EditRejection{
		Reason:     fault.reason,
		Field:      field,
		Anchor:     arg,
		Candidates: fault.lines,
	}

	// Which lines to show. An anchor that resolved pins the region; an
	// ambiguous one is shown at each of its candidates; and when nothing at all
	// resolved the region is unknown, so the file itself is the answer.
	positions := slices.Clone(resolved)
	for _, n := range fault.lines {
		positions = append(positions, n-1)
	}
	whole := len(positions) == 0 && fault.reason == RejectAnchorDrift

	var notice string
	rej.Current, notice = s.anchorWindow(lines, anchors, positions, whole)
	rej.Summary = rejectSummary(field, arg, fault, len(lines))
	rej.Detail = rejectDetail(req.Path, rej, notice, len(lines))
	return rej
}

// rejectSummary is the one-line form, which is also the tool error's detail.
func rejectSummary(field, arg string, fault *anchorFault, total int) string {
	switch fault.reason {
	case RejectMalformedAnchor:
		return fmt.Sprintf("%s %q is not an anchor: an anchor is %d lowercase hex characters, "+
			"copied from the start of a read_file line", field, arg, anchor.Length)
	case RejectAnchorDrift:
		if total == 0 {
			return fmt.Sprintf("%s %q matches no line: the file is empty", field, arg)
		}
		return fmt.Sprintf("%s %q matches no line in the file as it is now", field, arg)
	case RejectAmbiguousAnchor:
		return fmt.Sprintf("%s %q matches %d lines (%s), so it does not name a region",
			field, arg, len(fault.lines), joinInts(fault.lines))
	case RejectAnchorOrder:
		return "anchor_end is above anchor_start, so the two do not bound a region"
	default:
		return "the edit could not be placed exactly"
	}
}

// rejectDetail is the full model-facing text: what was refused, why, what to do
// about it, and the current anchors to do it with.
func rejectDetail(path string, rej *EditRejection, notice string, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: rejected, the file was not changed\n", ToolEditFile, path)
	fmt.Fprintf(&b, "reason: %s — %s\n", rej.Reason, rej.Summary)
	b.WriteString(rejectAdvice(rej.Reason, total))
	b.WriteByte('\n')

	if notice != "" {
		b.WriteString(notice)
		b.WriteByte('\n')
	}
	if len(rej.Current) == 0 {
		return b.String()
	}

	b.WriteString("current anchors:\n")
	prev := 0
	for _, l := range rej.Current {
		if prev != 0 && l.Line != prev+1 {
			b.WriteString("...\n")
		}
		b.WriteString(l.Rendered)
		b.WriteByte('\n')
		prev = l.Line
	}
	return b.String()
}

// rejectAdvice is the sentence that turns a refusal into a next move. A
// rejection the model cannot act on is a wasted turn, and each reason has a
// different act.
func rejectAdvice(reason RejectReason, total int) string {
	switch reason {
	case RejectMalformedAnchor:
		return "copy the anchor from the start of the read_file line you mean, verbatim; " +
			"line numbers are not anchors and edit_file ignores them"
	case RejectAnchorDrift:
		if total == 0 {
			return "the file has no lines, so it has no anchors; use write_file to give it content"
		}
		return "the file has changed since it was read, or the anchor did not come from a read_file " +
			"of this file; anchor against the lines below"
	case RejectAmbiguousAnchor:
		return "the file repeats this line and its neighbours, so the anchor names more than one " +
			"place; anchor on a nearby line that appears once"
	case RejectAnchorOrder:
		return "both anchors resolve and the file is unchanged; pass the earlier line as anchor_start"
	default:
		return "re-read the file and anchor against it"
	}
}

// anchorWindow returns the current anchors the model needs, ascending.
//
// positions are 0-based line indices to show, each widened by [rejectContext]
// either side and merged. whole asks for the file from its first line instead,
// which is what "the region's current anchors" means when no anchor resolved
// and the region is therefore unknown.
//
// The result is bounded by Limits.MaxLines exactly as a read_file is bounded,
// and the bound is *declared* in the returned notice. CLAUDE.md forbids
// clipping diagnostic output silently; it does not forbid the same declared
// window read_file already returns, and an unbounded dump of a four-megabyte
// file into a refusal would be its own harm.
func (s *Set) anchorWindow(lines, anchors []string, positions []int, whole bool) ([]AnchorLine, string) {
	if len(lines) == 0 {
		return nil, ""
	}

	show := make([]bool, len(lines))
	switch {
	case whole:
		for i := range show {
			show[i] = true
		}
	default:
		for _, p := range positions {
			lo := max(0, p-rejectContext)
			hi := min(len(lines)-1, p+rejectContext)
			for i := lo; i <= hi; i++ {
				show[i] = true
			}
		}
	}

	rendered := anchor.Render(lines)
	out := make([]AnchorLine, 0, len(lines))
	bounded := 0
	for i, on := range show {
		if !on {
			continue
		}
		if s.Limits.MaxLines > 0 && len(out) >= s.Limits.MaxLines {
			bounded++
			continue
		}
		out = append(out, AnchorLine{Line: i + 1, Anchor: anchors[i], Rendered: rendered[i]})
	}
	if len(out) == 0 {
		return nil, ""
	}

	notice := ""
	if bounded > 0 {
		notice = fmt.Sprintf("showing %s of %s that changed hands here; "+
			"bounded at max_lines=%d, read_file with offset=%d for the rest",
			plural(len(out), "line", "lines"), plural(len(out)+bounded, "line", "lines"),
			s.Limits.MaxLines, out[len(out)-1].Line+1)
	}
	return out, notice
}

// span is one line's content in the source bytes, terminator excluded.
//
// Offsets exist because the file is spliced rather than rebuilt. Rebuilding a
// file from [anchor.Split]'s lines would drop every "\r" in a CRLF checkout and
// rewrite the whole file as LF, which is a change nobody asked for, which no
// diff of the edited region would show, and which would stale every anchor in
// the file at once.
type span struct{ start, end int }

// lineSpans locates each line's content in src.
//
// It mirrors [anchor.Split] exactly — split on "\n", drop one trailing "\r",
// treat a single trailing newline as a terminator — and [checkSpans] proves it
// still does on every call rather than trusting this comment.
func lineSpans(src []byte) []span {
	if len(src) == 0 {
		return nil
	}
	s := src
	if s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}

	out := make([]span, 0, bytes.Count(s, []byte{'\n'})+1)
	start := 0
	for i := 0; i <= len(s); i++ {
		if i < len(s) && s[i] != '\n' {
			continue
		}
		end := i
		if end > start && s[end-1] == '\r' {
			end--
		}
		out = append(out, span{start: start, end: end})
		start = i + 1
	}
	return out
}

// checkSpans refuses to edit a file whose byte offsets and whose anchored lines
// disagree.
//
// The two are derived by different code — [anchor.Split] owns what a line *is*,
// [lineSpans] owns where it sits — and an off-by-one between them at a file
// boundary is precisely how an anchored editor corrupts a file while every
// happy-path test stays green. It is a whole-file string comparison on a file
// already bounded by MaxFileBytes, so it costs nothing worth counting against
// applying an edit to the wrong bytes.
//
// A disagreement is [FaultInternal] and not a rejection: the model did nothing
// wrong, the harness did, and ADR-0006 §3 wants that counted as `harness`.
func checkSpans(tool, given string, src []byte, lines []string, spans []span) error {
	if len(spans) != len(lines) {
		return internalErr(tool, given, nil, fmt.Sprintf(
			"line offsets disagree with the anchored lines: %d spans for %d lines",
			len(spans), len(lines)))
	}
	for i, sp := range spans {
		if string(src[sp.start:sp.end]) != lines[i] {
			return internalErr(tool, given, nil, fmt.Sprintf(
				"line offsets disagree with the anchored lines at line %d", i+1))
		}
	}
	return nil
}

// apply splices repl into src in place of lines start..end inclusive, and
// returns the new file.
//
// Everything outside the region is copied byte for byte, terminators included,
// so an edit to one line of a CRLF file leaves the rest of it CRLF and an edit
// to a file with no trailing newline does not grow one. New lines take the
// file's own terminator.
//
// A deletion takes a line terminator with it — the one after the region, or the
// one before it when the region runs to the end of the file — because otherwise
// removing a line would leave a blank one in its place. Deleting every line
// leaves an empty file rather than a lone newline.
//
// start and end index spans, so this is only ever reached for a file with
// lines: an empty file carries no anchors, so nothing resolves in it and
// [Set.locate] has already refused. That invariant is held by the rejection and
// not by a check here — a check that silently returned the file unchanged would
// report an edit that did not happen, which is the one outcome worse than a
// panic.
func apply(src []byte, spans []span, start, end int, repl []string) []byte {
	sEnd := len(src)
	if sEnd > 0 && src[sEnd-1] == '\n' {
		sEnd--
	}
	s, tail := src[:sEnd], src[sEnd:]

	from, to := spans[start].start, spans[end].end
	if len(repl) == 0 {
		switch {
		case end+1 < len(spans):
			to = spans[end+1].start
		case start > 0:
			from = spans[start-1].end
		}
	}

	inserted := strings.Join(repl, lineEnding(src))
	out := make([]byte, 0, len(s)-(to-from)+len(inserted)+len(tail))
	out = append(out, s[:from]...)
	out = append(out, inserted...)
	out = append(out, s[to:]...)
	if len(repl) == 0 && len(out) == 0 {
		// Every line was deleted. The trailing newline went with the last of
		// them; keeping it would leave a file holding one blank line where the
		// caller asked for nothing.
		//
		// Both halves of the condition are load-bearing. Emptiness alone would
		// also catch a one-blank-line file whose one line was replaced by
		// another blank line — no lines were deleted there, and dropping the
		// terminator would silently empty a file the caller only rewrote.
		return out
	}
	return append(out, tail...)
}

// lineEnding is the terminator new lines take: the file's first one, or "\n"
// for a file that has none to copy.
func lineEnding(src []byte) string {
	if i := bytes.IndexByte(src, '\n'); i > 0 && src[i-1] == '\r' {
		return "\r\n"
	}
	return "\n"
}

// unifiedDiff renders the change as a unified diff over one hunk.
//
// It is built rather than shelled out to `git diff --no-index`, which is what
// the rest of this repository uses for diff rendering, and the reason is
// determinism. SLICE-1's acceptance criteria include a byte-identical journal
// when a session is replayed, and this diff is journaled on EditApplied — a
// rendering that varies with whichever git is installed would break that
// criterion on the developer's machine before it ever reached CI. There is also
// nothing to compute: an anchored edit replaces one contiguous region, so the
// hunk is known exactly and no diff algorithm is involved.
//
// Line content is rendered without terminators, as unified diff always is. A
// file's trailing-newline state is preserved exactly by [apply] and is
// deliberately not represented here: the "\ No newline at end of file" marker
// cannot be placed symmetrically for every edit at the end of such a file, and
// an asymmetric one would assert something false about the side it was omitted
// from.
func unifiedDiff(path string, old []string, start, end int, repl []string) string {
	lo := max(0, start-diffContext)
	hi := min(len(old)-1, end+diffContext)

	oldCount := hi - lo + 1
	newCount := oldCount - (end - start + 1) + len(repl)

	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", path, path)
	fmt.Fprintf(&b, "@@ -%s +%s @@\n", hunkRange(lo+1, oldCount), hunkRange(lo+1, newCount))
	for k := lo; k < start; k++ {
		fmt.Fprintf(&b, " %s\n", old[k])
	}
	for k := start; k <= end; k++ {
		fmt.Fprintf(&b, "-%s\n", old[k])
	}
	for _, l := range repl {
		fmt.Fprintf(&b, "+%s\n", l)
	}
	for k := end + 1; k <= hi; k++ {
		fmt.Fprintf(&b, " %s\n", old[k])
	}
	return b.String()
}

// hunkRange formats one side of a hunk header the way diff(1) does: the count
// is omitted when it is 1, and an empty side is numbered from the line before
// it.
func hunkRange(start, count int) string {
	switch count {
	case 0:
		return strconv.Itoa(start-1) + ",0"
	case 1:
		return strconv.Itoa(start)
	default:
		return strconv.Itoa(start) + "," + strconv.Itoa(count)
	}
}

// joinInts renders line numbers for a message.
func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// render is the model-facing form of an applied or cancelled edit. A rejected
// one is rendered by [rejectDetail] or [fuzzyDetail], because a refusal has to
// carry the current anchors or the near misses, and an application has to carry
// the diff.
//
// The mode is named in the line the model reads, not only in the struct the
// engine journals: a fuzzy edit that looked like an anchored one in the
// transcript would be the same under-counting SLICE-1 §9 is about, arriving
// through the surface instead of through the record.
func (r EditResult) render() string {
	tool := editToolName(r.Mode)
	if r.Cancelled {
		return fmt.Sprintf("%s %s: cancelled before writing; the file was not changed\n", tool, r.Path)
	}

	var b strings.Builder
	region := fmt.Sprintf("lines %d-%d", r.StartLine, r.EndLine)
	if r.StartLine == r.EndLine {
		region = fmt.Sprintf("line %d", r.StartLine)
	}
	mode := r.Mode
	if r.Fuzzy != nil {
		mode = fmt.Sprintf("%s, similarity %s against a floor of %s",
			r.Mode, formatScore(r.Fuzzy.Similarity), formatScore(r.Fuzzy.Floor))
	}
	fmt.Fprintf(&b, "%s %s: applied at %s, %s replaced by %s (%s)\n",
		tool, r.Path, region, plural(r.LinesRemoved, "line", "lines"),
		plural(r.LinesAdded, "line", "lines"), mode)
	b.WriteString(r.Diff)
	return b.String()
}

// editToolName is the tool a result came from, derived from its mode so that
// one [EditResult] can render for either without carrying a second field that
// could disagree with the first.
func editToolName(mode string) string {
	if mode == ModeFuzzy {
		return ToolEditFileFuzzy
	}
	return ToolEditFile
}
