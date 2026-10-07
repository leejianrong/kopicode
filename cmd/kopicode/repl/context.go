package repl

import (
	"fmt"
	"strings"

	"github.com/leejianrong/kopicode/internal/engine"
)

// UsageFunc reports the session's usage now. It is the one thing /context needs
// from the session, passed in so this package never holds the session itself.
type UsageFunc func() engine.Usage

// showContext prints the /context report.
func (l *Loop) showContext() {
	if l.usage == nil {
		l.Notice("/context is not available: this session has no usage to report")
		return
	}
	for _, line := range ContextReport(l.usage()) {
		l.out.line(line)
	}
}

// ContextReport renders usage as the lines /context prints.
//
// Two different numbers are reported on purpose. The context line is the latest
// request's prompt: what the model holds now, and so how near its window is.
// The spent line is the sum over every request, which is what was billed and
// what the budget counts, and which grows much faster because each request
// resends the history. Showing only the second would make a healthy session
// look nearly full.
//
// Unknowns are said to be unknown. A window the binary does not have, and a
// cost the provider did not report for every request, are never estimated.
func ContextReport(u engine.Usage) []string {
	var lines []string

	switch {
	case u.Requests == 0:
		lines = append(lines, "context  not measured yet: the model has not responded in this session")
	case u.ContextWindow > 0:
		pct := 100 * float64(u.ContextTokens) / float64(u.ContextWindow)
		lines = append(lines, fmt.Sprintf("context  %s of %s tokens (%.1f%%) in the latest request",
			group(u.ContextTokens), group(u.ContextWindow), pct))
	default:
		lines = append(lines, fmt.Sprintf("context  %s tokens in the latest request (this model's window is not known)",
			group(u.ContextTokens)))
	}

	spent := fmt.Sprintf("spent    %s tokens this session (prompt %s, completion %s)",
		group(u.Total), group(u.Prompt), group(u.Completion))
	if u.TokenBudget > 0 {
		spent += fmt.Sprintf(", budget %s", group(u.TokenBudget))
	}
	lines = append(lines, spent)

	if u.CacheRead > 0 || u.CacheWrite > 0 {
		lines = append(lines, fmt.Sprintf("cached   %s prompt tokens read from the provider's cache, %s written",
			group(u.CacheRead), group(u.CacheWrite)))
	}

	switch {
	case u.Requests == 0:
	case u.CostUSD != nil:
		lines = append(lines, "cost     "+dollars(*u.CostUSD)+", as reported by the provider")
	default:
		lines = append(lines, "cost     unknown: the provider did not report a price for every request")
	}

	turns := fmt.Sprintf("turns    %d so far", u.Turns)
	if u.MaxTurns > 0 {
		turns += fmt.Sprintf("; the cap is %d per prompt", u.MaxTurns)
	}
	return append(lines, turns)
}

// group renders n with thousands separators.
func group(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// dollars renders a small price without rounding it to nothing.
func dollars(v float64) string {
	if v != 0 && v < 0.0001 {
		return fmt.Sprintf("$%.6f", v)
	}
	return fmt.Sprintf("$%.4f", v)
}
