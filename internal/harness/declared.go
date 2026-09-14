package harness

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// A declared harness configuration is ADR-0010's second, non-built-in class: a
// TOML file that names a built-in configuration as its base and overrides a few
// fields, resolved into one fully concrete [Config] before it is used for
// anything. It exists so an installed-binary user can tune the harness — raise
// the turn cap, widen the token budget — without editing Go source and
// rebuilding, which is exactly the case ADR-0007's compiled-in registry cannot
// serve (a private fine-tune, a niche model nobody hand-tuned).
//
// ADR-0010 decision 3 is the discipline that keeps this from reopening
// ADR-0007's reproducibility argument: a declared configuration is local-only
// and must never anchor a published, citable benchmark number. This package
// enforces the half of that which can be enforced mechanically — the resolved
// [Config.Name] is prefixed with [DeclaredConfigNamePrefix], and because the
// name is in the hash preimage (hash.go), a declared configuration can never
// share a hash with — and therefore never pool with — a built-in, even one
// declared with no overrides at all.
//
// # Why this reader is strict where file.go's is lenient
//
// [parseFileConfig] passes over any key it does not own, because
// .kopicode/config.toml is a shared file and refusing a neighbour's key would
// make adding one a breaking change. A declared-config file is the opposite: it
// is kopicode's own file, with no neighbour to tolerate, and the whole reason
// this feature exists is that a silently-ignored `max_turns` in config.toml
// gave a user no way to raise the cap and no error explaining why. So an
// unknown key here is a usage error naming the file and line, not a skip.

// DeclaredConfigNamePrefix marks a resolved declared configuration's
// [Config.Name], e.g. "declared:default".
//
// It is not decoration. [Config.Name] is in the hash preimage, so the prefix
// guarantees a declared configuration's hash differs from its base built-in's
// even when every other field is identical — which is ADR-0010 decision 3's
// "a declared config cannot pool with a built-in" turned from prose into a
// mechanical property of the hash. A built-in name never contains a colon, and
// [ConfigByName] never resolves a prefixed name, so the two name spaces cannot
// collide.
const DeclaredConfigNamePrefix = "declared:"

// declaredConfig is a parsed declared-config file before it is resolved against
// its base. A nil override pointer means the key was absent, which is what lets
// an omitted field keep the base's value rather than being reset to a zero.
type declaredConfig struct {
	path         string
	base         string
	maxTurns     *int
	tokenBudget  *int
	repairBudget *int
	maxTokens    *int
}

// LoadDeclaredConfig reads the declared-config file at path and resolves it into
// a fully concrete [Config]: the named built-in base with the file's overrides
// applied, its name prefixed with [DeclaredConfigNamePrefix].
//
// Every failure is a [UsageError] (exit 2, the same posture as the rest of the
// selection chain), so a front end turns a typo in a tuning file into a startup
// refusal that names the file and line rather than a session attributed to the
// wrong arm. A file that exists but cannot be read is a usage error too, not a
// silent fall-back to a default the user did not ask for.
func LoadDeclaredConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, usagef("harness: reading declared config %s: %v", path, err)
	}
	dc, err := parseDeclaredConfig(path, string(data))
	if err != nil {
		return Config{}, err
	}
	return resolveDeclared(dc)
}

