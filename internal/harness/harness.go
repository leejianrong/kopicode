// Package harness holds what a harness configuration is, which model gets
// which one, and how a run resolves the arm it is about to be.
//
// It is the implementation of
// [docs/adr/0007-model-selection-and-harness-config-shape.md]: one binary
// serves every supported model, the model is chosen at run time from a flag or
// the repository's config file, and an unrecognised id is a startup usage error
// rather than a provider error on the first request.
//
// # What this package produces, and what it does not
//
// It returns a [Selection]: the resolved model id, the provider pin, the
// harness configuration and its hash. The engine loop writes those onto
// journal.SessionStarted. Nothing here imports internal/journal — packages
// return data, the engine journals (CLAUDE.md, "Boundaries that must not be
// crossed") — and nothing here opens a file for writing, which is what lets
// ADR-0007 decision 4 hold: resolution happens before the journal exists, so a
// session that never started leaves no record behind.
//
// # Why a registry rather than a config file
//
// A harness configuration is a named, versioned value compiled into the binary
// (ADR-0007 decision 5). Users do not author one. The registry maps a model id
// to the configuration it gets by default and to that model's default provider
// pin, so adding a model is a row and a pull request rather than a silent
// default — the row is what forces "which harness configuration does this model
// get?" to be answered by a person.
//
// Slice 1 registers exactly one configuration and one model. That is the scope
// limit; the shape is not.
//
// # The hash
//
// [Config.Hash] is the comparison key for ADR-0005's paired method: two results
// may be pooled only when their hashes agree. A hash is worth exactly as much as
// its preimage is honest, so the preimage is written out field by field in
// hash.go, a test varies every field of [Config] by reflection and requires the
// hash to move, and [Config] may not contain a map — Go randomises map iteration
// order, and a hash that changes per process makes every A/B comparison silently
// fail to match.
package harness

import (
	"fmt"

	"github.com/leejianrong/kopicode/internal/provider"
)

// VerificationSource says where the verification command came from.
//
// It is in the hash preimage (ADR-0007 decision 6) because a run whose
// verification command was discovered from a Makefile is not the same arm as one
// where the repository named it: the two can run different commands on the same
// tree, and the difference belongs to the harness rather than to the model.
type VerificationSource string

const (
	// VerificationConfigured means .kopicode/config.toml named the command.
	VerificationConfigured VerificationSource = "configured"
	// VerificationDiscovered means the harness looked for one (a Makefile
	// target, `go test ./...`, `npm test`, `uv run pytest`) — docs/SLICE-1.md §5.
	VerificationDiscovered VerificationSource = "discovered"
)

// Verification is the verification policy half of a harness configuration.
type Verification struct {
	// Forced is whether a turn that modified files must run verification
	// before the engine may report success (docs/SLICE-1.md §5).
	Forced bool
	// Source is whether the command is configured or discovered.
	Source VerificationSource
}

// SeedPolicy says how the harness treats the provider's seed parameter.
//
// The *policy* is a harness property and is hashed; a per-run seed value drawn
// from the session's RNG would not be, because hashing it would give every run
// its own arm (ADR-0007 decision 6). [SeedFixed] is the case where the two
// coincide: the value is a constant of the configuration, so it is part of the
// configuration and is hashed with it.
type SeedPolicy string

const (
	// SeedUnseeded sends no seed.
	SeedUnseeded SeedPolicy = "unseeded"
	// SeedFixed sends [Sampling.Seed] on every request.
	SeedFixed SeedPolicy = "fixed"
)

// Sampling is the decoding configuration an arm requests.
//
// It lives inside the harness configuration rather than beside it because
// choosing temperature 0.2 for one model and 0.7 for another *is* per-model
// tuning, and a fourth axis that never varies independently of the third buys
// nothing (ADR-0007 decision 6). provider.Sampling is the per-request receipt of
// the same thing; [Config.RequestSampling] maps this onto it.
type Sampling struct {
	Temperature float64
	TopP        float64
	// MaxTokens caps one reply, not the session. The session's bound is
	// Config.TokenBudget.
	MaxTokens int
	// SeedPolicy says whether Seed is sent at all.
	SeedPolicy SeedPolicy
	// Seed is the fixed seed, meaningful only when SeedPolicy is SeedFixed.
	Seed int64
}

