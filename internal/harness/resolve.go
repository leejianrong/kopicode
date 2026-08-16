package harness

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"github.com/leejianrong/kopicode/internal/provider"
)

// Flag names, shared so the two front ends cannot drift.
//
// ADR-0007 decision 2 requires both flags on both surfaces, because setting the
// arm per invocation is the whole point of one binary serving every model. Two
// independently declared flags would eventually differ in a name or a default,
// and the difference would show up as a bench arm nobody could reproduce from
// the other binary.
const (
	FlagModel   = "model"
	FlagHarness = "harness"
)

// Overrides are the per-invocation values from the command line. An empty
// string means the flag was not given, which is what lets the config file be
// consulted next.
type Overrides struct {
	Model   string
	Harness string
}

// Bind registers --model and --harness on fs and returns where their values
// will land. Call fs.Parse afterwards.
//
// The flag package accepts both -model and --model, so ADR-0007's spelling
// works as written.
func Bind(fs *flag.FlagSet) *Overrides {
	var o Overrides
	fs.StringVar(&o.Model, FlagModel, "", "model id to run this session (default "+DefaultModelID+")")
	fs.StringVar(&o.Harness, FlagHarness, "", "harness configuration name (default: whatever the model maps to)")
	return &o
}

// Source names where a resolved value came from.
//
// It is diagnostics and not session content: the journal records the *resolved*
// model, pin and hash, because those identify the arm, and which of flag, config
// or default supplied them belongs to slog (ADR-0007 §Consequences, and
// CLAUDE.md on slog never duplicating the journal).
type Source string

const (
	// SourceFlag is --model or --harness on this invocation.
	SourceFlag Source = "flag"
	// SourceFile is a key in .kopicode/config.toml.
	SourceFile Source = "config"
	// SourceDefault is the built-in default.
	SourceDefault Source = "default"
	// SourceRegistry is the harness configuration the model's registry row
	// names, which is the default for the harness axis specifically.
	SourceRegistry Source = "registry"
)

// Selection is the resolved arm: the three values ADR-0007 decision 7 says
// identify one, plus the hash the bench runner pools on.
//
// This is what the engine loop needs in order to write journal.SessionStarted —
// ModelID, Pin and HarnessConfigHash map onto its model_id, provider and
// harness_config_hash fields — and what a provider request needs in order to be
// pinned. It is a value: resolving twice from the same inputs produces the same
// Selection, and nothing here is stateful.
type Selection struct {
	// ModelID is the resolved model id, in the provider's canonical spelling.
	ModelID string
	// Pin is the routing every request in this session declares.
	Pin provider.Pin
	// Config is the resolved harness configuration.
	Config Config
	// HarnessConfigHash is Config.Hash, carried so the engine does not have to
	// recompute it per event and so two events in one session cannot disagree.
	HarnessConfigHash string
	// Verify is the forced-verification command .kopicode/config.toml named, or
	// nil when it named none and discovery answers instead (docs/SLICE-1.md §5).
	//
	// The *command* is not in the hash and the *source* is: two repositories that
	// name different commands are still the same arm, because the harness did the
	// same thing in both — it ran what the repository said. A run that discovered
	// its command is a different arm, because the harness decided rather than
	// obeyed, and [Config.Verification.Source] carries exactly that distinction
	// into the preimage.
	Verify []string

	// ConfigFilePath is the config file the resolution read, or "" when there
	// was none. Diagnostics only.
	ConfigFilePath string
	// ModelSource and HarnessSource say which rung of the precedence chain
	// supplied each value. Diagnostics only — see [Source].
	ModelSource   Source
	HarnessSource Source
}

// LogValue renders a selection for slog.
//
// Grouped and explicit rather than reflected, so a field added to [Selection]
// does not silently start appearing in a log line. There is nothing
// credential-shaped here and there is nowhere to put one: this type holds a
// model id, a pin, a configuration and two provenance labels.
func (s Selection) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("model_id", s.ModelID),
		slog.String("model_source", string(s.ModelSource)),
		slog.String("harness", s.Config.Name),
		slog.String("harness_source", string(s.HarnessSource)),
		slog.String("harness_config_hash", s.HarnessConfigHash),
		slog.String("pin", s.Pin.String()),
		slog.String("config_file", s.ConfigFilePath),
	)
}

// UsageError is a startup failure the user can fix by typing something else.
//
// It is a distinct type because it is a distinct exit code: ADR-0007 decision 4
// makes an unrecognised model id exit 2, the usage code docs/SLICE-1.md step 14
// documents, and never a provider error. A front end asks [IsUsageError] and
// picks its exit code from the answer rather than mapping error strings.
type UsageError struct{ msg string }

func (e *UsageError) Error() string { return e.msg }

func usagef(format string, args ...any) *UsageError {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}

// IsUsageError reports whether err is a startup usage error — exit 2.
func IsUsageError(err error) bool {
	var u *UsageError
	return errors.As(err, &u)
}

