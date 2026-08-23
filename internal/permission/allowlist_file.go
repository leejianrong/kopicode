package permission

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AllowlistFile is the declared configuration [AllowlistPolicy] is built from
// (ADR-0011 decision 1): what an orchestrator with nobody at a terminal
// states, up front, that a kopicode invocation may do.
//
// # Why this file, and not a key in .kopicode/config.toml
//
// ADR-0007 decision 2 already owns `.kopicode/config.toml`'s flat-key
// grammar, and its keys — `model`, `harness`, `verify` — all answer the same
// question: which arm is this session. A declared allowlist answers a
// different one — what may this *particular unattended invocation* do — and
// the two do not share a lifecycle. `.kopicode/config.toml` is a repository's
// standing configuration, checked in or not, read by any session started
// there; a policy file is authored per invocation (or per orchestrator) and
// named explicitly with a flag, the same way `--model` overrides the
// repository's own choice rather than living inside it. Folding the two
// together would make an unrelated edit to one silently visible to the other
// (docs/adr/0009-ask-tool-contract.md already declined the analogous merge
// for `ask`, for the same reason: two mechanisms that only coincidentally
// look similar).
//
// # Why this is not a TOML library, again
//
// internal/harness/file.go's own doc comment already makes this repository's
// case for a hand-rolled flat-key reader over a dependency: two dependencies
// exist in go.mod today and a whole TOML implementation is not warranted by
// the two keys below. This reader is that same argument applied to a second,
// unrelated file, and it deliberately duplicates file.go's small parsing
// primitives (a quoted string, a single-line array, a bare key) rather than
// exporting them from internal/harness: internal/permission does not import
// internal/harness anywhere else, and a shared "TOML fragment" package
// between two otherwise-independent leaves would be exactly the kind of
// coupling ADR-0003 keeps this codebase from growing.
//
// # The grammar
//
//	root = "/abs/path/to/the/repository"
//	allow = [["go", "test", "./..."], ["npm", "test"]]
//
// Two required, flat, top-level keys and nothing else — no `[table]`, no
// nesting beyond `allow`'s one declared level. `root` is a quoted string and
// must be absolute: this file is handed to a session whose own working
// directory an orchestrator already controls, and a relative root would
// silently mean two different directories depending on where the caller
// happened to invoke kopicode from, which is exactly the kind of ambiguity a
// declared, orchestrator-authored file exists to remove. `allow` is an array
// of argv arrays, in [internal/harness/file.go]'s `verify` key's own spelling
// and for the identical reason: journal.PermissionDecided records argv, so a
// shell string accepted here would need splitting by something that is not
// the shell that eventually runs it, and the two disagree the moment a path
// holds a space.
//
// Unlike `verify`, an empty `allow = []` is accepted rather than refused: see
// [NewAllowlist]'s own doc comment for why "no shell command is ever
// approved" is a legitimate declaration here, not a way to switch a required
// gate off.
//
// # What it refuses, and why each refusal is better than a guess
//
//   - A `[table]` header. internal/harness's own reader treats one as "stop
//     reading top-level keys, they belong to somebody else" and returns what
//     it has so far without error, because .kopicode/config.toml is shared
//     with keys this reader does not own. A policy file has no such
//     neighbour — root and allow are the whole of it — so a table header
//     here is almost certainly a mistake (an accidental TOML table wrapping
//     what the author meant as top-level keys), and silently truncating the
//     scan would turn that mistake into a confusing "root is required" a
//     line further down rather than pointing at the line that caused it.
//   - Any key besides `root` and `allow`. The same reasoning: nothing else
//     defines a key in this file, so an unrecognised one is a typo
//     (`alow = [...]`) worth naming rather than a neighbour's business to
//     step around.
//   - A key set twice, a value that is not a quoted string or a single-line
//     array, an `allow` element that is not itself an argv array, an empty
//     argv element (a command needs at least argv[0]), and a `root` that is
//     empty or not absolute.
//   - Either key missing entirely. A declared allowlist with no declared root
//     has nothing to confine writes to, and a caller who forgot `allow`
//     meant to write `allow = []` and say so, not to have this reader guess
//     that was the intent.
type AllowlistFile struct {
	// Path is the file the values came from.
	Path string
	// Root is the `root` key: the resolved-through-a-Resolver confinement
	// scope [NewAllowlist] builds [AllowlistPolicy] with. Always absolute —
	// LoadAllowlistFile refuses a relative one, see above.
	Root string
	// Allow is the `allow` key: the closed, exact-match set of permitted
	// shell argv. Never nil after a successful load — an empty declared
	// allowlist is [][]string{}, not the zero value, so a caller can tell
	// "no file was loaded" (the zero [AllowlistFile]) from "a file was
	// loaded that permits no shell command at all."
	Allow [][]string
}

