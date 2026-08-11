package tools

import (
	"path"
	"strings"
)

// SLICE-1 drops `glob` as a distinct tool on the grounds that list_dir and grep
// cover it between them. That only holds if one of them can find a file by
// name, so list_dir takes a name pattern and grep takes the same pattern as an
// include filter — one matcher, one semantics, so a pattern that lists a set of
// files also greps exactly that set.

// matchName reports whether a root-relative slash path matches pattern.
//
// The pattern is path.Match syntax, with two accommodations for what models
// actually write:
//
//   - A pattern with no slash matches the base name, so "*.go" finds Go files
//     at any depth rather than only at the root. A pattern with a slash matches
//     the whole root-relative path, so "internal/*/*.go" means what it says.
//   - A leading "**/" is stripped. Doublestar is not path.Match syntax and
//     implementing it is a matcher of its own; stripping the one form that
//     appears in practice ("**/*_test.go") costs nothing and is honest about
//     its limits. A "**" anywhere else is left alone and will simply not match,
//     which is a visibly empty result rather than a wrong one.
//
// A malformed pattern is reported, never treated as "matches nothing" — a
// silently empty listing is indistinguishable from an empty directory, and that
// is a turn the model spends looking in the wrong place.
func matchName(pattern, rel string) (bool, error) {
	if pattern == "" {
		return true, nil
	}
	pattern = strings.TrimPrefix(pattern, "**/")
	if !strings.Contains(pattern, "/") {
		rel = path.Base(rel)
	}
	return path.Match(pattern, rel)
}

// validPattern checks a pattern once, up front, so a bad one is a single clear
// refusal rather than the same error repeated per entry.
func validPattern(pattern string) error {
	_, err := matchName(pattern, "probe")
	return err
}
