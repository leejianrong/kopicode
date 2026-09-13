package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// AskPolicyFile is docs/adr/0013-agent-controlled-resident-session-surface.md
// decision 6's mechanism: what an orchestrator with nobody at a terminal states,
// up front, that a kopicode session should do when the model calls `ask` and no
// human can answer. It resolves into [Options.AskPolicy], which [Open] turns
// into an [Answerer] (see [AskPolicyFile.answerer]).
//
// # Why this is not a key on the permission allowlist file
//
// It would be one line to add `ask_note = "..."` to
// [permission.AllowlistFile], and that is exactly the crossing this codebase has
// twice now declared out of bounds. ADR-0009 decision 2 declined to fold `ask`
// into internal/permission's runtime type system (no OperationAsk, no KindAsk, no
// Verdict carrying prose), and permission/allowlist_file.go's own doc comment
// declined the analogous merge at the file-format layer for the same reason: two
// mechanisms that only coincidentally look similar. A permission allowlist
// answers "may this action proceed" with a closed verdict; an ask policy answers
// a free-text question with free text. Folding an ask note into that file would
// make a future maintainer read the two as one concern, and ADR-0013 decision 6
// keeps them apart deliberately — this type lives in internal/engine, nowhere
// near internal/permission, the same "sibling mechanism at the same layer" shape
// ask.go already gives the runtime ask contract.
//
// # Why this is not a TOML library, again
//
// The same argument permission/allowlist_file.go and internal/harness/file.go
// already make: a whole TOML implementation is not warranted by the one key
// below, and a hand-rolled flat-key reader duplicating a couple of small parsing
// primitives is cheaper than a dependency and than a shared "TOML fragment"
// package that would couple two otherwise-independent leaves (ADR-0003). The
// primitives here (a bare key, a quoted string) are deliberately duplicated from
// those two readers rather than exported and shared.
//
// # The grammar
//
//	note = "When ask is called and nobody can answer, assume the most conservative
//	interpretation consistent with what has been read so far, proceed, and record
//	the assumption in the final summary."
//
// One required, flat, top-level key and nothing else — no `[table]`, no nesting,
// no second key. `note` is a quoted string (a TOML basic "..." or literal
// '...'), and must be non-empty: a policy that says nothing is a mistake, not a
// declaration, the same way permission/allowlist_file.go refuses an empty
// `root`.
//
// # What it refuses, and why each refusal is better than a guess
//
//   - A `[table]` header, any key besides `note`, a key set twice, a value that
//     is not a quoted string, an empty note, or the key missing entirely. `note`
//     is the whole of this file, so anything else is a typo worth naming rather
//     than a neighbour's business to step around — the identical reasoning
//     permission/allowlist_file.go's doc comment lays out at length.
type AskPolicyFile struct {
	// Path is the file the value came from.
	Path string
	// Note is the `note` key: the orchestrator-authored instruction the model
	// sees when it calls ask and nobody can answer. Never empty after a
	// successful load — LoadAskPolicyFile refuses an empty note, so a caller can
	// tell "no file was loaded" (the zero AskPolicyFile) from a loaded one.
	Note string
}

// askPolicyRefusalPrefix precedes the orchestrator's note in what the model
// sees, framing it: the honest fact (nobody is here) followed by the standing
// instruction the orchestrator left for exactly this case. Kept as one constant
// so the wiring and its test cannot drift on the wording.
const askPolicyRefusalPrefix = "No human is present. Policy note from the invoking orchestrator: "

// LoadAskPolicyFile reads and parses the ask-policy file at path.
//
// Like [permission.LoadAllowlistFile] and unlike
// [github.com/leejianrong/kopicode/internal/harness.LoadFileConfig], a missing
// file here is an error, not "use a built-in default": there is no safe default
// an ask policy could compile to, and a caller that passed --ask-policy-file has
// already said it wants exactly this file. Every error this returns is the
// caller's own configuration mistake to fix — a missing file, a bad path, a
// malformed line — never a harness or provider failure, which is why a front end
// maps every one of them to the usage exit code rather than the harness one.
func LoadAskPolicyFile(path string) (AskPolicyFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AskPolicyFile{}, fmt.Errorf("engine: reading %s: %w", path, err)
	}
	return parseAskPolicyFile(path, string(data))
}