// Config is one harness configuration: a named, versioned value compiled into
// the binary (ADR-0007 decision 5).
//
// Every field is in the hash preimage. Adding one without adding it to
// [Config.Hash] fails TestHashCoversEveryConfigField, which walks this struct by
// reflection rather than against a hand-written list — a field silently outside
// the preimage is the failure that makes the hash unfalsifiable, and it is
// invisible in review.
//
// No field may be a map, and TestConfigHoldsNoMap enforces it. Go randomises map
// iteration order, so a preimage that ranged one would hash differently in every
// process and no two results would ever pool.
type Config struct {
	// Name is how --harness selects this configuration, and it is in the
	// preimage: two configurations that happen to hold identical values are
	// still two configurations, and a report naming one of them should not be
	// able to pool the other's results.
	Name string

	// Version is bumped when the meaning of a field changes rather than its
	// value. The hash would move anyway; the version is what a human reads.
	Version int

	// SystemPrompt is the prompt as sent, hashed by digest (ADR-0007 decision
	// 6). It is empty in slice 1 because there is no system prompt in this
	// repository yet — KAN-843 writes it — and it is a field rather than an
	// omission so that landing one moves the hash, which is correct: a prompt
	// change is a harness change.
	SystemPrompt string

	// ToolSet is the tools presented to the model, in the order they are
	// presented.
	//
	// ADR-0007 decision 6 puts "names, schemas, descriptions" in the preimage.
	// Only the names exist today: there is no tool catalogue rendered for the
	// wire anywhere in this repository (provider.Request carries none, and
	// KAN-776 owns that shape), and hashing an invented rendering would be a
	// preimage that describes nothing. So this is names only, and it is
	// deliberately narrower than the ADR asks for — see the note in
	// hash.go. Whoever lands the catalogue extends this field.
	ToolSet []string

	// ParseRoutes is the tool-call extraction order, in parse.Route's wire
	// spelling, first success wins (docs/SLICE-1.md §3).
	ParseRoutes []string

	// RepairBudget is how many repair round trips a malformed call gets.
	RepairBudget int

	// MaxTurns is the loop's turn cap. ADR-0005 §6 caps a corpus task at 20.
	MaxTurns int

	// TokenBudget is the whole session's prompt+completion allowance, the
	// second half of the bounded loop (docs/SLICE-1.md build step 9).
	TokenBudget int

	// Verification is the forced-verification policy.
	Verification Verification

	// Sampling is the decoding configuration.
	Sampling Sampling
}

// RequestSampling maps the configuration's sampling onto the wire form.
//
// The seed is sent only under [SeedFixed]. A caller cannot supply a per-run seed
// through this method on purpose: a seed drawn per run is not part of the arm,
// and a harness that let one in here would have to decide whether to hash it.
func (c Config) RequestSampling() provider.Sampling {
	s := provider.Sampling{
		Temperature: c.Sampling.Temperature,
		TopP:        c.Sampling.TopP,
		MaxTokens:   c.Sampling.MaxTokens,
	}
	if c.Sampling.SeedPolicy == SeedFixed {
		seed := c.Sampling.Seed
		s.Seed = &seed
	}
	return s
}

// String names the configuration and its version, for a diagnostic line.
func (c Config) String() string { return fmt.Sprintf("%s/v%d", c.Name, c.Version) }

// DefaultConfigName is the one configuration slice 1 registers.
//
// A flag with a single legal value looks like scaffolding and is not: it is what
// makes slice 2's first paired A/B two invocations of one binary instead of two
// builds (ADR-0007 §Consequences).
const DefaultConfigName = "default"

// configs is every harness configuration compiled into this binary, by name.
//
// A map is fine here and is not in tension with the no-map rule on [Config]:
// nothing iterates it to produce a hash. [ConfigNames] sorts what it lists, so
// no output path depends on its order either.
var configs = map[string]Config{
	DefaultConfigName: {
		Name:    DefaultConfigName,
		Version: 1,

		// No system prompt exists yet (KAN-843). Empty is the honest value and
		// the hash records it as such.
		SystemPrompt: "",

		// The tool set of docs/SLICE-1.md build steps 5 and 6, in the order
		// the model is shown them: read before write, edit before its fuzzy
		// fallback. The names are internal/tools' constants, repeated here
		// rather than imported so that a configuration is a value and not a
		// view onto another package's registry — see hash.go on why this is
		// held to internal/tools by a test instead.
		ToolSet: []string{
			"read_file",
			"list_dir",
			"grep",
			"write_file",
			"edit_file",
			"edit_file_fuzzy",
			"run_shell",
		},

		// parse's own order, first success wins (docs/SLICE-1.md §3).
		ParseRoutes:  []string{"native", "fenced_json", "xml_tag"},
		RepairBudget: 2,

		// ADR-0005 §6 caps a corpus task at 20 turns, and internal/corpus
		// carries the same number per task.
		MaxTurns: 20,

		// An estimate, and labelled one. docs/provider-pin.md's "$2 per corpus
		// run" implies roughly 1.6M prompt tokens per task at the pinned
		// endpoint's prices; 2M is just above that, so the budget stops a
		// runaway loop without cutting short a session the cost model already
		// expects. The first real bench run's reported usage replaces it, and
		// replacing it moves the hash — which is correct, because a different
		// budget is a different arm.
		TokenBudget: 2_000_000,

		Verification: Verification{
			Forced: true,
			// The registered default is discovery. [Resolve] amends it to
			// [VerificationConfigured] for a repository whose
			// .kopicode/config.toml names a `verify` command, which moves the
			// hash — see the note there, and docs/SLICE-1.md §5.
			Source: VerificationDiscovered,
		},

		Sampling: Sampling{
			// Zero temperature and a fixed seed remove every source of
			// run-to-run variance the pinned endpoint lets the harness control
			// (docs/provider-pin.md: Parasail honours `seed`). ADR-0005's
			// paired design wants exactly that. The seed's value is arbitrary;
			// what matters is that it is fixed, recorded, and hashed.
			Temperature: 0,
			TopP:        1,
			MaxTokens:   8192,
			SeedPolicy:  SeedFixed,
			Seed:        1,
		},
	},
}
