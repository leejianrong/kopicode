package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/leejianrong/kopicode/internal/anchor"
)

// ModeFuzzy is the fallback edit mode: the model gives the text it believes is
// in the file rather than an anchor, and the tool finds it by normalised
// similarity.
//
// **This value is the `unattributed` trigger.** SLICE-1 §9 classifies any
// failing session that used the fuzzy fallback at any point as `unattributed`
// rather than `model`, because a fuzzy match above the floor and in the wrong
// place applies cleanly, emits no error, and would otherwise be laundered into
// a model-capability number (ADR-0006 §3). The mode is therefore recorded on
// every result this file produces — applied, refused and cancelled alike — and
// [EditResult].Fuzzy carries it a second time as a pointer, so a caller has to
// miss two independent signals to under-count the bucket.
const ModeFuzzy = "fuzzy"

// RejectBelowFloor is a fuzzy edit whose closest region did not reach the
// similarity floor. It is journal.EditRejected's fifth reason, the one that
// payload's doc reserves for this mode.
//
// It is not drift and it is not ambiguity: the text the model gave is not in
// the file in any recognisable form, so the recovery is neither "re-read" nor
// "pick a different anchor" but "here is what is actually closest, at these
// line numbers". The rejection carries the near misses so that costs no turn.
const RejectBelowFloor RejectReason = "below_floor"

// DefaultFuzzyFloor is the similarity a region must reach to be a candidate.
//
// **The basis, stated rather than assumed.** With whitespace already
// normalised away, a model quoting a region it has seen should reproduce it
// close to verbatim, so the floor is not separating "similar" from
// "dissimilar" — it is deciding how much paraphrase the fallback absorbs. At
// 0.90 a region may differ from the model's text by about one character in
// ten: a renamed identifier or a changed literal inside a short block, which is
// the imprecision the fallback exists for. Below roughly 0.85 a three-line
// block can differ by a whole short line and still match, which is the
// misapplication mode ADR-0006 was written about. Above roughly 0.95 the
// fallback stops absorbing the paraphrase it exists to absorb and degenerates
// into an exact-match tool with extra steps.
//
// **The floor is not what makes this safe, and it is important not to believe
// it is.** A block that differs by one identifier scores well above 0.90, so
// when the file holds both the intended region and a near-duplicate, *both*
// clear the floor and the ambiguity refusal is what stops the guess. What the
// floor stops is a single weak match being applied for want of a better one.
// The residual — exactly one region above the floor, and it is the wrong one —
// is not closed by any value of this number; it is what the `unattributed`
// bucket exists to absorb.
//
// It is a default and not a constant: [Limits].FuzzyFloor overrides it, because
// a number nobody can vary is a number nobody can measure.
const DefaultFuzzyFloor = 0.90

// NearMissCount is how many near misses a [RejectBelowFloor] returns. SLICE-1
// §4 fixes it at three: it is a model-facing contract, and the model uses it to
// retry against reality rather than against what it remembers.
const NearMissCount = 3

// ErrNoMatchText reports a fuzzy edit with nothing to find.
//
// Empty text would match the zero-length window at every position in the file,
// which is the one input that makes an approximate matcher unconditional. It is
// refused as an argument error rather than as a rejection because it is not a
// question of where the edit landed: there is nothing to land.
var ErrNoMatchText = errors.New("fuzzy edit has no text to find")

// FuzzyEditRequest is one edit_file_fuzzy call.
//
// There is no anchor here and there is no line number. Anchors belong to
// [EditRequest]; a line number would be a tiebreaker between equal matches, and
// ADR-0006 §7 rejects tiebreaking between equal matches at all — on a shifted
// file a repeated region still matches at the wrong number, so the pair would
// validate and apply to the wrong place.
type FuzzyEditRequest struct {
	// Path is the file to edit, relative to the repository root or absolute.
	Path string

	// Before is the text the model believes is in the file. It is matched
	// against every same-height region of the file under [normalizeLines], and
	// the region it names is replaced. It must not be empty.
	//
	// It is *not* required to be exact, which is the whole point of this mode;
	// it is required to be closer than [Limits].FuzzyFloor to exactly one
	// region, which is the whole point of the refusals.
	Before string

	// After is what the matched region becomes. It is read the way
	// [anchor.Split] reads a file and spliced the way [EditRequest].NewText is,
	// so it carries the same contract: empty deletes the region outright,
	// terminator included, and a region replaced *by* a blank line is "\n".
	After string
}

