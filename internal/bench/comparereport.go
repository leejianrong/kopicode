package bench

import (
	"fmt"
	"io"
	"strings"
)

// WriteCompareReport renders a paired [Result] alongside the two runs it
// scored, as the text a human reads.
//
// It follows [WriteReport]'s own rule: everything printed here is derived
// from the two [RunResult] values and the [Result] [Compare] produced from
// them, and nothing is a second record. The poolability notes below are the
// one thing this file computes rather than reads off [Result] — ADR-0007
// decision 7 and ADR-0005's experiment-series boundary are both refusals a
// human has to be told about, because [Compare] itself only refuses an
// unpaired task set, and a comparison across two different corpora or two
// dirty builds is arithmetically well-defined and evidentially worthless.
func WriteCompareReport(w io.Writer, res Result, a, b *RunResult) error {
	var buf strings.Builder

	fmt.Fprintf(&buf, "paired comparison: %s vs %s\n\n", res.ArmA, res.ArmB)
	writeCompareRunSummary(&buf, "A", a)
	writeCompareRunSummary(&buf, "B", b)
	buf.WriteString("\n")

	if notes := poolabilityNotes(a, b); len(notes) > 0 {
		for _, note := range notes {
			fmt.Fprintf(&buf, "  NOTE  %s\n", note)
		}
		buf.WriteString("\n")
	}

	writeContingencyTable(&buf, res.Table)
	buf.WriteString("\n")

	fmt.Fprintf(&buf, "  discordant pairs   %d (of %d task(s))\n", res.Discordant(), res.Table.Pairs())
	fmt.Fprintf(&buf, "  method             %s\n", res.Method)
	if res.Method == MethodChiSquared {
		fmt.Fprintf(&buf, "  statistic          %.4f (1 df, Edwards' continuity correction)\n", res.Statistic)
	}
	fmt.Fprintf(&buf, "  p-value            %.4f\n", res.P)
	fmt.Fprintf(&buf, "  direction          %s\n", res.Direction)
	buf.WriteString("\n")
	buf.WriteString(verdictLine(res))
	buf.WriteString("\n")

	_, err := io.WriteString(w, buf.String())
	return err
}

func writeCompareRunSummary(b *strings.Builder, label string, r *RunResult) {
	fmt.Fprintf(b, "  %s   run %s\n", label, r.RunID)
	fmt.Fprintf(b, "      arm       %s / %s (hash %s)\n",
		r.Arm.ModelID, r.Arm.HarnessConfigName, short(r.Arm.HarnessConfigHash))
	fmt.Fprintf(b, "      pin       %s\n", r.Arm.ProviderPin)
	fmt.Fprintf(b, "      provider  %s\n", r.Provider)
	fmt.Fprintf(b, "      build     %s %s (%s, %s)\n",
		r.Arm.Build.Version, short(r.Arm.Build.Commit), r.Arm.Build.TreeState, r.Arm.Build.Source)
	fmt.Fprintf(b, "      corpus    %s %s at %s\n", r.CorpusVersion, r.CorpusDigest, short(r.Commit))
	fmt.Fprintf(b, "      result    %d/%d passed, %d errored, %d tokens\n",
		r.Passed(), len(r.Tasks), r.Errored(), totalTokens(r))
	buckets := r.Buckets()
	fmt.Fprintf(b, "      buckets   harness %d, unattributed %d, model %d, unclassified %d (of %d failed)\n",
		buckets.Harness, buckets.Unattributed, buckets.Model, buckets.Unclassified, buckets.Failed)
}

func totalTokens(r *RunResult) int {
	n := 0
	for _, t := range r.Tasks {
		n += t.Tokens.Total
	}
	return n
}

// poolabilityNotes flags the ways two runs can score cleanly under [Compare]
// while still not being the paired comparison ADR-0005 and ADR-0007 ask for.
func poolabilityNotes(a, b *RunResult) []string {
	var notes []string
	if a.CorpusDigest != "" && b.CorpusDigest != "" && a.CorpusDigest != b.CorpusDigest {
		notes = append(notes, fmt.Sprintf(
			"corpus digest differs (A: %s, B: %s) — ADR-0005's experiment-series boundary means "+
				"these are two different corpora, not two arms over one", short(a.CorpusDigest), short(b.CorpusDigest)))
	}
	if a.Arm.Build.TreeState != "clean" {
		notes = append(notes, fmt.Sprintf(
			"run A's build tree state is %q — ADR-0007 decision 7: not poolable with anything",
			a.Arm.Build.TreeState))
	}
	if b.Arm.Build.TreeState != "clean" {
		notes = append(notes, fmt.Sprintf(
			"run B's build tree state is %q — ADR-0007 decision 7: not poolable with anything",
			b.Arm.Build.TreeState))
	}
	if a.Provider != b.Provider {
		notes = append(notes, fmt.Sprintf(
			"providers differ (A: %s, B: %s) — comparing live traffic against a replayed fixture "+
				"is not an A/B, one side answers questions the other side does not see",
			a.Provider, b.Provider))
	} else if a.Provider == ProviderMock {
		notes = append(notes, "both runs replayed the mock provider — a real A/B needs --provider live")
	}
	return notes
}

func writeContingencyTable(b *strings.Builder, t Table) {
	b.WriteString("  contingency table (rows: A, columns: B)\n")
	fmt.Fprintf(b, "                 B pass   B fail\n")
	fmt.Fprintf(b, "    A pass       %6d   %6d\n", t.BothPassed, t.AOnly)
	fmt.Fprintf(b, "    A fail       %6d   %6d\n", t.BOnly, t.BothFailed)
}

// verdictLine spells the statistic out in one sentence, because a p-value and
// a direction printed as two fields still leave "so which arm actually won"
// as an inference the reader has to do themselves.
func verdictLine(r Result) string {
	const alpha = 0.05
	switch {
	case r.Method == MethodNone:
		return "  verdict: the arms agreed on every task; there is no evidence of a difference."
	case r.P >= alpha:
		return fmt.Sprintf(
			"  verdict: not significant at alpha=%.2f (p=%.4f) — %s, but the discordant pairs are "+
				"too few or too close to call.", alpha, r.P, r.Direction)
	default:
		return fmt.Sprintf(
			"  verdict: significant at alpha=%.2f (p=%.4f) — %s.", alpha, r.P, r.Direction)
	}
}
