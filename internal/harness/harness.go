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
	// 6). Editing it is a harness change and moves the hash, which is the
	// point: the prompt is a per-arm value, and results either side of an edit
	// are not comparable. [DefaultSystemPrompt] is what the shipped
	// configuration carries.
	SystemPrompt string

	// ToolSet is the tools presented to the model, in the order they are
	// presented — the order the system prompt's sections follow (KAN-843) and
	// the order [ToolCatalogue] renders in.
	//
	// This stays names only, and stays a duplicate of internal/tools' own
	// constants rather than an import of them, for the reason the package doc
	// comment gives: a configuration is a value, not a view onto another
	// package's registry. TestToolSetMatchesInternalTools (registry_test.go)
	// holds it to internal/tools by a test instead of an import.
	ToolSet []string

	// ToolCatalogue is the wire's tool-definition array (KAN-844): every
	// tool's name, one-clause description, and per-argument name, type,
	// required flag and one-clause description, in [ToolSet]'s order.
	//
	// ADR-0007 decision 6 puts "names, schemas, descriptions" in the preimage,
	// and until this field existed only the names did (see the note this
	// replaced, still readable in hash.go's history, and the still-narrower
	// note that used to sit here). This is the whole of what ADR-0007 asked
	// for, not a step towards it.
	//
	// It is a literal here rather than something computed by calling into
	// internal/engine, for the same reason [ToolSet] is a literal rather than
	// an import: internal/engine imports internal/harness
	// (internal/engine/selection.go), so the dependency cannot run the other
	// way, and a configuration that could only be built by calling into the
	// package that resolves it would not be a value ADR-0007 decision 5 can
	// hold up. internal/engine/toolcatalogue_test.go closes the loop the other
	// direction it can be closed: it renders the *real* engine catalogue —
	// engine.Catalogue(cfg.ToolSet), the same schemas engine.schemaOf derives
	// by reflection off the argument structs in catalogue.go, through
	// provider.RenderTools — and requires the result to equal this field
	// exactly. A description edited on one side and not the other fails there,
	// the same way TestSystemPromptDocumentsEveryToolArgument catches a prompt
	// and a struct tag disagreeing.
	//
	// No map anywhere in it: TestConfigHoldsNoMap walks the whole type graph
	// under Config, not just its direct fields, and provider.ToolParameters
	// renders JSON Schema's `properties` object from an ordered
	// []provider.ToolProperty for exactly this reason — see that type's doc
	// comment.
	ToolCatalogue []provider.ToolDefinition

	// AdvertiseNativeTools says whether [ToolCatalogue] is sent on the wire at
	// all as the request's native tool-definition array.
	//
	// This is the "arm variable" KAN-844's card names explicitly: SLICE-1 §3
	// has three tool-call extraction routes — native, fenced JSON, XML-tagged —
	// precisely because not every model reliably uses the native route, and
	// whether an arm *offers* that route in the first place is exactly the
	// kind of thing a second arm might vary deliberately, to measure whether
	// advertising it changes which route a model reaches for. A bool the loop
	// decided for itself would be a bound outside the hash, the same reason
	// MaxTurns and the rest live here and not as constants in internal/engine.
	//
	// true for the one configuration slice 1 registers: there is exactly one
	// arm today, so "always advertise" and "sometimes advertise" are not yet
	// different claims to have evidence about, and false would make this
	// card's whole point — that a rendered catalogue reaches the wire —
	// untestable by the one arm that exists. KAN-855 is what varies it.
	AdvertiseNativeTools bool

	// ParseRoutes is the tool-call extraction order this arm is willing to
	// accept a reply through, in parse.Route's wire spelling, first success
	// wins (docs/SLICE-1.md §3). It stays a duplicate of parse.Route's own
	// strings rather than an import of the type, for the reason [ToolSet]'s
	// own comment gives: a configuration is a value, and a []parse.Route field
	// would make internal/harness import internal/parse for a type this
	// package only ever round-trips through text.
	//
	// This is the accepting-side companion to [AdvertiseNativeTools]'s
	// telling side (KAN-855): whether the model is TOLD native tools exist
	// versus which routes the harness will ACCEPT when it answers. Until
	// KAN-855, this field was in the hash preimage and read by nothing —
	// internal/parse.Extract tried a fixed, unexported order with no
	// parameter, so an arm declaring a different ParseRoutes hashed
	// differently and behaved identically, which is the worse direction to be
	// wrong in: the hash claims a distinction the binary does not honour.
	// internal/engine.New now decodes this once per session, at the same
	// place it builds the tool catalogue, into the []parse.Route
	// [parse.NewRepairer] is actually built with — see decodeParseRoutes in
	// internal/engine/engine.go — and an entry that does not name a real
	// route (checked by [parse.Route.UnmarshalText]) or an empty list is a
	// startup [ErrConfig], not a silent fall-back to the package default: a
	// misconfigured arm must fail loudly rather than quietly running the
	// ordinary one under a different hash.
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

		// prompt_default.md, compiled in. Landing it moved this
		// configuration's hash away from the empty-prompt one every earlier
		// build wrote, and TestDefaultConfigHashIsStable's literal moved with
		// it — see the note there. PreimageVersion did not: the encoding is
		// unchanged, only the value.
		SystemPrompt: DefaultSystemPrompt,

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

		// The wire's tool-definition catalogue (KAN-844), in ToolSet's
		// order. Every name, description and argument here is a literal
		// copy of what internal/engine/catalogue.go's argument structs
		// declare in their `json`, `kopicode` and `kopicode_desc` tags —
		// duplicated for the reason ToolSet's own note gives, and held
		// equal to the source by internal/engine/toolcatalogue_test.go
		// rather than by trust.
		ToolCatalogue: []provider.ToolDefinition{
			{Type: "function", Function: provider.ToolFunction{
				Name:        "read_file",
				Description: "Read a text file from the repository, returning each line with an anchor you can later edit by.",
				Parameters: provider.ToolParameters{
					Properties: []provider.ToolProperty{
						{Name: "path", Type: "string", Description: "the file's path, relative to the repository root"},
						{Name: "offset", Type: "integer", Description: "1-based line number to start returning from; 0 or omitted means the beginning of the file"},
						{Name: "limit", Type: "integer", Description: "maximum number of lines to return; 0 or omitted uses the tool's per-call maximum"},
					},
					Required: []string{"path"},
				},
			}},
			{Type: "function", Function: provider.ToolFunction{
				Name:        "list_dir",
				Description: "List a directory's entries, optionally recursively and filtered by a glob pattern.",
				Parameters: provider.ToolParameters{
					Properties: []provider.ToolProperty{
						{Name: "path", Type: "string", Description: "the directory's path, relative to the repository root; empty means the repository root"},
						{Name: "recursive", Type: "boolean", Description: "when true, also list nested directories, skipping .git and .kopicode"},
						{Name: "pattern", Type: "string", Description: "a glob matched against an entry's file name, or its whole relative path when the pattern contains a slash"},
					},
				},
			}},
			{Type: "function", Function: provider.ToolFunction{
				Name:        "grep",
				Description: "Search text files for a regular expression, returning matching lines with their file and line number.",
				Parameters: provider.ToolParameters{
					Properties: []provider.ToolProperty{
						{Name: "pattern", Type: "string", Description: "the RE2 regular expression to search for; no backreferences or lookahead"},
						{Name: "path", Type: "string", Description: "the file or directory to search, relative to the repository root; empty means the whole repository"},
						{Name: "include", Type: "string", Description: "a glob restricting which file names are searched"},
						{Name: "ignore_case", Type: "boolean", Description: "when true, match case-insensitively"},
					},
					Required: []string{"pattern"},
				},
			}},
			{Type: "function", Function: provider.ToolFunction{
				Name:        "write_file",
				Description: "Create a file or replace its entire contents, creating parent directories as needed.",
				Parameters: provider.ToolParameters{
					Properties: []provider.ToolProperty{
						{Name: "path", Type: "string", Description: "the file's path, relative to the repository root"},
						{Name: "content", Type: "string", Description: "the file's complete new contents, replacing anything already there"},
					},
					Required: []string{"path", "content"},
				},
			}},
			{Type: "function", Function: provider.ToolFunction{
				Name:        "edit_file",
				Description: "Replace one anchored region of a file with new text, using anchors printed by a prior read_file call.",
				Parameters: provider.ToolParameters{
					Properties: []provider.ToolProperty{
						{Name: "path", Type: "string", Description: "the file's path, relative to the repository root"},
						{Name: "anchor_start", Type: "string", Description: "the anchor of the region's first line, printed by a prior read_file call"},
						{Name: "anchor_end", Type: "string", Description: "the anchor of the region's last line, printed by a prior read_file call; the same anchor twice replaces one line"},
						{Name: "new_text", Type: "string", Description: "the replacement text for the anchored region; empty deletes it"},
					},
					Required: []string{"path", "anchor_start", "anchor_end", "new_text"},
				},
			}},
			{Type: "function", Function: provider.ToolFunction{
				Name:        "edit_file_fuzzy",
				Description: "Replace text matched by similarity when no anchor is available; refuses an ambiguous or too-weak match. Prefer edit_file.",
				Parameters: provider.ToolParameters{
					Properties: []provider.ToolProperty{
						{Name: "path", Type: "string", Description: "the file's path, relative to the repository root"},
						{Name: "before", Type: "string", Description: "text believed to be in the file, matched by similarity with whitespace normalised"},
						{Name: "after", Type: "string", Description: "the replacement text for whatever before matched"},
					},
					Required: []string{"path", "before", "after"},
				},
			}},
			{Type: "function", Function: provider.ToolFunction{
				Name:        "run_shell",
				Description: "Run a command line through the platform shell and return its combined output and exit status.",
				Parameters: provider.ToolParameters{
					Properties: []provider.ToolProperty{
						{Name: "command", Type: "string", Description: "the command line to run, interpreted by the platform shell"},
						{Name: "dir", Type: "string", Description: "the working directory, relative to the repository root; empty means the repository root"},
						{Name: "timeout_seconds", Type: "integer", Description: "how many seconds to allow the command to run; 0 or omitted uses the tool's default of 120"},
					},
					Required: []string{"command"},
				},
			}},
		},

		// One arm exists; it advertises the native route. See
		// AdvertiseNativeTools's own doc comment for why this is a
		// configuration value and not a constant.
		AdvertiseNativeTools: true,

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