// FuzzyMatch is one region of the file the fallback compared against.
//
// It is returned for matches above the floor and for near misses below it, and
// it carries the line numbers because that is what makes the report usable: the
// model reads a near miss, sees where the text it meant actually is, and comes
// back with an anchored edit.
//
// Lines are the file's own lines, **not** rendered with anchors. That is not an
// oversight. Anchors are obtainable only from read_file, which is what makes an
// edit into a region the model was never shown structurally impossible
// (ADR-0006 decision 1) — and a fuzzy near miss quotes content the model may
// never have read, so handing out its anchors would sell that guarantee for a
// diagnostic convenience. TestNoToolButReadFileEmitsAnchors covers this tool.
type FuzzyMatch struct {
	// StartLine and EndLine are the 1-based inclusive region.
	StartLine int
	EndLine   int
	// Similarity is the normalised score in [0, 1].
	Similarity float64
	// Lines is the region as it is on disk, one string per line, terminators
	// excluded.
	Lines []string
}

// FuzzyInfo is the fallback's own record of a call, present on every result
// [Set.EditFileFuzzy] returns and nil on every result [Set.EditFile] returns.
type FuzzyInfo struct {
	// Floor is the similarity floor this call ran under, which is
	// [Limits].FuzzyFloor or [DefaultFuzzyFloor]. It is recorded per call
	// rather than assumed from the default, because an A/B arm that varies it
	// and does not record it is not evidence.
	Floor float64
	// Similarity is the score of the region that was replaced, and 0 when none
	// was.
	Similarity float64
	// Scanned is how many regions were compared. Zero means the file has no
	// region of that height at all, which is a different finding from "nothing
	// was close".
	Scanned int
}

