package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/repo"
	"github.com/leejianrong/kopicode/internal/syntax"
	"github.com/leejianrong/kopicode/internal/tools"
)

// DefaultMaxTurns is the turn cap a [Config] that leaves MaxTurns zero gets.
//
// Twenty, because the frozen corpus is specified as tasks an agent can finish
// "in ≤20 turns" (docs/SLICE-1.md §Build Plan step 15) and a cap below the
// corpus's own bound would fail tasks the harness is meant to pass. It is a
// harness-configuration value (ADR-0007 decision 6 puts the cap in the hash
// preimage), so a bench arm sets it explicitly and this default is only what a
// caller gets for not choosing.
const DefaultMaxTurns = 20

// Snapshotter records the working tree after a turn that changed it.
//
// It is an interface, declared at the consumer, for the reason [Provider] is:
// *repo.Snapshotter satisfies it structurally without internal/repo learning
// that the loop exists, and a test can drive the "turn touched files → journal
// a TurnSnapshot" path without a git fixture. Slice 1 is write-only (affordance
// G1); restore and fork are slice 2 and nothing here reads a snapshot back.
type Snapshotter interface {
	Snapshot(ctx context.Context, turn int) (repo.Snapshot, error)
}

// Config is everything the loop needs: the identity of the arm it is running,
// the bounds it runs under, and the collaborators it drives.
//
// The identity fields are **inputs**, not decisions. Resolving a model id,
// its provider pin and its harness configuration is ADR-0007's precedence and
// lives in the surface (KAN-805); the engine is handed the resolved values and
// journals them on SessionStarted. An engine that resolved its own model would
// give the two front ends two answers to which arm ran.
type Config struct {
	// SessionID identifies the session. It is injected rather than generated
	// here because a replayed session must produce a byte-identical journal
	// and a generated id is one of the three things that would otherwise
	// differ (docs/SLICE-1.md §Test Plan; the clock is the journal's, and the
	// engine draws no randomness at all).
	SessionID string

	// ModelID, Pin and Sampling are the arm's provider-facing identity. They go
	// on every request and on SessionStarted.
	ModelID  string
	Pin      provider.Pin
	Sampling provider.Sampling

	// HarnessConfigHash identifies the harness configuration this session ran
	// under (ADR-0007 decision 6). The engine does not compute it: the preimage
	// includes the system prompt, the tool set as presented to the model and
	// the bounds below, and the value is resolved from the model id before the
	// engine is built.
	HarnessConfigHash string

	// Build identifies the binary, which the hash deliberately excludes, so
	// that two incomparable builds cannot pool as one arm (ADR-0007 decision
	// 7). Produced by internal/build and mapped by the surface.
	Build journal.BuildInfo

	// CWD is the directory the session was started in and RepoHead the commit
	// HEAD pointed at, both recorded on SessionStarted. RepoHead is "" outside
	// a repository.
	CWD      string
	RepoHead string

	// SystemPrompt opens every request. It is an input: there is no system
	// prompt in this repo yet and writing one here would be inventing a
	// per-arm value, because ADR-0007 decision 6 puts the prompt's digest
	// inside the harness config hash. KAN-843 writes it.
	SystemPrompt string

	// MaxTurns bounds one call to [Engine.Run]. Zero means [DefaultMaxTurns];
	// negative is a configuration error, because an unbounded loop is the
	// failure mode the cap exists to prevent. Reaching it stops the exchange
	// and journals SessionEnded with reason "max_turns", which SLICE-1 §9
	// classifies as a `harness` failure.
	MaxTurns int

	// TokenBudget bounds a session's total reported token usage. Zero means
	// unbounded; negative is a configuration error.
	//
	// **What "enforced" can mean here, stated rather than implied.** The only
	// authoritative token count is provider.Usage on a reply, so it arrives
	// after the request that spent it. This is therefore a *stop condition on
	// spend to date*, checked before every provider call: once the usage the
	// provider has reported reaches the budget, no further request is sent and
	// the exchange stops with reason "budget_exhausted". A session can exceed
	// the budget by at most the usage of the request in flight when it was
	// crossed. It is deliberately **not** admission control from
	// [Size.EstimatedTokens] — that method documents itself as not a token
	// count, and refusing a request on a byte estimate while journaling it as a
	// budget decision is fabricated precision.
	TokenBudget int

	// RepairBudget is how many repair round trips one reply gets before the
	// call fails and the turn continues with the failure as an observation
	// (docs/SLICE-1.md §3). Zero means [parse.DefaultBudget]; **negative
	// disables repair**, which is the arm that measures what repair buys.
	//
	// The asymmetry with MaxTurns is deliberate: zero repairs is a legitimate
	// experiment and zero turns is not, so zero means "the default" here and
	// the deliberate no-repair arm has to say so with -1 rather than by
	// leaving a field unset.
	RepairBudget int

	// Provider is the model provider. Required.
	Provider Provider

	// Journal is the session record. Required — there is exactly one record and
	// a loop without it is a loop nothing can be derived from.
	Journal journal.Journal

	// Tools is the tool set, bound to the repository root. Required.
	Tools *tools.Set

	// Permissions is the consent gate. Required: a nil gate would mean every
	// model-authored shell command runs unasked, and "no gate configured" must
	// not be the quiet way to get that.
	Permissions *permission.Gate

	// Syntax is the post-edit language check (ADR-0006 §4). Required, for the
	// same reason: it is the gate whose *absence* reads as a pass. Where no
	// checker exists for a language it records that honestly, so an
	// unsupported project needs no nil here.
	Syntax *syntax.Gate

	// Snapshots records the tree after a turn that ran a tool able to change
	// it. Optional: nil means no TurnSnapshot events, which is what a session
	// outside a git repository gets.
	Snapshots Snapshotter

	// Catalogue is what the repair loop consults to say what a call got wrong.
	// Nil means [Catalogue] — the schemas for the tools this engine
	// dispatches. Supplying one is how a bench arm varies the descriptions the
	// model is repaired against.
	//
	// This is **not** the tool catalogue as rendered on the wire.
	// provider.Request carries no tool definitions and this card does not add
	// any; KAN-844 decides that shape.
	Catalogue parse.Tools

	// Stream, when non-nil, is called once per delta as the reply arrives, on
	// the goroutine pulling the stream. It is the seam the REPL prints tokens
	// through (KAN-793).
	//
	// Synchronous on purpose. A channel or a goroutine here would put the
	// scheduler between the provider and the journal, and a byte-identical
	// replayed journal rests on there being no such thing in any output path.
	// A slow callback slows the turn, which is the honest trade.
	Stream func(provider.Delta)
}

