// Package wrap lays out the help text the CLI prints, so it fits whatever
// terminal width it is given.
package wrap

import "strings"

// Wrap breaks text into lines of at most width characters, splitting only at
// whitespace. A word longer than width gets a line of its own.
func Wrap(text string, width int) []string {
	if width < 1 {
		return nil
	}

	var lines []string
	current := ""

	for _, word := range strings.Fields(text) {
		switch {
		case current == "":
			current = word
		case len(current)+1+len(word) <= width:
			current += " " + word
		default:
			lines = append(lines, current)
			current = word
		}
	}

	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// Indent prefixes every line with prefix.
func Indent(lines []string, prefix string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = prefix + line
	}
	return out
}

// Longest returns the length of the longest line, or zero for no lines.
func Longest(lines []string) int {
	longest := 0
	for _, line := range lines {
		if len(line) > longest {
			longest = len(line)
		}
	}
	return longest
}