// EditFileFuzzy replaces the region that matches req.Before, or refuses.
//
// **It refuses far more often than it applies, and that is the design.** This
// is the one edit path in kopicode allowed to be approximate, so it is the one
// path that could apply cleanly to the wrong region and report success —
// exactly the defect ADR-0006 exists to prevent, and the reason SLICE-1 §9
// marks any session that reaches here `unattributed`. Three rules hold it:
//
//   - **Below the floor, refuse**, and return the [NearMissCount] closest
//     regions with their line numbers so the model can retry against reality.
//   - **More than one region above the floor, refuse as ambiguous**, and return
//     every one of them. There is no tiebreak. Not the higher score, not the
//     first, not the one nearest anything — a rule that picks is a rule that
//     picks wrong silently, which is the failure this whole card is about.
//   - **Exactly one region above the floor applies**, and the result says so in
//     three places the caller cannot all miss: Mode, Fuzzy and the rendered
//     output.
//
// A region is a run of file lines exactly as tall as req.Before whose score
// under [regionScore] reaches the floor. What that score does and does not
// treat as the same text is [normalizeLines] and [regionScore]; both are stated
// there in full, because a matcher whose notion of "the same" is implicit is a
// matcher nobody can review.
//
// **When this is reachable.** Only by calling it. It is a distinct tool with a
// distinct argument shape, and [Set.EditFile] contains no call into this file —
// TestAnchoredModeCannotReachTheFuzzyFallback parses the source and proves it
// rather than trusting this sentence. That matters because the fallback is for
// a model that could not produce an anchor *at all*, never a rescue for an
// anchor that drifted: a drifted anchor means the file moved under the model,
// and answering that with an approximate match on remembered text is precisely
// how a stale edit lands somewhere plausible. Anchor drift is a hard rejection
// and stays one.
//
// Everything about how bytes reach disk is [Set.EditFile]'s: the same file
// checks, the same splice that preserves CRLF and a missing trailing newline,
// the same in-Go unified diff. There is deliberately no second renderer — the
// diff is journaled and SLICE-1 requires a byte-identical journal on replay.
func (s *Set) EditFileFuzzy(ctx context.Context, req FuzzyEditRequest) (EditResult, error) {
	floor := s.fuzzyFloor()
	if err := ctx.Err(); err != nil {
		return cancelledFuzzy(req, floor), cancelledErr(ToolEditFileFuzzy, req.Path, err,
			"the file was not changed")
	}

	needle := anchor.Split([]byte(req.Before))
	if len(needle) == 0 {
		return EditResult{}, taskErr(ToolEditFileFuzzy, req.Path, ErrNoMatchText,
			"pass the text you want replaced; empty text matches everywhere, which is "+
				"the one input that would make this tool apply an edit anywhere")
	}

	p, err := s.Root.Resolve(ToolEditFileFuzzy, req.Path)
	if err != nil {
		return EditResult{}, err
	}
	src, err := s.editSource(ToolEditFileFuzzy, req.Path, p)
	if err != nil {
		return EditResult{}, err
	}

	lines := anchor.Split(src)
	spans := lineSpans(src)
	if err := checkSpans(ToolEditFileFuzzy, req.Path, src, lines, spans); err != nil {
		return EditResult{}, err
	}

	above, near, scanned := rankRegions(lines, needle, floor)
	info := &FuzzyInfo{Floor: floor, Scanned: scanned}

	if len(above) != 1 {
		rej := fuzzyReject(req, floor, above, near, len(lines), len(needle))
		res := rejectedFuzzy(req, p, info, rej)
		return res, taskErr(ToolEditFileFuzzy, req.Path, ErrEditRejected, rej.Summary)
	}

	// The last check before the file changes, matching edit_file: after the
	// write there is nothing to cancel, and reporting a completed edit as
	// cancelled would leave the model believing a change it made is not there.
	if err := ctx.Err(); err != nil {
		return cancelledFuzzy(req, floor), cancelledErr(ToolEditFileFuzzy, req.Path, err,
			"the file was not changed")
	}

	m := above[0]
	start, end := m.StartLine-1, m.EndLine-1
	repl := anchor.Split([]byte(req.After))
	next := apply(src, spans, start, end, repl)
	if err := s.Root.dir.WriteFile(p.Rel, next, defaultFileMode); err != nil {
		return EditResult{}, s.editErr(ToolEditFileFuzzy, req.Path, err)
	}

	info.Similarity = m.Similarity
	res := EditResult{
		Path: p.Slash(),
		Mode: ModeFuzzy,
		// AnchorStart, AnchorEnd and AnchorVersion stay empty, and that is a
		// statement rather than an omission: no anchor was derived, resolved or
		// spent here. Stamping the anchor contract on a record that never used
		// it would invite a reader of journal.EditApplied to conclude the edit
		// was anchored, which is the one conclusion this mode must never
		// support.
		StartLine:    m.StartLine,
		EndLine:      m.EndLine,
		LinesRemoved: end - start + 1,
		LinesAdded:   len(repl),
		BytesBefore:  len(src),
		BytesAfter:   len(next),
		Diff:         unifiedDiff(p.Slash(), lines, start, end, repl),
		Fuzzy:        info,
	}
	res.Output = res.render()
	return res, nil
}

// fuzzyFloor is the floor this set runs under.
//
// A value outside (0, 1] falls back to the default. The zero value is the case
// that matters: a Limits built by hand and missing this field would otherwise
// mean "every region matches", turning the one approximate path in this package
// into an unconditional one — a forgotten field silently becoming the most
// dangerous possible configuration. Failing to the documented default is the
// direction ADR-0006 asks us to be wrong in.
func (s *Set) fuzzyFloor() float64 {
	if f := s.Limits.FuzzyFloor; f > 0 && f <= 1 {
		return f
	}
	return DefaultFuzzyFloor
}

// cancelledFuzzy is the result of a call the context ended before it wrote.
func cancelledFuzzy(req FuzzyEditRequest, floor float64) EditResult {
	r := EditResult{
		Path:      req.Path,
		Mode:      ModeFuzzy,
		Cancelled: true,
		Fuzzy:     &FuzzyInfo{Floor: floor},
	}
	r.Output = r.render()
	return r
}

// rejectedFuzzy is the result of a fuzzy edit that was refused. The file is
// untouched, and the tests assert that byte for byte rather than trusting the
// error.
func rejectedFuzzy(req FuzzyEditRequest, p Path, info *FuzzyInfo, rej *EditRejection) EditResult {
	return EditResult{
		Path:      p.Slash(),
		Mode:      ModeFuzzy,
		Rejection: rej,
		Fuzzy:     info,
		Output:    rej.Detail,
	}
}