// Errors a caller can branch on.
var (
	// ErrNotStarted is a Run or Close before Start.
	ErrNotStarted = errors.New("engine: session not started")
	// ErrAlreadyStarted is a second Start.
	ErrAlreadyStarted = errors.New("engine: session already started")
	// ErrEnded is a Run or Close after Close.
	ErrEnded = errors.New("engine: session already ended")
	// ErrConfig is a Config the engine refuses to run with.
	ErrConfig = errors.New("engine: invalid configuration")
)

// Engine is one session's bounded turn loop.
//
// It is single-threaded over its own turn and is not safe for concurrent use.
// The concurrency in this system lives inside the tool subprocesses; the loop
// itself starts no goroutine, and neither does anything it calls in an output
// path.
//
// Nothing here stores a context. Every call takes one as its first parameter
// and threads it to the provider, the consent gate, the tools and the syntax
// gate, which is what makes a cancelled turn reach the shell's process group.
type Engine struct {
	cfg Config
	asm *Assembler

	started bool
	ended   bool

	// turn is the session-wide 1-based turn counter, which is the journal
	// envelope's turn. It does not reset between exchanges: a REPL session's
	// second prompt continues the same record.
	turn int

	// spent is the token usage the provider has reported so far, summed. It is
	// the only token number the budget is allowed to be decided from.
	spent journal.TokenCounts

	// stop and detail are what the last exchange concluded, so Close can
	// journal a SessionEnded that says why rather than always saying
	// "completed".
	stop   Stop
	detail string
}

