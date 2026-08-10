package parse

import "strings"

// fenceMarker opens and closes a fenced block.
const fenceMarker = "```"

// toolFenceLabels are the info strings that make a fence a tool-call fence.
// The list is deliberately short: ```json is not on it, because a model
// illustrating a JSON payload in prose writes exactly that, and reading it as a
// call would execute something the model never asked for.
var toolFenceLabels = map[string]bool{
	"tool":       true,
	"tool_call":  true,
	"tool_calls": true,
	"toolcall":   true,
}

// fence is one fenced block found in the reply text.
type fence struct {
	label   string // the info string, trimmed and lowercased
	content string // everything between the fences
	raw     string // the block including its fences
	closed  bool
}

// isTool reports whether the fence claims to hold a tool call.
func (f fence) isTool() bool { return toolFenceLabels[f.label] }

// extractFenced reads route (b): a ```tool fenced JSON block.
//
// A tool fence that is opened and never closed fails rather than being parsed
// from whatever text follows it. A truncated stream is the common cause, and
// the tail of a truncated call is the last thing that should be run.
func extractFenced(text string) ([]ToolCall, *Error) {
	if !strings.Contains(text, fenceMarker) {
		return nil, nil
	}

	var out []ToolCall
	for _, f := range scanFences(text) {
		if !f.isTool() {
			continue
		}
		if !f.closed {
			return nil, &Error{
				Kind:    KindUnclosedFence,
				Route:   RouteFencedJSON,
				Detail:  "a ```" + f.label + " fence was opened and never closed",
				Snippet: snippet(f.raw),
			}
		}
		calls, err := decodeStream(RouteFencedJSON, f.content, f.raw)
		if err != nil {
			return nil, err
		}
		out = append(out, calls...)
	}
	return out, nil
}

// scanFences returns every fenced block in text, in order.
//
// It is hand-rolled rather than a regexp so that an unclosed fence is a
// reportable state rather than a pattern that simply fails to match, and so
// that pathological model output cannot find a backtracking cliff.
func scanFences(text string) []fence {
	lines := strings.SplitAfter(text, "\n")

	var out []fence
	i := 0
	for i < len(lines) {
		open := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(open, fenceMarker) {
			i++
			continue
		}

		ticks := countLeadingBackticks(open)
		label := strings.ToLower(strings.TrimSpace(open[ticks:]))
		start := i
		i++

		var body strings.Builder
		closed := false
		for i < len(lines) {
			line := strings.TrimSpace(lines[i])
			if isClosingFence(line, ticks) {
				closed = true
				i++
				break
			}
			body.WriteString(lines[i])
			i++
		}

		out = append(out, fence{
			label:   label,
			content: body.String(),
			raw:     strings.Join(lines[start:i], ""),
			closed:  closed,
		})
	}
	return out
}

// isClosingFence reports whether a trimmed line closes a fence opened with
// ticks backticks. A closing fence carries no info string, which is what stops
// a nested ```tool inside an open ```go block from being read as a close.
func isClosingFence(line string, ticks int) bool {
	if !strings.HasPrefix(line, fenceMarker) {
		return false
	}
	n := countLeadingBackticks(line)
	if n < ticks {
		return false
	}
	return strings.TrimSpace(line[n:]) == ""
}

func countLeadingBackticks(s string) int {
	n := 0
	for n < len(s) && s[n] == '`' {
		n++
	}
	return n
}