// Resolve works out which arm this invocation is, from the flags and the
// repository's config file.
//
// dir is where the session was started; the config file is looked for from
// there upwards (see [LoadFileConfig]).
//
// Precedence is ADR-0007 decision 2, highest first: the flag, then the config
// file, then the built-in default. There is deliberately no environment variable
// at any position — decision 3, on provenance: a flag is in the shell history
// and the CI job log and a config key shows up in a diff, while an environment
// variable is ambient and invisible in both, which is the standard mechanism by
// which "I ran the same command and got a different number" happens. The
// asymmetry with OPENROUTER_API_KEY points the other way and is also deliberate:
// a credential must live in the environment precisely because it must not be
// written to a file anyone can commit.
//
// # Ordering
//
// Every failure here happens before a session exists. Nothing in this package
// opens the journal, takes the repository lock, assembles a prompt or contacts a
// provider, so ADR-0007 decision 4's "before anything else" is a property of
// what this function *can* do rather than of where a front end happens to call
// it. TestRefusalWritesNothing and the front ends' integration test hold the
// observable half: a refused invocation leaves the working tree exactly as it
// found it.
func Resolve(dir string, o Overrides) (Selection, error) {
	file, err := LoadFileConfig(dir)
	if err != nil {
		return Selection{}, err
	}

	modelID, modelSource := pick(o.Model, file.Model, DefaultModelID, SourceDefault)

	entry, ok := Lookup(modelID)
	if !ok {
		return Selection{}, unknownModel(modelID, modelSource, file.Path)
	}

	harnessName, harnessSource := pick(o.Harness, file.Harness, entry.HarnessConfig, SourceRegistry)

	cfg, ok := ConfigByName(harnessName)
	if !ok {
		return Selection{}, unknownHarness(harnessName, harnessSource, file.Path)
	}

	// The registered configuration says the command is discovered, because that
	// is what the harness does by default. A repository that names one has
	// changed what the harness does, so it changes the configuration and
	// therefore the hash — which is the behaviour Verification.Source was
	// declared for, and the reason it is in the preimage at all.
	//
	// The hash is recomputed from the amended value rather than read off the
	// registry: a Selection whose Config and HarnessConfigHash disagreed would be
	// an arm identified by a value it is not.
	if len(file.Verify) > 0 {
		cfg.Verification.Source = VerificationConfigured
	}

	return Selection{
		ModelID:           entry.ModelID,
		Pin:               entry.Pin,
		Config:            cfg,
		HarnessConfigHash: cfg.Hash(),
		Verify:            file.Verify,
		ConfigFilePath:    file.Path,
		ModelSource:       modelSource,
		HarnessSource:     harnessSource,
	}, nil
}

// pick applies the precedence chain to one axis and reports which rung answered.
func pick(fromFlag, fromFile, fallback string, fallbackSource Source) (string, Source) {
	switch {
	case fromFlag != "":
		return fromFlag, SourceFlag
	case fromFile != "":
		return fromFile, SourceFile
	default:
		return fallback, fallbackSource
	}
}

// unknownModel is ADR-0007 decision 4's error: it names the requested id, says
// where the id came from, lists the supported set and offers a near match when
// one is honest.
//
// Where it came from is in the message because the three rungs fail in different
// places — a flag is fixed by retyping the command, a config key by editing a
// file that may be several directories up — and a message that does not say
// which one it read sends the reader looking in the wrong place.
func unknownModel(modelID string, source Source, configPath string) *UsageError {
	var b strings.Builder
	fmt.Fprintf(&b, "unknown model %q (from %s)", modelID, sourceDescription(source, FlagModel, configPath))

	if suggestion := nearest(modelID, SupportedModels()); suggestion != "" {
		fmt.Fprintf(&b, "\n\ndid you mean %q?", suggestion)
	}

	b.WriteString("\n\nsupported models:")
	for _, id := range SupportedModels() {
		fmt.Fprintf(&b, "\n  %s", id)
	}
	b.WriteString("\n\nadding a model is a registry row and a pull request " +
		"(docs/adr/0007-model-selection-and-harness-config-shape.md decision 5)")

	return &UsageError{msg: b.String()}
}

// unknownHarness is the same refusal for the harness axis. --harness can select
// any *registered* configuration and nothing else: users do not author one.
func unknownHarness(name string, source Source, configPath string) *UsageError {
	var b strings.Builder
	fmt.Fprintf(&b, "unknown harness configuration %q (from %s)", name, sourceDescription(source, FlagHarness, configPath))

	if suggestion := nearest(name, ConfigNames()); suggestion != "" {
		fmt.Fprintf(&b, "\n\ndid you mean %q?", suggestion)
	}

	b.WriteString("\n\nharness configurations are compiled in, not written by you:")
	for _, n := range ConfigNames() {
		fmt.Fprintf(&b, "\n  %s", n)
	}

	return &UsageError{msg: b.String()}
}

func sourceDescription(source Source, flagName, configPath string) string {
	switch source {
	case SourceFlag:
		return "--" + flagName
	case SourceFile:
		if configPath != "" {
			return configPath
		}
		return ConfigDirName + "/" + ConfigFileName
	case SourceDefault:
		// Reachable only if the built-in default names a model this binary does
		// not register, which is a bug in the binary and not in what the user
		// typed. Say so rather than implying they typed it.
		return "the built-in default, which is a bug in this binary"
	case SourceRegistry:
		return "this model's registry row, which is a bug in this binary"
	default:
		return string(source)
	}
}
