// Package strutil holds the small string helpers the report writer uses.
package strutil

import "strings"

// Reverse returns s with its characters in reverse order.
func Reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// Initials returns the first character of each whitespace-separated word in s,
// joined together. Empty input gives an empty string.
func Initials(s string) string {
	parts := strings.Fields(s)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		out = append(out, string([]rune(part)[:1]))
	}
	return strings.Join(out, "")
}

// Truncate shortens s to at most n characters, appending an ellipsis when it
// had to cut. n counts characters, not bytes.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