// New checks a configuration and returns the engine it describes. It journals
// nothing; [Engine.Start] opens the record.
func New(cfg Config) (*Engine, error) {
	switch {
	case cfg.SessionID == "":
		return nil, fmt.Errorf("%w: SessionID is required, and is injected rather than "+
			"generated so a replay reproduces the journal byte for byte", ErrConfig)
	case cfg.Provider == nil:
		return nil, fmt.Errorf("%w: Provider is required", ErrConfig)
	case cfg.Journal == nil:
		return nil, fmt.Errorf("%w: Journal is required — there is one session record and it is the journal", ErrConfig)
	case cfg.Tools == nil:
		return nil, fmt.Errorf("%w: Tools is required", ErrConfig)
	case cfg.Permissions == nil:
		return nil, fmt.Errorf("%w: Permissions is required; a nil gate would run model-authored "+
			"shell unasked, which must not be what forgetting a field does", ErrConfig)
	case cfg.Syntax == nil:
		return nil, fmt.Errorf("%w: Syntax is required; a gate that did not run must not read as a pass "+
			"(docs/adr/0006-hash-anchored-edits-and-failure-attribution.md §4)", ErrConfig)
	case cfg.MaxTurns < 0:
		return nil, fmt.Errorf("%w: MaxTurns %d is negative; zero means %d and an unbounded loop is "+
			"the failure the cap exists to prevent", ErrConfig, cfg.MaxTurns, DefaultMaxTurns)
	case cfg.TokenBudget < 0:
		return nil, fmt.Errorf("%w: TokenBudget %d is negative; zero means unbounded", ErrConfig, cfg.TokenBudget)
	}

	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = DefaultMaxTurns
	}
	if cfg.Catalogue == nil {
		cfg.Catalogue = Catalogue()
	}

	return &Engine{cfg: cfg, asm: NewAssembler(cfg.SystemPrompt)}, nil
}

// repairBudget maps Config.RepairBudget onto parse's, where negative already
// means zero.
func (e *Engine) repairBudget() int {
	if e.cfg.RepairBudget == 0 {
		return parse.DefaultBudget
	}
	if e.cfg.RepairBudget < 0 {
		return 0
	}
	return e.cfg.RepairBudget
}

// Session reports the session this engine records.
func (e *Engine) Session() journal.Session { return e.cfg.Journal.Session() }

// Turns reports how many turns the session has used across every exchange.
func (e *Engine) Turns() int { return e.turn }

// Tokens reports the token usage the provider has reported so far, summed. It
// is the figure the budget is decided from and the only one this package treats
// as a measurement.
func (e *Engine) Tokens() journal.TokenCounts { return e.spent }

// Start opens the session record.
func (e *Engine) Start(ctx context.Context) error {
	if e.started {
		return ErrAlreadyStarted
	}
	_, err := e.append(ctx, 0, journal.SessionStarted{
		CWD:      e.cfg.CWD,
		RepoHead: e.cfg.RepoHead,
		ModelID:  e.cfg.ModelID,
		Provider: journal.ProviderPin{
			Order:          e.cfg.Pin.Order,
			AllowFallbacks: e.cfg.Pin.AllowFallbacks,
			Quantizations:  e.cfg.Pin.Quantizations,
		},
		HarnessConfigHash: e.cfg.HarnessConfigHash,
		Build:             e.cfg.Build,
	})
	if err != nil {
		return err
	}
	e.started = true
	return nil
}

// Close writes SessionEnded and settles the session.
//
// The reason and exit code come from the last exchange, so a session whose loop
// hit the turn cap closes as "max_turns" rather than as "completed" — that is
// the fact SLICE-1 §9's classifier reads, and a Close that always said
// "completed" would launder a harness failure into a clean stop. A session
// closed without having run anything closes as "completed".
//
// The journal deliberately ignores a cancelled context on Append, so a session
// torn down by Ctrl-C still records why it ended.
func (e *Engine) Close(ctx context.Context) error {
	switch {
	case !e.started:
		return ErrNotStarted
	case e.ended:
		return ErrEnded
	}
	stop := e.stop
	if stop == StopUnspecified {
		stop = StopCompleted
	}
	_, err := e.append(ctx, 0, journal.SessionEnded{
		Reason:   stop.Reason(),
		ExitCode: stop.ExitCode(),
		Detail:   journal.InlineText(e.detail),
	})
	e.ended = true
	return err
}

// append writes one event, wrapping the failure so a journal that broke is
// never mistaken for a loop that did.
func (e *Engine) append(ctx context.Context, turn int, payload journal.Payload) (journal.Event, error) {
	ev, err := e.cfg.Journal.Append(ctx, turn, payload)
	if err != nil {
		return journal.Event{}, fmt.Errorf("engine: journaling %s: %w", payload.Type(), err)
	}
	return ev, nil
}