// --- matching ---------------------------------------------------------------

// rankRegions scores every region of the file the same height as the needle,
// and returns those at or above the floor, the closest [NearMissCount] overall,
// and how many were compared.
//
// **Regions are exactly as tall as the needle, and no other height is tried.**
// A model that dropped or added a line therefore does not match, and is told so
// with near misses. Admitting neighbouring heights would look more forgiving
// and be strictly worse: a region and its one-line-larger superset both score
// well against the same needle, so nearly every successful match would arrive
// with a spurious rival above the floor and be refused as ambiguous. Refusing
// for a reason the model can act on beats refusing for one it cannot.
//
// The scan is exact — every region is compared unless it is provably incapable
// of affecting either answer. Levenshtein distance is at least the difference
// in length, so a region whose normalised length differs enough has a bounded
// best-possible score; when that bound is below both the floor and the current
// third-best near miss, the region can neither match nor be reported, and the
// comparison is skipped. [regionScore] is never above that bound, so the skip
// only ever removes work whose outcome is already determined and the result
// does not depend on the order of the scan.
func rankRegions(lines, needle []string, floor float64) (above, near []FuzzyMatch, scanned int) {
	n := len(needle)
	if n == 0 || n > len(lines) {
		return nil, nil, 0
	}

	wantLines := normalizeLines(needle)
	want := strings.Join(wantLines, "\n")
	wantLen := utf8.RuneCountInString(want)

	for i := 0; i+n <= len(lines); i++ {
		scanned++
		region := lines[i : i+n]
		gotLines := normalizeLines(region)
		got := strings.Join(gotLines, "\n")

		if len(near) == NearMissCount {
			bound := lengthBound(wantLen, utf8.RuneCountInString(got))
			if bound < floor && bound < near[len(near)-1].Similarity {
				continue
			}
		}

		m := FuzzyMatch{
			StartLine:  i + 1,
			EndLine:    i + n,
			Similarity: regionScore(wantLines, want, gotLines, got),
			Lines:      slices.Clone(region),
		}
		if m.Similarity >= floor {
			above = append(above, m)
		}
		near = keepNearest(near, m)
	}
	return above, near, scanned
}

// regionScore is how close a region is to the text the model gave: the lowest
// of the whole block's similarity, its first line's, and its last line's. A
// region is no closer than its worst part.
//
// **The two boundary terms exist because of a failure the corpus found rather
// than because they seemed tidy.** A block's score is dominated by its interior,
// so a window offset by one line still scores extremely well — a five-line
// function preceded by a blank line scores 0.96 against the window that starts
// at the blank and stops one line short. Without the boundary terms nearly
// every successful multi-line match would arrive with one or two shifted
// neighbours above the floor and be refused as ambiguous, and the model would
// be told to choose between "lines 2-6" and "lines 3-7" — a refusal it cannot
// act on, for a file that is not actually repetitive.
//
// It is a *requirement*, not a preference, and the difference is the whole
// subject of this card. It narrows what counts as a match; it never chooses
// between two things that qualify. Two regions that both begin and end where
// the model said, and are both close throughout, are still ambiguous and are
// still refused.
//
// Taking the minimum rather than filtering separately keeps one number for one
// region, so a near miss's score never contradicts the refusal that reports it:
// "nothing reached 0.90, the closest was 0.96" is not a sentence that can occur.
func regionScore(wantLines []string, want string, gotLines []string, got string) float64 {
	score := similarity(want, got)
	if len(wantLines) < 2 {
		// A one-line region *is* its own first and last line.
		return score
	}
	last := len(wantLines) - 1
	return min(score,
		similarity(wantLines[0], gotLines[0]),
		similarity(wantLines[last], gotLines[last]))
}

// keepNearest inserts m into the running near-miss list, closest first, keeping
// at most [NearMissCount]. Equal scores keep the earlier region, so the report
// reads down the file.
func keepNearest(near []FuzzyMatch, m FuzzyMatch) []FuzzyMatch {
	at := len(near)
	for i, have := range near {
		if m.Similarity > have.Similarity {
			at = i
			break
		}
	}
	if at == len(near) && len(near) == NearMissCount {
		return near
	}
	near = slices.Insert(near, at, m)
	if len(near) > NearMissCount {
		near = near[:NearMissCount]
	}
	return near
}

