package bench

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// WriteReport renders a run as the text a human reads.
//
// It is derived entirely from [RunResult], which is itself derived from the
// journal and the oracle's exit status. Nothing here is a second record: a
// number that appears in this report and nowhere else would be a claim nobody
// can check.
//
// Four things it will not do. It does not omit the reclamation counts, because
// silent cleanup and silent accumulation look identical from outside and the
// whole card is that sentence. It does not omit the unclassified bucket, because
// a classifier that has not run must not read as one that found nothing. It
// does not omit an empty bucket either — ADR-0006 §3 asks for `unattributed`'s
// size on every result, and a bucket printed only when it is non-zero says the
// same nothing whether it is empty or uncomputed. And it does not clip an
// oracle's output — that goes to the run directory, whole, where the report
// points at it.
func WriteReport(w io.Writer, r *RunResult) error {
	var b strings.Builder

	fmt.Fprintf(&b, "run %s — %d task(s), %d passed, %d errored\n",
		r.RunID, len(r.Tasks), r.Passed(), r.Errored())
	fmt.Fprintf(&b, "  arm       %s / %s (hash %s)\n",
		r.Arm.ModelID, r.Arm.HarnessConfigName, short(r.Arm.HarnessConfigHash))
	fmt.Fprintf(&b, "  pin       %s\n", r.Arm.ProviderPin)
	fmt.Fprintf(&b, "  build     %s %s (%s, %s)\n",
		r.Arm.Build.Version, short(r.Arm.Build.Commit), r.Arm.Build.TreeState, r.Arm.Build.Source)
	fmt.Fprintf(&b, "  corpus    %s %s at %s\n", r.CorpusVersion, r.CorpusDigest, short(r.Commit))
	fmt.Fprintf(&b, "  jobs      %d\n", r.Jobs)
	fmt.Fprintf(&b, "  output    %s\n", r.OutDir)
	fmt.Fprintf(&b, "  elapsed   %s\n", round(r.Duration))

	// ADR-0007 decision 7: a build that is not clean is not poolable with
	// anything, not even another dirty build from the same commit. Saying so
	// next to the numbers is cheaper than discovering it in a comparison.
	if r.Arm.Build.TreeState != "clean" {
		fmt.Fprintf(&b, "  NOTE      the build tree state is %q, so these results are not poolable "+
			"with any other run (ADR-0007 decision 7)\n", r.Arm.Build.TreeState)
	}

	b.WriteString("\n")
	writeTasks(&b, r)
	b.WriteString("\n")
	writeBuckets(&b, r.Buckets())
	b.WriteString("\n")
	writeReclamation(&b, r.Reclamation)

	_, err := io.WriteString(w, b.String())
	return err
}

func writeTasks(b *strings.Builder, r *RunResult) {
	width := len("task")
	for _, t := range r.Tasks {
		if len(t.TaskID) > width {
			width = len(t.TaskID)
		}
	}

	fmt.Fprintf(b, "  %-*s  %-6s  %-16s  %5s  %8s  %9s  %s\n",
		width, "task", "result", "stop", "turns", "tokens", "elapsed", "bucket")
	for _, t := range r.Tasks {
		fmt.Fprintf(b, "  %-*s  %-6s  %-16s  %5d  %8d  %9s  %s\n",
			width, t.TaskID, verdict(t), orDash(t.Stop), t.Turns,
			t.Tokens.Total, round(t.Duration), bucketLabel(t))
	}

	for _, t := range r.Tasks {
		for _, note := range taskNotes(t) {
			fmt.Fprintf(b, "  %s: %s\n", t.TaskID, note)
		}
	}
}

// taskNotes are the things about a task that a column cannot hold and a reader
// must not have to go looking for.
func taskNotes(t TaskResult) []string {
	var notes []string
	if t.Panicked {
		notes = append(notes, "the session panicked; the worktree was still reclaimed")
	}
	if t.SessionErr != "" && !t.Panicked {
		notes = append(notes, "session: "+oneLine(t.SessionErr))
	}
	if t.Oracle.TimedOut {
		notes = append(notes, fmt.Sprintf("the oracle timed out after %s, so nothing was measured",
			round(t.Oracle.Duration)))
	}
	if t.Oracle.Err != nil && !t.Oracle.TimedOut {
		notes = append(notes, "oracle: "+oneLine(t.Oracle.Err.Error()))
	}
	if t.Oracle.Signal != "" {
		notes = append(notes, "the oracle was killed by "+t.Oracle.Signal+
			", which is not the same as a suite that failed")
	}
	if t.WorktreeKept && t.Worktree != "" {
		notes = append(notes, "worktree kept at "+t.Worktree)
	}
	return notes
}