// parseDeclaredConfig reads the flat top-level keys of a declared-config file.
//
// It shares file.go's line grammar — comments, blank lines, `key = value`
// pairs, and bare keys only — but not its tolerance: a key this reader does not
// own is an error, and so is a [table] header, because a declared-config file
// has no other owner whose keys should survive.
func parseDeclaredConfig(path, content string) (declaredConfig, error) {
	dc := declaredConfig{path: path}
	seen := map[string]bool{}

	for i, raw := range strings.Split(content, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			return declaredConfig{}, usagef("%s:%d: a declared harness config is flat `key = value` "+
				"lines only; a %q table header is not understood", path, lineNo, line)
		}

		key, rest, ok := strings.Cut(line, "=")
		if !ok {
			return declaredConfig{}, usagef("%s:%d: not a key = value line, a comment, or blank: %q",
				path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		if !bareKey(key) {
			return declaredConfig{}, usagef("%s:%d: %q is not a bare key; a declared harness config "+
				"reads flat keys only", path, lineNo, key)
		}
		if seen[key] {
			return declaredConfig{}, usagef("%s:%d: %q is set twice; which one wins is not something "+
				"this reader is willing to decide for you", path, lineNo, key)
		}
		seen[key] = true

		rest = strings.TrimSpace(rest)
		if key == "base" {
			value, err := parseString(rest)
			if err != nil {
				return declaredConfig{}, usagef("%s:%d: base = %s", path, lineNo, err)
			}
			dc.base = value
			continue
		}

		target, ok := map[string]**int{
			"max_turns":     &dc.maxTurns,
			"token_budget":  &dc.tokenBudget,
			"repair_budget": &dc.repairBudget,
			"max_tokens":    &dc.maxTokens,
		}[key]
		if !ok {
			return declaredConfig{}, usagef("%s:%d: %q is not a field a declared harness config can "+
				"override; the tunable keys are base, max_turns, token_budget, repair_budget, max_tokens",
				path, lineNo, key)
		}

		n, err := parseInt(rest)
		if err != nil {
			return declaredConfig{}, usagef("%s:%d: %s = %s", path, lineNo, key, err)
		}
		*target = &n
	}

	return dc, nil
}

// resolveDeclared applies a parsed declared config onto its named base and
// returns the fully concrete [Config].
//
// The base must be an existing built-in configuration (ADR-0010 decision 1: a
// declared config is authored by naming a built-in as its base). Bounds are
// validated here, against the resolved value, so a bad override is a startup
// usage error rather than the engine's later ErrConfig — the limits are exactly
// the engine's own (internal/engine/engine.go): MaxTurns must be positive,
// TokenBudget may be zero to mean unbounded, RepairBudget may be zero for the
// no-repair arm.
func resolveDeclared(dc declaredConfig) (Config, error) {
	if dc.base == "" {
		return Config{}, usagef("%s: a declared harness config must name a `base` built-in "+
			"configuration; the built-in configurations are: %s", dc.path, strings.Join(ConfigNames(), ", "))
	}

	base, ok := ConfigByName(dc.base)
	if !ok {
		var b strings.Builder
		fmt.Fprintf(&b, "%s: unknown base harness configuration %q", dc.path, dc.base)
		if suggestion := nearest(dc.base, ConfigNames()); suggestion != "" {
			fmt.Fprintf(&b, "\n\ndid you mean %q?", suggestion)
		}
		b.WriteString("\n\na declared config's base must be a built-in configuration:")
		for _, n := range ConfigNames() {
			fmt.Fprintf(&b, "\n  %s", n)
		}
		return Config{}, &UsageError{msg: b.String()}
	}

	cfg := base
	cfg.Name = DeclaredConfigNamePrefix + dc.base
	if dc.maxTurns != nil {
		cfg.MaxTurns = *dc.maxTurns
	}
	if dc.tokenBudget != nil {
		cfg.TokenBudget = *dc.tokenBudget
	}
	if dc.repairBudget != nil {
		cfg.RepairBudget = *dc.repairBudget
	}
	if dc.maxTokens != nil {
		cfg.Sampling.MaxTokens = *dc.maxTokens
	}

	// Validate the resolved value against the engine's own bounds. A built-in
	// base is always valid, so a failure here can only come from an override —
	// the message names the field so the fix is obvious.
	switch {
	case cfg.MaxTurns <= 0:
		return Config{}, usagef("%s: max_turns resolves to %d; it must be greater than 0 "+
			"(an unbounded loop is the failure the cap exists to prevent)", dc.path, cfg.MaxTurns)
	case cfg.TokenBudget < 0:
		return Config{}, usagef("%s: token_budget resolves to %d; it must be 0 (unbounded) or greater",
			dc.path, cfg.TokenBudget)
	case cfg.RepairBudget < 0:
		return Config{}, usagef("%s: repair_budget resolves to %d; it must be 0 (no repair) or greater",
			dc.path, cfg.RepairBudget)
	case cfg.Sampling.MaxTokens <= 0:
		return Config{}, usagef("%s: max_tokens resolves to %d; it must be greater than 0 "+
			"(it caps one reply)", dc.path, cfg.Sampling.MaxTokens)
	}

	return cfg, nil
}

// parseInt reads a TOML integer, allowing digit-group underscores (1_000_000),
// a leading sign, and a trailing comment.
//
// It is deliberately a little more lenient than TOML — it strips every
// underscore rather than only those between digits — because the direction of
// that leniency is harmless (it accepts a few malformed numbers) while the
// alternative, a stricter parser, is more code guarding inputs a human is
// unlikely to type. A quoted value fails here rather than being unquoted,
// because these keys are integers and a quoted one is a category mistake worth
// naming.
func parseInt(v string) (int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("has no value")
	}
	// An integer carries no quoted string, so the first '#' begins a comment.
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	if v == "" {
		return 0, fmt.Errorf("has no value")
	}

	n, err := strconv.Atoi(strings.ReplaceAll(v, "_", ""))
	if err != nil {
		return 0, fmt.Errorf("%q is not an integer", v)
	}
	return n, nil
}