// parseAskPolicyFile reads the flat top-level keys of an ask-policy file. See
// [AskPolicyFile] for the grammar and what is refused.
func parseAskPolicyFile(path, content string) (AskPolicyFile, error) {
	cfg := AskPolicyFile{Path: path}
	haveNote := false

	for i, raw := range strings.Split(content, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			return AskPolicyFile{}, fmt.Errorf("%s:%d: table headers are not supported; a kopicode "+
				"ask-policy file has exactly one top-level key, note, and nothing else defines a key here "+
				"for a table to separate it from (docs/adr/0013-agent-controlled-resident-session-surface.md)",
				path, lineNo)
		}

		key, rest, ok := strings.Cut(line, "=")
		if !ok {
			return AskPolicyFile{}, fmt.Errorf("%s:%d: not a key = value line, a comment, or a blank line: %q",
				path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		if !askPolicyBareKey(key) {
			return AskPolicyFile{}, fmt.Errorf("%s:%d: %q is not a bare key; this reader accepts flat keys "+
				"only, the same discipline internal/harness/file.go holds .kopicode/config.toml to", path, lineNo, key)
		}
		if key != "note" {
			return AskPolicyFile{}, fmt.Errorf("%s:%d: %q is not a key this file recognizes; only note is — "+
				"a key belonging to a different concern (.kopicode/config.toml's model/harness, a policy "+
				"file's root/allow) does not belong in an ask-policy file", path, lineNo, key)
		}
		if haveNote {
			return AskPolicyFile{}, fmt.Errorf("%s:%d: %q is set twice; which one wins is not something "+
				"this reader is willing to decide for you", path, lineNo, key)
		}

		v, err := askPolicyParseString(strings.TrimSpace(rest))
		if err != nil {
			return AskPolicyFile{}, fmt.Errorf("%s:%d: note = %s: %w", path, lineNo, rest, err)
		}
		if v == "" {
			return AskPolicyFile{}, fmt.Errorf("%s:%d: note is an empty string; an ask policy that says "+
				"nothing is a mistake, not a declaration — omit the file to fall back to the fixed refusal", path, lineNo)
		}
		cfg.Note = v
		haveNote = true
	}

	if !haveNote {
		return AskPolicyFile{}, fmt.Errorf("%s: missing required key %q", path, "note")
	}
	return cfg, nil
}

// answerer turns a loaded policy into an [Answerer] that returns the note as a
// refusal. It returns an error, not an AskAnswer with text, on purpose: that is
// the exact path [Engine.runAsk] already journals as journal.AskAnswered with
// Refused true (ADR-0013 decision 6 is explicit that Refused's existing meaning
// — nobody answered this specific question — is precisely right here, and that
// only the *content* of the reason changes, from the fixed headless refusal to
// the orchestrator's own actionable instruction). Source is stamped policy by
// mustAnswerer, not here, exactly as denyHeadlessAsk relies on AskUnattended to
// do. No change to AskAnswer or journal.AskAnswered's shape.
func (f AskPolicyFile) answerer() Answerer {
	msg := askPolicyRefusalPrefix + f.Note
	return func(context.Context, AskRequest) (AskAnswer, error) {
		return AskAnswer{}, errors.New(msg)
	}
}

// askPolicyBareKey reports whether k is a bare key: letters, digits, underscore
// and dash, at least one character. Duplicated from
// internal/harness/file.go's bareKey and permission/allowlist_file.go's
// allowlistBareKey rather than shared — see [AskPolicyFile]'s doc comment for
// why.
func askPolicyBareKey(k string) bool {
	if k == "" {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// askPolicyParseString reads a TOML basic ("...") or literal ('...') string,
// allowing a trailing comment after it. Escape sequences are refused rather than
// decoded, mirroring permission/allowlist_file.go's allowlistParseString and
// internal/harness/file.go's parseString exactly — see those functions for why.
func askPolicyParseString(v string) (string, error) {
	if v == "" {
		return "", fmt.Errorf("has no value")
	}
	quote := v[0]
	if quote != '"' && quote != '\'' {
		return "", fmt.Errorf("%s is not a quoted string; write it as \"a-value\"", v)
	}
	end := strings.IndexByte(v[1:], quote)
	if end < 0 {
		return "", fmt.Errorf("%s is missing its closing %c", v, quote)
	}
	value := v[1 : 1+end]
	if quote == '"' && strings.Contains(value, `\`) {
		return "", fmt.Errorf("%q contains a backslash escape, which this reader does not decode; use a "+
			"literal string in single quotes", value)
	}

	trailer := strings.TrimSpace(v[end+2:])
	if trailer != "" && !strings.HasPrefix(trailer, "#") {
		return "", fmt.Errorf("%s has trailing text after the closing quote: %q", v, trailer)
	}
	return value, nil
}