// lengthBound is the highest similarity two normalised blocks of these rune
// lengths could possibly have, because Levenshtein distance is never less than
// the difference in length.
func lengthBound(a, b int) float64 {
	longest := max(a, b)
	if longest == 0 {
		return 1
	}
	return 1 - float64(max(a, b)-min(a, b))/float64(longest)
}

// normalizeLines is what "normalised whitespace" means here, and the exact
// statement matters in both directions: normalisation is what makes the
// fallback useful, and it is also what makes two distinct regions collide.
//
// It collapses, and these become indistinguishable:
//
//   - tabs against spaces, and any depth of leading indentation;
//   - trailing whitespace;
//   - any run of whitespace *between* two non-space characters, which becomes
//     one space;
//   - CRLF against LF, because [anchor.Split] has already dropped the "\r".
//
// It does **not** collapse, and these stay different:
//
//   - the presence of whitespace against its absence: "a b" and "ab" differ by
//     a character, so `foo(a, b)` never matches `foo(a,b)` exactly;
//   - blank lines, which normalise to empty lines and are still lines — a
//     region with a blank line in it is not the same as one without;
//   - the number of lines, since a region is compared only against regions of
//     the same height;
//   - letter case, so a renamed identifier is a difference and not a match;
//   - anything else: comments, punctuation, identifiers, string contents.
//
// When normalisation *does* make two genuinely different regions equal — the
// same block indented with tabs in one place and spaces in another — the
// answer is the ambiguity refusal, not a tiebreak. That case is a test.
func normalizeLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = normalizeLine(l)
	}
	return out
}

// normalizeLine trims a line's outer whitespace and collapses each inner run to
// a single space. "Whitespace" is Unicode's, so a non-breaking space collapses
// too — a model that copied one out of rendered output should not be refused
// for it.
func normalizeLine(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	pending, started := false, false
	for _, r := range s {
		if unicode.IsSpace(r) {
			pending = true
			continue
		}
		if pending && started {
			b.WriteByte(' ')
		}
		pending, started = false, true
		b.WriteRune(r)
	}
	return b.String()
}

// similarity scores two normalised blocks in [0, 1].
//
// The metric is **Levenshtein distance over runes, normalised by the length of
// the longer block**: 1 - d/max(len). SLICE-1's unit test plan asks for a score
// that is symmetric, whitespace-normalising and monotonic in edit distance, and
// this is the smallest thing that is all three by construction — Levenshtein is
// symmetric and the divisor is symmetric, the inputs are normalised by
// [normalizeLines], and for a fixed pair of lengths the score is strictly
// decreasing in the distance. Nothing here is learned, weighted or tuned, so a
// score means the same thing in every file.
//
// Runes rather than bytes, so a multi-byte character counts as one edit and not
// as three. Token- or line-level scoring was the alternative and is rejected:
// at line granularity a region differing by one identifier and a region
// differing entirely both score 1 - 1/n, which is precisely the discrimination
// the adversarial cases need.
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	longest := max(len(ra), len(rb))
	if longest == 0 {
		return 1
	}
	return 1 - float64(levenshtein(ra, rb))/float64(longest)
}

