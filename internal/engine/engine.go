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
	"github.com/leejianrong/kopicode/internal/verify"
)

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

	// Selection is the resolved arm: model id, provider pin, harness
	// configuration and the hash SessionStarted records. It is produced by
	// [ResolveSelection] before the engine is built (ADR-0007), and everything
	// the loop needs from it is read off it rather than restated here.
	//
	// **Every bound the loop runs under comes from Selection.Config**, not from
	// a constant in this package: the turn cap, the token budget, the repair
	// budget, the sampling parameters and the system prompt are all in the
	// harness config hash preimage, so a bound the loop held itself would be a
	// bound *outside* the arm — two runs differing in it would compare as
	// identical, which is the hash describing something that is not happening.
	Selection Selection

	// Build identifies the binary, which the hash deliberately excludes, so
	// that two incomparable builds cannot pool as one arm (ADR-0007 decision
	// 7). Produced by internal/build and mapped by the surface.
	Build journal.BuildInfo

	// CWD is the directory the session was started in and RepoHead the commit
	// HEAD pointed at, both recorded on SessionStarted. RepoHead is "" outside
	// a repository.
	CWD      string
	RepoHead string

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

	// Verify is forced verification: the project's own test command, run after
	// any turn that could have changed the tree (docs/SLICE-1.md §5).
	//
	// **Nil does not mean "skip".** It means the default verifier over
	// Tools.Root and Selection.Verify, built in [New]. That direction is the
	// whole point: this is a gate, and a gate whose absence reads as a pass is
	// the failure internal/syntax's package doc argues at length about. A
	// session that genuinely has nothing to verify gets there through
	// internal/verify reporting that it found no command, which is on the
	// record, rather than through a field somebody forgot.
	//
	// It is overridable because the timeout, the clock and the binary lookup are
	// all things a test has to drive, and because a bench arm may want a
	// different bound than an interactive session.
	Verify *verify.Verifier

	// Snapshots records the tree after a turn that ran a tool able to change
	// it. Optional: nil means no TurnSnapshot events, which is what a session
	// outside a git repository gets.
	Snapshots Snapshotter

	// Catalogue is what the repair loop consults to say what a call got wrong.
	// Nil means [Catalogue] over Selection.Config.ToolSet, which is what a
	// session should almost always use: the tools the arm presents.
	//
	// It is overridable because the descriptions a model is repaired against
	// are a harness axis ADR-0005 §7 defers the contents of. Overriding it does
	// **not** widen what the loop will dispatch — Selection.Config.ToolSet
	// decides that on its own, so a catalogue offering more cannot smuggle a
	// tool into an arm that does not present it.
	//
	// This is **not** the tool catalogue as rendered on the wire.
	// provider.Request carries no tool definitions and this card does not add
	// any; KAN-844 decides that shape.
	Catalogue parse.Tools

	// Stream, when non-nil, is called once per delta as the reply arrives, on
	// the goroutine pulling the stream, with the turn the reply belongs to. It
	// is the seam the REPL prints tokens through (KAN-793), reached from a
	// surface through [Options.Events].
	//
	// The turn is passed because a delta is the one thing a surface sees that
	// is not yet in the record, and without it the surface cannot say which
	// turn the text it is printing belongs to — journal events carry the turn
	// in their envelope, and a stream where only some events know is a stream
	// a renderer has to guess about.
	//
	// Synchronous on purpose. A channel or a goroutine here would put the
	// scheduler between the provider and the journal, and a byte-identical
	// replayed journal rests on there being no such thing in any output path.
	// A slow callback slows the turn, which is the honest trade.
	Stream func(turn int, d provider.Delta)
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

	// offered is the tool set the arm presents, as a membership test. It is
	// what the dispatcher checks, so a tool this binary can run but this arm
	// does not present cannot be reached — the hash says which tools the model
	// was given, and that has to be true rather than decorative.
	offered map[string]bool

	started bool
	ended   bool

	// turn is the session-wide 1-based turn counter, which is the journal
	// envelope's turn. It does not reset between exchanges: a REPL session's
	// second prompt continues the same record.
	turn int

	// spent is the token usage the provider has reported so far, summed. It is
	// the only token number the budget is allowed to be decided from.
	spent journal.TokenCounts

	// unverified is the last verification's rejection, still outstanding.
	//
	// It is the session's memory of "the project's own command says this tree is
	// not finished", and it is what stops the clean prose stop from being read as
	// success (docs/SLICE-1.md §5). It is set by a verification that ran and
	// exited non-zero, and cleared by one that ran and passed — a later passing
	// run is the only thing that answers an earlier failing one. A verification
	// that could not run neither sets nor clears it: it is not evidence in either
	// direction, and letting it clear a real failure would be the harness
	// forgiving a rejection because a timer went off.
	unverified string

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
	case cfg.Selection.ModelID == "":
		return nil, fmt.Errorf("%w: Selection carries no model id; resolve the arm with "+
			"ResolveSelection before building an engine (ADR-0007)", ErrConfig)
	case cfg.Selection.HarnessConfigHash == "":
		return nil, fmt.Errorf("%w: Selection carries no harness config hash; a session whose arm "+
			"cannot be identified pools with nothing (ADR-0007 decision 6)", ErrConfig)
	case cfg.Selection.Config.MaxTurns <= 0:
		return nil, fmt.Errorf("%w: the harness configuration's MaxTurns is %d; an unbounded loop is "+
			"the failure the cap exists to prevent, and a default supplied here would be a bound "+
			"outside the hash", ErrConfig, cfg.Selection.Config.MaxTurns)
	case cfg.Selection.Config.TokenBudget < 0:
		return nil, fmt.Errorf("%w: the harness configuration's TokenBudget is %d; zero means unbounded",
			ErrConfig, cfg.Selection.Config.TokenBudget)
	case cfg.Selection.Config.RepairBudget < 0:
		return nil, fmt.Errorf("%w: the harness configuration's RepairBudget is %d; zero is the "+
			"no-repair arm and is how the experiment measures what repair buys",
			ErrConfig, cfg.Selection.Config.RepairBudget)
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
	}

	// The tool set is validated even when a catalogue is supplied, because it
	// is the tool set and not the catalogue that decides what the loop will
	// dispatch.
	offered := make(map[string]bool, len(cfg.Selection.Config.ToolSet))
	for _, name := range cfg.Selection.Config.ToolSet {
		offered[name] = true
	}
	cat, err := Catalogue(cfg.Selection.Config.ToolSet)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	if cfg.Catalogue == nil {
		cfg.Catalogue = cat
	}

	// A missing verifier is filled in rather than treated as "no verification":
	// see the field's own comment. Root and the configured command are read off
	// what the engine was already given, so there is nothing here a surface has
	// to know about internal/verify — which matters, because ADR-0003's
	// allowlist does not let a front end import it.
	if cfg.Verify == nil {
		cfg.Verify = &verify.Verifier{
			Root:       cfg.Tools.Root.Path(),
			Configured: cfg.Selection.Verify,
		}
	}

	return &Engine{
		cfg:     cfg,
		asm:     NewAssembler(cfg.Selection.Config.SystemPrompt),
		offered: offered,
	}, nil
}

// Selection reports the arm this engine is running.
func (e *Engine) Selection() Selection { return e.cfg.Selection }

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
		ModelID:  e.cfg.Selection.ModelID,
		Provider: journalPin(e.cfg.Selection.Pin),
		// Copied, never recomputed: one session has one arm, and two events
		// disagreeing about which would be worse than either.
		HarnessConfigHash: e.cfg.Selection.HarnessConfigHash,
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

// journalPin maps the pin onto the journal's declaration of the same shape. The
// two packages agree on a wire contract rather than on an import.
func journalPin(p provider.Pin) journal.ProviderPin {
	return journal.ProviderPin{
		Order:          p.Order,
		AllowFallbacks: p.AllowFallbacks,
		Quantizations:  p.Quantizations,
	}
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