// LoadAllowlistFile reads and parses the declared-allowlist file at path.
//
// Unlike [github.com/leejianrong/kopicode/internal/harness.LoadFileConfig], a
// missing file here is an error, not "use the built-in default": there is no
// safe default this file could compile to (defaulting to "no shell, no
// writes anywhere" would be indistinguishable from a typo in the flag that
// named this path), and a caller that passes --policy-file has already said
// it wants exactly this file. Every error this function returns is the
// caller's own mistake to fix — a missing file, a bad path, a malformed line
// — never a harness or provider failure, which is why cmd/kopicode maps every
// one of them to the usage exit code rather than the harness one.
func LoadAllowlistFile(path string) (AllowlistFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return AllowlistFile{}, fmt.Errorf("permission: reading %s: %w", path, err)
	}
	return parseAllowlistFile(path, string(data))
}

// parseAllowlistFile reads the flat top-level keys of a declared-allowlist
// file. See [AllowlistFile] for the grammar and what is refused.
func parseAllowlistFile(path, content string) (AllowlistFile, error) {
	cfg := AllowlistFile{Path: path}
	seen := map[string]bool{}
	haveRoot, haveAllow := false, false

	for i, raw := range strings.Split(content, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			return AllowlistFile{}, fmt.Errorf("%s:%d: table headers are not supported; a kopicode "+
				"declared-allowlist file has exactly two top-level keys, root and allow, and nothing else "+
				"defines a key here for a table to separate it from (docs/adr/0011-unattended-invocation-policy-gate.md)",
				path, lineNo)
		}

		key, rest, ok := strings.Cut(line, "=")
		if !ok {
			return AllowlistFile{}, fmt.Errorf("%s:%d: not a key = value line, a comment, or a blank line: %q",
				path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		if !allowlistBareKey(key) {
			return AllowlistFile{}, fmt.Errorf("%s:%d: %q is not a bare key; this reader accepts flat keys "+
				"only, the same discipline internal/harness/file.go holds .kopicode/config.toml to", path, lineNo, key)
		}
		if seen[key] {
			return AllowlistFile{}, fmt.Errorf("%s:%d: %q is set twice; which one wins is not something "+
				"this reader is willing to decide for you", path, lineNo, key)
		}
		seen[key] = true

		switch key {
		case "root":
			v, err := allowlistParseString(strings.TrimSpace(rest))
			if err != nil {
				return AllowlistFile{}, fmt.Errorf("%s:%d: root = %s: %w", path, lineNo, rest, err)
			}
			if v == "" {
				return AllowlistFile{}, fmt.Errorf("%s:%d: root is an empty string; it has to name the "+
					"directory writes are confined to", path, lineNo)
			}
			if !filepath.IsAbs(v) {
				return AllowlistFile{}, fmt.Errorf("%s:%d: root %q is not an absolute path; a relative root "+
					"would mean a different directory depending on where kopicode happened to be invoked "+
					"from, which a declared, orchestrator-authored file must not leave ambiguous", path, lineNo, v)
			}
			cfg.Root = v
			haveRoot = true

		case "allow":
			argv, err := allowlistParseArgvArray(strings.TrimSpace(rest))
			if err != nil {
				return AllowlistFile{}, fmt.Errorf("%s:%d: allow = %s: %w", path, lineNo, rest, err)
			}
			cfg.Allow = argv
			haveAllow = true

		default:
			return AllowlistFile{}, fmt.Errorf("%s:%d: %q is not a key this file recognizes; only root and "+
				"allow are — a key belonging to a different concern (.kopicode/config.toml's model/harness/"+
				"verify, say) does not belong in a declared-allowlist file", path, lineNo, key)
		}
	}

	if !haveRoot {
		return AllowlistFile{}, fmt.Errorf("%s: missing required key %q", path, "root")
	}
	if !haveAllow {
		return AllowlistFile{}, fmt.Errorf("%s: missing required key %q; write %s if no shell command should "+
			"ever be approved", path, "allow", `allow = []`)
	}
	if cfg.Allow == nil {
		cfg.Allow = [][]string{}
	}
	return cfg, nil
}

// allowlistBareKey reports whether k is a bare key: letters, digits,
// underscore and dash, at least one character. Identical to
// internal/harness/file.go's bareKey, duplicated rather than shared — see
// [AllowlistFile]'s doc comment for why.
func allowlistBareKey(k string) bool {
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

// allowlistParseString reads a TOML basic ("...") or literal ('...') string,
// allowing a trailing comment after it. Escape sequences are refused rather
// than decoded, mirroring internal/harness/file.go's parseString exactly —
// see that function's doc comment for why.
func allowlistParseString(v string) (string, error) {
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

// allowlistParseArgvArray reads the single-line `allow = [[...], [...]]`
// value: an array of argv arrays.
//
// It is line-oriented, like internal/harness/file.go's parseStringArray:
// TOML permits a multi-line array, and this reader refuses to read half of
// one rather than guess where it ends.
func allowlistParseArgvArray(v string) ([][]string, error) {
	if v == "" {
		return nil, fmt.Errorf("has no value")
	}
	if v[0] != '[' {
		return nil, fmt.Errorf("%s is not an array; write allow as a list of argv lists, for example "+
			`[["go", "test"], ["npm", "test"]] — kopicode records and replays argv and will not split a `+
			"command line for you", v)
	}
	end := matchingBracket(v)
	if end < 0 {
		return nil, fmt.Errorf("%s does not close on this line; this reader is line-oriented and does not "+
			"read a multi-line array", v)
	}
	if trailer := strings.TrimSpace(v[end+1:]); trailer != "" && !strings.HasPrefix(trailer, "#") {
		return nil, fmt.Errorf("%s has trailing text after the closing bracket: %q", v, trailer)
	}

	body := strings.TrimSpace(v[1:end])
	if body == "" {
		// An explicitly empty declared allowlist — see NewAllowlist's doc
		// comment for why this is accepted rather than refused the way
		// harness's `verify = []` is.
		return [][]string{}, nil
	}

	var out [][]string
	for _, element := range splitTopLevel(body) {
		element = strings.TrimSpace(element)
		if element == "" {
			return nil, fmt.Errorf("allow has an empty element between commas")
		}
		if element[0] != '[' {
			return nil, fmt.Errorf("element %d (%s) is not an argv array; each entry in allow must itself "+
				`be a list, for example ["go", "test"]`, len(out)+1, element)
		}
		innerEnd := matchingBracket(element)
		if innerEnd < 0 || strings.TrimSpace(element[innerEnd+1:]) != "" {
			return nil, fmt.Errorf("element %d (%s) does not close cleanly as its own argv array", len(out)+1, element)
		}
		argv, err := allowlistParseArgvBody(element[1:innerEnd])
		if err != nil {
			return nil, fmt.Errorf("element %d (%s): %w", len(out)+1, element, err)
		}
		if len(argv) == 0 {
			return nil, fmt.Errorf("element %d (%s) is an empty argv; an allowed command needs at least argv[0]",
				len(out)+1, element)
		}
		out = append(out, argv)
	}
	return out, nil
}

// allowlistParseArgvBody parses the comma-separated quoted strings inside one
// argv array's brackets — the same job internal/harness/file.go's
// splitArrayElements plus parseString do together for `verify`.
func allowlistParseArgvBody(body string) ([]string, error) {
	var argv []string
	for _, field := range splitTopLevel(body) {
		element, err := allowlistParseString(strings.TrimSpace(field))
		if err != nil {
			return nil, fmt.Errorf("element %d %w", len(argv)+1, err)
		}
		argv = append(argv, element)
	}
	return argv, nil
}

// matchingBracket is the index of the ']' that closes the '[' at v[0], or -1
// when the line does not close it.
//
// Unlike internal/harness/file.go's closingBracket, this tracks bracket
// depth rather than stopping at the first unquoted ']': allow's declared
// shape nests one array inside another (the outer list of commands, each
// holding its own argv), and the outer close is the one after every inner
// array has closed, not the first one encountered.
func matchingBracket(v string) int {
	depth := 0
	var quote byte
	for i := range len(v) {
		c := v[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// splitTopLevel splits body on the commas that are not inside a quoted
// string or a nested bracket pair, dropping a trailing empty element so a
// trailing comma is allowed. It is internal/harness/file.go's
// splitArrayElements, extended to also respect bracket nesting so a
// comma inside an inner argv array (["go", "test", "-run", "A,B"]) does not
// split the outer list in the wrong place.
func splitTopLevel(body string) []string {
	var out []string
	depth := 0
	var quote byte
	start := 0
	for i := range len(body) {
		c := body[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(body[start:i]))
			start = i + 1
		}
	}
	tail := strings.TrimSpace(body[start:])
	if tail != "" {
		out = append(out, tail)
	}
	return out
}