// levenshtein is the edit distance between two rune slices, two rows at a time.
func levenshtein(a, b []rune) int {
	if len(a) < len(b) {
		a, b = b, a
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// --- refusal ----------------------------------------------------------------

// fuzzyReject builds the refusal the model reads and the engine journals.
//
// It is reached for both of the fallback's refusals, because both are decided
// by the same count: not one region above the floor. Zero is below_floor and
// carries the near misses; more than one is ambiguous and carries all of them.
func fuzzyReject(req FuzzyEditRequest, floor float64, above, near []FuzzyMatch,
	total, height int,
) *EditRejection {
	rej := &EditRejection{Field: "before", Matches: near}
	if len(above) > 1 {
		rej.Reason = RejectAmbiguousAnchor
		rej.Matches = above
		rej.Candidates = make([]int, len(above))
		for i, m := range above {
			rej.Candidates[i] = m.StartLine
		}
	} else {
		rej.Reason = RejectBelowFloor
	}

	rej.Summary = fuzzySummary(rej.Reason, floor, rej.Matches, total, height)
	rej.Detail = fuzzyDetail(req.Path, rej, floor, total)
	return rej
}

// fuzzySummary is the one-line form, which is also the tool error's detail.
func fuzzySummary(reason RejectReason, floor float64, matches []FuzzyMatch, total, height int) string {
	if reason == RejectAmbiguousAnchor {
		return fmt.Sprintf("the text matches %d regions at or above the floor of %s (%s), "+
			"so it does not name one region",
			len(matches), formatScore(floor), joinRegions(matches))
	}
	switch {
	case total == 0:
		return "the file is empty, so there is no region to match"
	case height > total:
		return fmt.Sprintf("the text is %s and the file has %s, so there is no region of "+
			"that height to compare it against",
			plural(height, "line", "lines"), plural(total, "line", "lines"))
	case len(matches) == 0:
		return fmt.Sprintf("no region of %s reached the floor of %s",
			plural(height, "line", "lines"), formatScore(floor))
	default:
		best := matches[0]
		return fmt.Sprintf("no region reached the floor of %s; the closest scored %s at %s",
			formatScore(floor), formatScore(best.Similarity), regionRange(best))
	}
}

// fuzzyDetail is the full model-facing text: what was refused, why, what to do
// about it, and the regions themselves with their line numbers.
//
// Nothing here is clipped. The regions echoed back are the same height as the
// text the model itself supplied, so the report is bounded by an input the
// model already spent the tokens on — there is no file-sized dump to guard
// against, and CLAUDE.md's rule against truncating the diagnostic output that
// justifies a fix applies with full force to a near-miss report, which is the
// only thing the model has to retry from.
func fuzzyDetail(path string, rej *EditRejection, floor float64, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: rejected, the file was not changed\n", ToolEditFileFuzzy, path)
	fmt.Fprintf(&b, "reason: %s — %s\n", rej.Reason, rej.Summary)
	b.WriteString(fuzzyAdvice(rej.Reason, total))
	b.WriteByte('\n')

	if len(rej.Matches) == 0 {
		return b.String()
	}
	if rej.Reason == RejectAmbiguousAnchor {
		b.WriteString("every region at or above the floor:\n")
	} else {
		fmt.Fprintf(&b, "the %s closest, nearest first:\n", plural(len(rej.Matches), "region", "regions"))
	}

	width := len(strconv.Itoa(total))
	for _, m := range rej.Matches {
		fmt.Fprintf(&b, "  %s, similarity %s\n", regionRange(m), formatScore(m.Similarity))
		for i, l := range m.Lines {
			// Line numbers, never anchors: this quotes content the model may
			// never have read, and anchors come only from read_file.
			fmt.Fprintf(&b, "  %*d| %s\n", width, m.StartLine+i, l)
		}
	}
	return b.String()
}

// fuzzyAdvice is the sentence that turns a refusal into a next move. Both
// reasons end up pointing at anchored mode, and they should: this is the
// fallback, so the way out of a fallback failure is the primary path.
func fuzzyAdvice(reason RejectReason, total int) string {
	if reason == RejectAmbiguousAnchor {
		return "the file contains this text in more than one place, so it does not say which; " +
			"read_file the one you mean and use edit_file with its anchors — this tool never " +
			"picks between matches"
	}
	if total == 0 {
		return "the file has no lines to match against; use write_file to give it content"
	}
	return "the text is not in the file in a recognisable form; read_file the region you " +
		"mean and use edit_file with its anchors, which needs no quoting at all"
}

// regionRange names a region the way the rest of this package names one.
func regionRange(m FuzzyMatch) string {
	if m.StartLine == m.EndLine {
		return fmt.Sprintf("line %d", m.StartLine)
	}
	return fmt.Sprintf("lines %d-%d", m.StartLine, m.EndLine)
}

// joinRegions lists regions for a message.
func joinRegions(ms []FuzzyMatch) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = regionRange(m)
	}
	return strings.Join(parts, ", ")
}

// formatScore renders a similarity for a human and for a model, at the two
// decimal places the floor is stated to. More would imply a precision the
// metric does not carry.
func formatScore(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}