func writeReclamation(b *strings.Builder, c Reclamation) {
	fmt.Fprintf(b, "  worktrees: created %d, removed %d, kept %d\n", c.Created, c.Removed, c.Kept)
	if c.PrunedAdmin > 0 || c.PrunedStale > 0 {
		fmt.Fprintf(b, "  reclaimed at run start: %d orphaned checkout(s), %d stale registration(s) "+
			"— a previous run did not finish, or kept its worktrees\n", c.PrunedStale, c.PrunedAdmin)
	}
	if c.Kept > 0 {
		b.WriteString("  kept worktrees are reclaimed by the NEXT run: inspect before you re-run\n")
	}
	if len(c.CreateFailed) > 0 {
		fmt.Fprintf(b, "  NOT CREATED (%d): %s — these tasks got no worktree, so nothing was "+
			"measured for them and this run does not cover the whole corpus\n",
			len(c.CreateFailed), strings.Join(c.CreateFailed, ", "))
	}
	if len(c.Failed) > 0 {
		fmt.Fprintf(b, "  NOT RECLAIMED (%d): %s\n", len(c.Failed), strings.Join(c.Failed, ", "))
	}
}

// writeBuckets is SLICE-1 §9's attribution, tallied.
//
// It is printed on every run, in full, with every count present at every size.
// The `unattributed` line is the one ADR-0006 §3 requires by name — "report its
// size every time" — and it is a line of its own rather than a column in the
// tally so that a reader skimming a report cannot miss the number the ADR says
// is a quality signal in its own right.
//
// The denominator is the failing tasks, because that is what is being
// attributed. A run in which nothing failed prints the header and the
// `unattributed` line anyway: "no failures" and "nobody classified the
// failures" must not render as the same silence.
func writeBuckets(b *strings.Builder, c BucketCounts) {
	fmt.Fprintf(b, "  attribution over %d failed task(s): harness %d, unattributed %d, "+
		"model %d, unclassified %d\n", c.Failed, c.Harness, c.Unattributed, c.Model, c.Unclassified)
	fmt.Fprintf(b, "  unattributed = %d — sessions that used the fuzzy edit fallback at any "+
		"point (ADR-0006 §3).\n", c.Unattributed)
	b.WriteString("    It does not detect a misapplied edit, it refuses to launder one, so it " +
		"absorbs\n    some real model failures. Its size is stated on every run, zero included.\n")

	if c.Harness > 0 {
		fmt.Fprintf(b, "  %d failure(s) classified `harness`. The slice-1 bar is zero "+
			"(docs/SLICE-1.md §Test Plan)\n", c.Harness)
	}
	if c.Unclassified > 0 {
		fmt.Fprintf(b, "  %d failure(s) were NOT attributed: no classifier ran, or the one that "+
			"did could not read the record. This is not a clean bill of health\n", c.Unclassified)
	}
}

// bucketLabel prints the unclassified bucket rather than leaving the column
// blank. An empty cell reads as "nothing to report"; this one means "nobody has
// looked yet", and the two are opposite claims.
//
// A task that passed prints neither. Attribution is failure attribution
// (ADR-0006 §3 taints "any failing session"), so a passing row has nothing to
// attribute and "unclassified" there would read as the classifier having been
// skipped.
func bucketLabel(t TaskResult) string {
	if t.Passed {
		return "n/a"
	}
	if t.Bucket == BucketUnclassified {
		return "unclassified"
	}
	return string(t.Bucket)
}

func verdict(t TaskResult) string {
	if t.Passed {
		return "pass"
	}
	return "fail"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	if sha == "" {
		return "-"
	}
	return sha
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func round(d time.Duration) string {
	if d >= time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(time.Millisecond).String()
}
