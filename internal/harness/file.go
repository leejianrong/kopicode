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
// It is not the whole file. docs/SLICE-1.md §5 puts the verification command in
// here too, and this reader passes over every key it does not own rather than
// refusing them — a config file is shared, and a reader that rejected a
// neighbour's key would make adding one a breaking change.
type FileConfig struct {
	// Path is the file the values came from, or "" when no file was found.
	// Nothing else distinguishes "absent" from "present and empty", and the
	// two want different words in a diagnostic.
	Path string
	// Model is the `model` key, or "" when unset.
	Model string
	// Harness is the `harness` key, or "" when unset.
	Harness string
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
	dir, err := filepath.Abs(dir)
	if err != nil {
		return FileConfig{}, fmt.Errorf("harness: resolving %s: %w", dir, err)
	}

	for {
		path := filepath.Join(dir, ConfigDirName, ConfigFileName)
		data, readErr := os.ReadFile(path)
		switch {
		case readErr == nil:
			cfg, parseErr := parseFileConfig(path, string(data))
			if parseErr != nil {
				return FileConfig{}, parseErr
			}
			return cfg, nil
		case !os.IsNotExist(readErr):
			// A file that exists and cannot be read is not "absent". Reporting
			// it as absent would silently run the default model against a
			// repository that asked for another one.
			return FileConfig{}, fmt.Errorf("harness: reading %s: %w", path, readErr)
		}

		// A repository boundary ends the walk, after this directory has been
		// looked in. .git is a directory in a normal checkout and a file in a
		// linked worktree, so this tests for existence rather than for a kind.
		if _, statErr := os.Lstat(filepath.Join(dir, ".git")); statErr == nil {
			return FileConfig{}, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return FileConfig{}, nil
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
