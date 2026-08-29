package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigDirName is the per-repository directory kopicode keeps its state in. It
// is excluded through .git/info/exclude and never through .gitignore (CLAUDE.md,
// "Never touch the user's git state").
const ConfigDirName = ".kopicode"

// ConfigFileName is the repository's configuration file, the middle rung of
// ADR-0007 decision 2's precedence chain.
const ConfigFileName = "config.toml"

// FileConfig is the part of .kopicode/config.toml this package reads.
//
// It is not the whole file. This reader passes over every key it does not own
// rather than refusing them — a config file is shared, and a reader that
// rejected a neighbour's key would make adding one a breaking change.
type FileConfig struct {
	// Path is the file the values came from, or "" when no file was found.
	// Nothing else distinguishes "absent" from "present and empty", and the
	// two want different words in a diagnostic.
	Path string
	// Model is the `model` key, or "" when unset.
	Model string
	// Harness is the `harness` key, or "" when unset.
	Harness string
	// Verify is the `verify` key: the forced-verification command this
	// repository names for itself (docs/SLICE-1.md §5). Nil when unset, which is
	// what lets internal/verify's discovery answer instead.
	//
	// It is an array of strings — `verify = ["make", "test"]` — and never a
	// shell string, because journal.VerificationRun records argv. A shell string
	// would have to be re-split on replay by a splitter that is not the shell
	// that ran it, and the two disagree the first time a path holds a space.
	Verify []string
}

// LoadFileConfig finds and reads the repository's config file, starting at dir
// and walking upwards.
//
// The walk stops at the first directory holding a .git entry, that directory
// included: a repository's configuration is the repository's, and a session run
// from a checkout nested inside another project must not silently inherit the
// outer project's model. It also stops at the filesystem root, which is what
// happens outside a repository entirely.
//
// A missing file is not an error. It is the ordinary case — most repositories
// will never have one — and it resolves to the built-in default.
func LoadFileConfig(dir string) (FileConfig, error) {
	path, data, ok, err := findUpward(dir, filepath.Join(ConfigDirName, ConfigFileName))
	if err != nil {
		return FileConfig{}, err
	}
	if !ok {
		return FileConfig{}, nil
	}
	return parseFileConfig(path, string(data))
}

// AgentsFileName is the repository's own project-instructions file
// (KAN-1024): AGENTS.md, at the repository root, read the same way
// LoadFileConfig reads .kopicode/config.toml.
const AgentsFileName = "AGENTS.md"

// AgentsFile is a repository's own AGENTS.md, found by [LoadAgentsFile].
type AgentsFile struct {
	// Path is the file that was read.
	Path string
	// Content is the file's content, verbatim.
	Content string
}

// LoadAgentsFile finds and reads the repository's AGENTS.md, starting at dir
// and walking upwards — the identical boundary [LoadFileConfig] uses (the
// first directory holding a .git entry, inclusive, or the filesystem root).
//
// ok is false when no file was found, which is the ordinary case: most
// repositories will never have one. A file that exists and cannot be read is
// not "absent" — the same rule LoadFileConfig enforces for the config file —
// so that case is a non-nil error rather than a silent skip.
func LoadAgentsFile(dir string) (AgentsFile, bool, error) {
	path, data, ok, err := findUpward(dir, AgentsFileName)
	if err != nil {
		return AgentsFile{}, false, err
	}
	if !ok {
		return AgentsFile{}, false, nil
	}
	return AgentsFile{Path: path, Content: string(data)}, true, nil
}

