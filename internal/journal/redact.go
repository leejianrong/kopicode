package journal

import (
	"bytes"
	"sort"
)

// DefaultSecretEnv names the environment variables whose values must never
// reach the record.
//
// The list is short on purpose. Redaction here is not a scan for
// credential-shaped strings — a denylist of shapes misses the next provider's
// format, and kopicode already knows the actual secret because it read it from
// the environment to make the request.
func DefaultSecretEnv() []string {
	return []string{"OPENROUTER_API_KEY"}
}

// minSecretLen is the shortest value worth redacting.
//
// A one- or two-character environment value is not a credential, and replacing
// every occurrence of "1" in the record with a placeholder would corrupt the
// journal to protect nothing. A key short enough to fall under this is short
// enough to be a placeholder itself.
const minSecretLen = 12

// redaction is one literal to remove and what replaces it.
type redaction struct {
	needle      []byte
	replacement []byte
}

// redactor removes known secret values from an encoded line before it is
// written.
//
// # Why at append time, and why on the bytes
//
// The payload types are already structurally unable to hold a credential:
// there is no map, no headers field, and two reflective guards in
// payload_test.go keep it that way. What no type can prevent is a model running
// `env` in a shell and the API key arriving verbatim inside ToolResult.Output.
// Append is the last place that can stop it, so redaction happens here.
//
// It runs over the encoded line rather than over the payload struct, which
// means it also covers an UnknownPayload's preserved bytes and any envelope
// field a newer build added — neither of which a field-by-field walk would
// know about.
//
// The values themselves are never logged, never journaled and never included in
// an error message. FileJournal has a String method for the same reason.
type redactor struct {
	pairs []redaction
}

// newRedactor reads the named variables through lookup and prepares the
// substitutions. A variable that is unset, empty or too short contributes
// nothing.
func newRedactor(names []string, lookup func(string) (string, bool)) *redactor {
	r := &redactor{}
	for _, name := range names {
		v, ok := lookup(name)
		if !ok || len(v) < minSecretLen {
			continue
		}
		replacement := []byte("[redacted:" + name + "]")
		r.pairs = append(r.pairs, redaction{needle: []byte(v), replacement: replacement})

		// The line is JSON, so a value containing a quote or a backslash lands
		// on disk in escaped form and the literal would not match it. Look for
		// both spellings of the same secret.
		if esc := jsonStringBody(v); esc != v {
			r.pairs = append(r.pairs, redaction{needle: []byte(esc), replacement: replacement})
		}
	}

	// Longest first: if one secret contains another, replacing the shorter one
	// first would leave the longer one's remaining bytes on disk.
	sort.SliceStable(r.pairs, func(i, j int) bool {
		return len(r.pairs[i].needle) > len(r.pairs[j].needle)
	})
	return r
}

// scrub returns line with every known secret replaced, and reports whether
// anything changed. The input is left untouched when there is nothing to do,
// so the common case costs one substring search per secret and no allocation.
func (r *redactor) scrub(line []byte) ([]byte, bool) {
	if r == nil || len(r.pairs) == 0 {
		return line, false
	}
	out, changed := line, false
	for _, p := range r.pairs {
		if !bytes.Contains(out, p.needle) {
			continue
		}
		out = bytes.ReplaceAll(out, p.needle, p.replacement)
		changed = true
	}
	return out, changed
}

// jsonStringBody returns s as it appears inside a JSON string on disk, without
// the surrounding quotes. It encodes the same way the journal does — HTML
// unescaped — so the two spellings cannot drift apart.
func jsonStringBody(s string) string {
	b, err := encode(s)
	if err != nil || len(b) < 2 {
		return s
	}
	return string(b[1 : len(b)-1])
}
