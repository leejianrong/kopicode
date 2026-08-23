// Package profile validates the fields on a user profile before it is saved.
package profile

import (
	"fmt"
	"strings"
	"unicode"
)

// maxDisplayNameLength is the longest display name the UI will lay out on one
// line without truncating.
const maxDisplayNameLength = 40

// ValidateDisplayName checks a proposed profile display name before it is
// written to storage. Display names may contain letters, digits, spaces,
// hyphens and apostrophes, so that names like "Mary-Jane" or "O'Brien" are
// accepted, and must be no longer than maxDisplayNameLength characters. A
// name that is empty, or holds nothing but whitespace, is rejected: both
// render as a blank name once displayed.
func ValidateDisplayName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("display name cannot be blank")
	}
	if len(name) > maxDisplayNameLength {
		return fmt.Errorf("display name too long: %d characters (max %d)", len(name), maxDisplayNameLength)
	}
	for _, r := range name {
		if !isAllowedNameRune(r) {
			return fmt.Errorf("display name contains invalid character %q", r)
		}
	}
	return nil
}

// isAllowedNameRune reports whether r may appear in a display name.
func isAllowedNameRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '\''
}