// findUpward looks for relPath in dir, then in each parent directory in turn,
// stopping at the first directory holding a .git entry (that directory
// included) or at the filesystem root — the boundary [LoadFileConfig] and
// [LoadAgentsFile] share, extracted here so the walk itself exists exactly
// once.
//
// ok is false when the walk reached a boundary with nothing to read. A file
// that exists and cannot be read is reported as a non-nil error rather than
// as absent, because the two callers of this function both attach that same
// meaning to "exists but unreadable": treating it as absent would silently
// fall back to a default the repository never asked for.
func findUpward(dir, relPath string) (path string, content []byte, ok bool, err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", nil, false, fmt.Errorf("harness: resolving %s: %w", dir, err)
	}

	for {
		candidate := filepath.Join(dir, relPath)
		data, readErr := os.ReadFile(candidate)
		switch {
		case readErr == nil:
			return candidate, data, true, nil
		case !os.IsNotExist(readErr):
			return "", nil, false, fmt.Errorf("harness: reading %s: %w", candidate, readErr)
		}

		// A repository boundary ends the walk, after this directory has been
		// looked in. .git is a directory in a normal checkout and a file in a
		// linked worktree, so this tests for existence rather than for a kind.
		if _, statErr := os.Lstat(filepath.Join(dir, ".git")); statErr == nil {
			return "", nil, false, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil, false, nil
		}
		dir = parent
	}
}

// parseFileConfig reads the flat top-level keys of a TOML file.
//
// # Why this is not a TOML library
//
// Dependencies stay near-zero here, and ADR-0007 decision 2 chose a flat key —
// `model = "..."` — over a `[model]` table precisely because it is the smaller
// addition. What that leaves to read is a line of the form `key = "value"`, and
// a whole TOML implementation to read it would be a dependency justified by
// syntax nobody uses.
//
// # What it accepts, and what it refuses
//
// Comments, blank lines, and top-level `key = value` pairs. A `[table]` header
// ends the scan: keys below one are not top-level keys, and reading them as
// though they were is how `model` inside `[experiment]` would silently become
// the session's model.
//
// Values are interpreted only for the keys this reader owns, and only as basic
// or literal strings. Everything else is skipped without being parsed — see
// [FileConfig].
//
// Anything it cannot make sense of is an error naming the file and the line,
// never a silent skip. That direction is deliberate: this file decides which
// model runs, and a misread key means a session attributed to the wrong arm.
// Refusing to start says so; guessing does not.
func parseFileConfig(path, content string) (FileConfig, error) {
	cfg := FileConfig{Path: path}
	seen := map[string]bool{}

	for i, raw := range strings.Split(content, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			// A table header. Everything after it belongs to that table.
			return cfg, nil
		}

		key, rest, ok := strings.Cut(line, "=")
		if !ok {
			return FileConfig{}, usagef("%s:%d: not a key = value line, a comment, or a table header: %q",
				path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		if !bareKey(key) {
			return FileConfig{}, usagef("%s:%d: %q is not a bare key; kopicode reads flat keys only "+
				"(docs/adr/0007-model-selection-and-harness-config-shape.md decision 2)",
				path, lineNo, key)
		}
		if seen[key] {
			return FileConfig{}, usagef("%s:%d: %q is set twice; which one wins is not something this "+
				"reader is willing to decide for you", path, lineNo, key)
		}
		seen[key] = true

		if key == "verify" {
			argv, err := parseStringArray(strings.TrimSpace(rest))
			if err != nil {
				return FileConfig{}, usagef("%s:%d: %s = %s", path, lineNo, key, err)
			}
			cfg.Verify = argv
			continue
		}

		target := map[string]*string{"model": &cfg.Model, "harness": &cfg.Harness}[key]
		if target == nil {
			// A key belonging to somebody else. Its value is not this reader's
			// business and is not parsed.
			continue
		}

		value, err := parseString(strings.TrimSpace(rest))
		if err != nil {
			return FileConfig{}, usagef("%s:%d: %s = %s", path, lineNo, key, err)
		}
		*target = value
	}

	return cfg, nil
}

// bareKey reports whether k is a TOML bare key: letters, digits, underscore and
// dash, at least one character. Quoted and dotted keys are refused rather than
// interpreted, because a reader that half-understands them is worse than one
// that says it does not.
func bareKey(k string) bool {
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

// parseString reads a TOML basic ("...") or literal ('...') string, allowing a
// trailing comment after it.
//
// Escape sequences are refused rather than decoded. A model id has no use for
// one, and a decoder that handled \n but not \u would be a subset nobody
// documented.
func parseString(v string) (string, error) {
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
		return "", fmt.Errorf("%q contains a backslash escape, which kopicode's reader does not decode; "+
			"use a literal string in single quotes", value)
	}

	trailer := strings.TrimSpace(v[end+2:])
	if trailer != "" && !strings.HasPrefix(trailer, "#") {
		return "", fmt.Errorf("%s has trailing text after the closing quote: %q", v, trailer)
	}
	return value, nil
}

// parseStringArray reads a single-line TOML array of strings, allowing a
// trailing comma and a trailing comment.
//
// # Why an array and not a command line
//
// journal.VerificationRun records argv, deliberately: a replayed session runs
// exactly what the original ran, and there is no shell in the middle to agree
// with. A `verify = "make test"` accepted here would have to be split by
// something, and whatever that something is, it is not the shell the user had in
// mind the first time a directory name has a space in it. So a bare string is
// refused with the array spelled out, rather than split on whitespace and hoped
// over.
//
// # What it refuses, and why each refusal is better than a guess
//
//   - A bare string, per above.
//   - An empty array. It looks like "switch verification off" and is not: the
//     forced-verification policy is a harness configuration field and is in the
//     hash (ADR-0007 decision 6), so turning it off here would move an arm's
//     behaviour outside the value that identifies the arm.
//   - An array that does not close on this line. TOML permits a multi-line
//     array; this reader is line-oriented and says so rather than reading half
//     of one.
func parseStringArray(v string) ([]string, error) {
	if v == "" {
		return nil, fmt.Errorf("has no value")
	}
	if v[0] != '[' {
		return nil, fmt.Errorf("%s is not an array; write the command as argv, "+
			"for example [\"make\", \"test\"] — kopicode records and replays argv and will not "+
			"split a command line for you", v)
	}
	end := closingBracket(v)
	if end < 0 {
		return nil, fmt.Errorf("%s does not close on this line; kopicode's reader is line-oriented "+
			"and does not read a multi-line array", v)
	}
	if trailer := strings.TrimSpace(v[end+1:]); trailer != "" && !strings.HasPrefix(trailer, "#") {
		return nil, fmt.Errorf("%s has trailing text after the closing bracket: %q", v, trailer)
	}

	var argv []string
	for _, field := range splitArrayElements(v[1:end]) {
		element, err := parseString(field)
		if err != nil {
			return nil, fmt.Errorf("element %d %w", len(argv)+1, err)
		}
		argv = append(argv, element)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s is empty; an empty command is not a way to switch verification "+
			"off — that is a harness configuration field and is in the arm's hash "+
			"(docs/adr/0007-model-selection-and-harness-config-shape.md decision 6)", v)
	}
	return argv, nil
}

// closingBracket is the index of the `]` that closes the array at v[0], or -1
// when the line does not close it.
//
// It skips brackets inside quoted strings, so `["go", "test", "-run", "A]B"]`
// closes where it looks like it closes, and it stops at the first one outside a
// string, so a trailing `# ]` in a comment cannot move it.
func closingBracket(v string) int {
	var quote byte
	for i := 1; i < len(v); i++ {
		c := v[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ']':
			return i
		}
	}
	return -1
}

// splitArrayElements splits an array body on the commas that are not inside a
// quoted string, dropping a trailing empty element so that a trailing comma is
// allowed as TOML allows it.
//
// Splitting on every comma would break ["go", "test", "-run", "A,B"], which is
// an ordinary thing to want to verify with.
func splitArrayElements(body string) []string {
	var out []string
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
		case c == ',':
			out = append(out, strings.TrimSpace(body[start:i]))
			start = i + 1
		}
	}
	out = append(out, strings.TrimSpace(body[start:]))

	// A trailing comma leaves one empty tail; anything else empty is a hole in
	// the middle and is left in place so parseString reports it.
	if n := len(out); n > 0 && out[n-1] == "" {
		out = out[:n-1]
	}
	return out
}
