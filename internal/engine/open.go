package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/leejianrong/kopicode/internal/build"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/lock"
	"github.com/leejianrong/kopicode/internal/permission"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/repo"
	"github.com/leejianrong/kopicode/internal/syntax"
	"github.com/leejianrong/kopicode/internal/tools"
)

// This file is the engine's front door, and it exists because [Config] is not
// one.
//
// ADR-0003 decision 3 says both surfaces drive the engine through its exported
// interface only, and internal/arch enforces it: a front end may import
// internal/engine, internal/bench and internal/build, and nothing else. Config
// is a struct of journal.Journal, *tools.Set, *permission.Gate, *syntax.Gate,
// parse.Tools and func(provider.Delta) — every one of them a type cmd/ cannot
// *name*, since Go needs an import to write a type in a struct literal, a type
// switch or a function literal's parameter list. So the exported interface was
// not reachable from the surface it was exported for.
//
// [Open] is the fix, and it is the ADR being satisfied rather than amended: the
// allowlist is unchanged and still has three entries. What changes is that the
// engine now offers a constructor whose inputs are stdlib types, types declared
// in this package, and internal/build — so a front end asks for a session
// instead of assembling one out of the engine's internals.
//
// cmd/kopibench is deliberately not in scope. internal/bench is allowlisted and
// may import whatever it needs, so the bench runner wires its own collaborators
// — a worktree per task, a non-interactive policy, a recorded provider — and
// routing that through a constructor built for an interactive session would
// make both worse.

// Options is everything a front end supplies to start a session.
//
// Every field is a stdlib type, a type declared in this package, or a type from
// internal/build. That is the constraint the whole file is written against and
// it is checked, not assumed: internal/arch walks cmd/ and would fail on any
// import this shape forced.
//
// What is deliberately *not* here is as informative as what is:
//
//   - **The verifier.** Nil means the default built from the tool root and the
//     selection, never "skip" ([Config.Verify]). A surface able to pass nil
//     would be a surface able to disable a gate by forgetting a field, and a
//     gate whose absence reads as a pass is the failure ADR-0006 §4 is about.
//   - **The syntax gate, the permission gate, the tool set, the snapshotter and
//     the journal.** All built here from Dir. A front end that could substitute
//     one could substitute a permissive one.
//   - **The harness configuration.** It comes from Selection and nowhere else:
//     every bound the loop runs under is in the hash preimage, so a bound
//     supplied here would be a bound outside the arm (ADR-0007 decision 6).
type Options struct {
	// Dir is the directory the session runs in — the tool root, the journal's
	// root, and what containment is judged against. "" means the process's
	// working directory.
	Dir string

	// Selection is the resolved arm, from [ResolveSelection]. Required.
	//
	// It is an input rather than something this call resolves, because
	// ADR-0007 decision 4 requires the resolution to happen *before* anything
	// is opened: an unknown model is a startup usage error, and a session that
	// never started must leave no journal, no lock and no half-written record.
	// Resolving here would put the refusal after the directory was created.
	Selection Selection

	// SessionID identifies the session. "" mints one from the clock and
	// crypto/rand.
	//
	// A caller wanting a replay to produce a byte-identical journal supplies
	// its own: the id is one of the three injected sources that criterion rests
	// on (docs/SLICE-1.md §Test Plan), and a generated one is the source that
	// would otherwise differ.
	//
	// Resuming a session (see [Options.Resume]) reuses this same field: SessionID
	// is required either way, and what changes is only whether it is expected to
	// already exist.
	SessionID string

	// Resume tells Open that SessionID names an existing session to continue
	// rather than a new one (KAN-939). It requires a non-empty SessionID —
	// resuming "mint one for me" is not a request Open can satisfy — and a
	// session directory that already exists at that id; either failing is a
	// configuration error reported before anything else opens.
	//
	// It changes three things, all downstream of the same one bit rather than
	// three independent flags, because "this is a continuation" is one fact
	// and asking a caller to keep three switches in agreement about it is how
	// they drift:
	//
	//   - The journal opens in append mode and keeps the existing sequence
	//     numbering. This already happens for *any* SessionID that names an
	//     existing session directory — [journal.Open] resumes unconditionally
	//     — so Resume changes nothing about the journal itself; it only makes
	//     the intent explicit rather than incidental.
	//   - The session's prior events are read back and replayed into the
	//     turn loop's message history ([Config.ResumeHistory]), so the first
	//     new turn's request carries the whole prior conversation rather than
	//     starting from nothing.
	//   - Inside a git repository with [SnapshotsAuto], the turn snapshotter
	//     attaches to the session's existing shadow-ref chain
	//     ([repo.NewResumingSnapshotter]) instead of refusing it, so the next
	//     snapshot chains onto the last recorded turn.
	//
	// False (the zero value) is what every ordinary session gets, including
	// one whose caller-supplied SessionID happens to collide with an old
	// session's id by accident: the journal still resumes (as it always has),
	// but the assembler starts with no history and, inside a repository, the
	// snapshotter still refuses a session id that already has shadow refs —
	// exactly as it did before this field existed. Resume has to be asked for;
	// a collision alone is never read as a request to continue.
	//
	// # What Resume deliberately does not do
	//
	// It does not restore the working tree to any prior snapshot's state.
	// [Open] does not call [repo.Repo.Restore] on a caller's behalf, and this
	// is an argued decision rather than a gap: the ordinary reason to resume a
	// session is that a process was interrupted and the tree is exactly where
	// it was left, and force-restoring in that case would be work with no
	// payoff at best. The case that is not ordinary — the user made changes to
	// the tree between the interruption and the resume, by hand or with
	// another tool — is exactly the case where restoring would silently
	// discard that work with no consent asked, which is the direction ADR-0006
	// and this project's edit tool spend real effort refusing to be wrong in.
	// A caller that wants the tree rewound first has [repo.Repo.Restore]
	// available as its own explicit step, before calling Open, using the
	// Tree recorded on the last journal.TurnSnapshot event this session wrote.
	Resume bool

	// Fork, when non-nil, tells [Fork] which session and turn to branch a
	// brand-new session from, sharing that session's history up to Fork.Turn
	// and diverging after (KAN-940). It is meaningless to [Open], which
	// refuses outright when it is set — see [Fork]'s own doc comment for why
	// this is a second entry point rather than a third [Options.Resume]-style
	// bool.
	//
	// SessionID here (if non-empty) names the *new* session, exactly as it
	// does for every other call; Fork.SessionID names the *source*. The two
	// must differ — Fork is not another spelling of Resume — and Fork
	// refuses a caller who set them equal before touching the filesystem.
	//
	// Fork requires a git repository at Dir, and Snapshots must not be
	// SnapshotsOff: both the source session's tree at Fork.Turn and the new
	// session's own snapshot chain live there, and CLAUDE.md's argument for
	// why a fork restores the tree automatically — see [Fork]'s doc
	// comment — has no lesser version for a caller that opted out of
	// snapshots.
	Fork *ForkSource

	// Events receives the session's events as they are recorded, in record
	// order, on the goroutine running the turn. Nil means nothing is announced.
	//
	// It is a tee over the journal and not a hook in the loop: what a surface
	// is told is what the record accepted, and the append happens first. See
	// [Event].
	Events Observer

	// Consent answers permission requests. Nil refuses everything, which is the
	// fail-closed direction — see [asker.Ask].
	Consent Consenter

	// ConsentMode says who Consent's answers are attributed to on the record:
	// a human, or the harness itself with nobody present. The zero value,
	// [ConsentInteractive], is what a caller gets by not setting the field,
	// and it is what the REPL wants — see [ConsentMode].
	//
	// This is KAN-885's fix: `run --print` has no terminal and no human, so it
	// leaves Consent nil and sets this to [ConsentUnattended] — mustAskPolicy
	// answers that combination itself (permission.NewUnattendedDeny) — and
	// every resulting journal.PermissionDecided is stamped
	// permission.SourcePolicy instead of permission.SourceUser. Without this
	// field the engine had no way to tell the two apart, and every headless
	// denial was recorded as though a human had made it.
	ConsentMode ConsentMode

	// Ask answers the ask tool's calls (docs/adr/0009-ask-tool-contract.md).
	// Nil refuses every question and says why, the same fail-closed direction
	// as a nil Consent — see [noAnswerer].
	Ask Answerer

	// AskMode says who Ask's replies are attributed to, mirroring ConsentMode
	// for the identical reason: attribution is not the surface's fact to
	// assert about itself. The zero value, [AskInteractive], is what the REPL
	// wants; `run --print`'s denyHeadlessAsk sets this to [AskUnattended] the
	// same way its Consent sets ConsentMode.
	AskMode AskMode

	// Policy, when non-nil, is ADR-0011's declared allowlist: the caller
	// states, up front, the closed set of shell argv this session may run and
	// the root writes outside the repo root are confined to, and [Open] builds
	// [permission.AllowlistPolicy] from it instead of routing consent through
	// [Consent]/[ConsentMode] at all. `cmd/kopicode run --print --policy-file`
	// is the first caller (docs/adr/0011-unattended-invocation-policy-gate.md).
	//
	// It carries [PolicyFile]'s declared *shape* rather than a live
	// permission.Policy value, for the same reason the syntax gate, the
	// permission gate and the tool set are not Options fields at all (see this
	// struct's own doc comment, "what is deliberately not here"):
	// [permission.NewAllowlist] needs a [permission.Resolver], and the only one
	// that judges paths correctly for this session is the one built from this
	// session's own tool root a few lines into [openSession] — before that
	// point, nothing exists for a caller-supplied resolver to agree with. A
	// field of the finished interface type would therefore either force this
	// file to expose its internal resolver to every caller, or accept a
	// resolver built some other way and risk two implementations of
	// containment disagreeing (see [permission.Resolver]'s own doc comment for
	// why that is the failure this package works to avoid). Declared data that
	// [Open] turns into the real policy itself has neither problem.
	//
	// The zero value (nil) is every existing caller's behaviour, unchanged:
	// [Open] still calls [mustAskPolicy] on [Consent]/[ConsentMode] exactly as
	// it did before this field existed, so `run --print` with no
	// --policy-file and the REPL are both byte-for-byte what they were.
	//
	// Setting this alongside a non-nil [Consent] is refused outright, as an
	// [ErrConfig] — never "one wins silently." The two are different live
	// answerers to the identical question ("may this action proceed"), and a
	// caller that wired both has not said which one it means; guessing would
	// be exactly the kind of ambiguity ADR-0007 decision 4's ordering exists
	// to refuse before anything is touched, and refusing loudly costs nothing
	// here since it is caught before the working tree is even looked at.
	//
	// [ConsentMode] is deliberately *not* part of that check. It only decides
	// attribution for the [Consent]-based path — [mustAskPolicy]'s branch,
	// which [gatePolicy] never calls at all once Policy is set — so a caller
	// like `headless` that always sets ConsentMode to [ConsentUnattended]
	// regardless of whether --policy-file was passed is not stating a second
	// conflicting answerer; it is a mode flag that this Policy-set branch
	// simply has no use for. [permission.AllowlistPolicy] stamps
	// [permission.SourcePolicy] on its own, independent of ConsentMode.
	Policy *PolicyFile

	// Provider overrides the model provider. Nil builds the live OpenRouter
	// client from OPENROUTER_API_KEY.
	//
	// It is an override rather than a required field because a session's
	// provider is decided by the arm, not by the surface; and it exists at all
	// because the mock/replay provider is SLICE-1's primary test seam (P2) and
	// a constructor that could only build a billable client could not be
	// exercised at zero token cost.
	Provider Provider

	// ProviderBaseURL points the live client at a different API root — a proxy,
	// a self-hosted gateway, or an httptest server. "" means OpenRouter.
	// Ignored when Provider is set.
	ProviderBaseURL string

	// Snapshots decides whether turn snapshots are written. The zero value
	// writes them in a repository and not outside one, which is what an
	// interactive session wants.
	Snapshots SnapshotMode

	// Now is the clock the journal stamps events from and the snapshotter
	// commits with. Nil means time.Now.
	Now func() time.Time
}

// ConsentMode says whether [Options.Consent]'s answers are a human's or the
// harness's own, so [Open] can tell permission.NewAsk which [permission.Source]
// to stamp on every resulting journal.PermissionDecided.
//
// It exists opposite [Options.Consent] rather than as a field the surface
// stuffs into the Consenter's own answer, because attribution is not the
// surface's fact to assert about itself — it is the one thing the engine has
// to get right for the record to be trustworthy, and a self-reported "I am a
// human" is not a control. A surface says how it was built (REPL vs. headless);
// the engine decides what that means for the journal (CLAUDE.md, "the engine
// decides policy, the surfaces decide presentation").
type ConsentMode uint8

const (
	// ConsentInteractive is the zero value: Consent's answers came from a
	// human relayed by a surface, and every PermissionDecided this session
	// journals is stamped permission.SourceUser. This is what the REPL uses,
	// and — because it is the zero value — what a caller gets by not setting
	// the field at all, which is what keeps the REPL unchanged by this field
	// existing.
	ConsentInteractive ConsentMode = iota

	// ConsentUnattended says nobody is present to ask: Consent, if it answers
	// at all, is the harness answering its own question, and every
	// PermissionDecided this session journals is stamped permission.SourcePolicy
	// instead. `run --print` leaves Consent nil under this mode — mustAskPolicy
	// answers for it — rather than supplying its own Consenter; a caller sets
	// this exactly when Options.Consent has no human behind it, and setting it
	// while Consent really does put the question to a person just misattributes
	// in the opposite direction.
	ConsentUnattended
)

// SnapshotMode says whether a session records the tree after a turn that could
// have changed it (affordance G1, ADR-0002 §3).
type SnapshotMode uint8

const (
	// SnapshotsAuto writes a shadow ref per modifying turn when the session
	// directory is a git repository, and nothing when it is not. It is the
	// zero value and what an interactive session uses.
	SnapshotsAuto SnapshotMode = iota

	// SnapshotsOff writes none.
	//
	// It exists because a caller can already have the tree under control by
	// other means — the bench runner gives every task its own worktree off a
	// frozen commit, so a second history of the same turns buys nothing and
	// leaves refs to reclaim. It is **not** a way to run quietly in a user's
	// repository: turning it on in an interactive session throws away the only
	// record of what the tree looked like per turn.
	SnapshotsOff
)

// ErrNoAPIKey is returned when no Provider was supplied and
// OPENROUTER_API_KEY is not set. It is a configuration failure and is reported
// before anything billable is attempted.
var ErrNoAPIKey = errors.New("engine: OPENROUTER_API_KEY is not set")

// APIKeyEnv is the environment variable the live client's credential comes
// from. It is named here so a surface can say so in an error message without
// importing internal/provider.
const APIKeyEnv = "OPENROUTER_API_KEY"

// ErrSessionLocked reports that another live session already holds the working
// tree (docs/SLICE-1.md §8). The error returned by [Open] wraps it and its
// message names the holder, so a surface prints the error and matches on this
// rather than on prose.
//
// It is re-exported from internal/lock because ADR-0003's allowlist gives a
// front end three internal imports and internal/lock is not one of them: the
// engine decides that a second session is refused, and the surface decides how
// to say so, which is the same division the permission gate is built on.
var ErrSessionLocked = lock.ErrHeld

// Session is a running session and everything it holds open.
//
// It exists so that a front end has **one** thing to close. Open acquires a
// journal file, a tool root handle and a shadow-ref index; returning the engine
// and expecting the caller to remember three teardowns is how a REPL that
// exits by an unusual path leaves an unfsynced record behind.
//
// The methods delegate rather than embed, deliberately. An embedded *Engine
// would put [Engine.Close] — which writes SessionEnded and releases nothing —
// one dot away from [Session.Close], and picking the wrong one leaks the file
// handles while looking correct.
type Session struct {
	engine  *Engine
	closers []func() error

	id  string
	dir string
}

// ID reports the session identifier, generated or supplied.
func (s *Session) ID() string { return s.id }

// Dir reports the resolved directory the session runs in.
func (s *Session) Dir() string { return s.dir }

// Path reports the session's directory under .kopicode, where the journal and
// its blobs live. A surface prints it so a user can go and read the record.
func (s *Session) Path() string { return journal.SessionDir(s.dir, s.id) }

// Selection reports the arm this session is running.
func (s *Session) Selection() Selection { return s.engine.Selection() }

// Turns reports how many turns the session has used across every exchange.
func (s *Session) Turns() int { return s.engine.Turns() }

// Tokens reports the provider-reported usage so far, summed.
func (s *Session) Tokens() journal.TokenCounts { return s.engine.Tokens() }

// Run is one bounded exchange. See [Engine.Run].
func (s *Session) Run(ctx context.Context, prompt string) (Result, error) {
	return s.engine.Run(ctx, prompt)
}

// Close writes SessionEnded and releases everything Open acquired.
//
// It is safe to call once. Every resource is released even when an earlier step
// failed — a journal that could not be closed must not leave the tool root open
// — and the first error is returned with the rest joined onto it, because a
// teardown that reported only its first failure is a teardown that hides the
// others.
func (s *Session) Close(ctx context.Context) error {
	errs := []error{s.engine.Close(ctx)}
	for i := len(s.closers) - 1; i >= 0; i-- {
		errs = append(errs, s.closers[i]())
	}
	s.closers = nil
	return errors.Join(errs...)
}

// Open builds a session in Options.Dir and opens its record.
//
// The order matters and is the order ADR-0007 decision 4 asks for. Everything
// that can be refused without touching the filesystem is refused first, so a
// session that will not run leaves nothing behind; then the working tree's
// advisory lock, which is the one refusal that needs a file and creates none on
// the path that refuses (docs/SLICE-1.md §8); then the journal.
// [ResolveSelection] happens earlier still, in the caller.
//
// Two refusals here are conditions rather than faults, and both are matchable:
// [ErrNoAPIKey] and [ErrSessionLocked].
//
// It journals nothing. [Session.Run] and [Session.Close] do; the caller starts
// the record with [Engine.Start] by way of the returned session having already
// done so — see below.
//
// It refuses outright when [Options.Fork] is set. Open and [Fork] mint
// different things — one session continuing (or starting fresh) in place,
// one branching a brand-new session from a copy of another's history — and a
// single call guessing which was meant from which optional fields happen to
// be set is exactly the kind of ambiguity ADR-0007 decision 4's ordering
// exists to refuse before anything is touched, rather than resolve by
// convention.
func Open(ctx context.Context, opts Options) (*Session, error) {
	if opts.Fork != nil {
		return nil, fmt.Errorf("%w: Options.Fork is set; call Fork instead of Open", ErrConfig)
	}
	return openSession(ctx, opts, nil)
}

// Fork branches a brand-new session from a copy of an existing one's history,
// up to and including Options.Fork.Turn, diverging after (KAN-940).
//
// It shares Open's plumbing — the credential check, the working-tree lock,
// the tool set and consent gate, the provider, the final [New] and
// [Engine.Start] — through [openSession], and diverges in exactly the ways
// [Options.Fork]'s own doc comment and fork.go's package comment argue:
// which journal supplies the new session's history (the source's, copied
// rather than referenced), whether the working tree moves (restored to the
// source's tree at the chosen turn, automatically, because forking's whole
// point requires the tree to actually be there), and how the new session's
// snapshot chain is seeded ([repo.NewForkingSnapshotter], attached to the
// source's chain without adopting its turn numbers).
//
// Every refusal below runs before anything is opened, locked or written —
// the same ordering ADR-0007 decision 4 holds Open to — so a caller who got
// Fork's arguments wrong leaves nothing behind to clean up.
func Fork(ctx context.Context, opts Options) (*Session, error) {
	switch {
	case opts.Fork == nil:
		return nil, fmt.Errorf("%w: Fork needs Options.Fork naming the session and turn to branch from",
			ErrConfig)
	case opts.Resume:
		return nil, fmt.Errorf("%w: Options.Resume and Options.Fork are mutually exclusive: Resume "+
			"continues an existing session's own record in place, Fork starts a brand-new one seeded "+
			"from a copy of another session's history", ErrConfig)
	case opts.Fork.SessionID == "":
		return nil, fmt.Errorf("%w: Options.Fork.SessionID is empty; Fork needs to know which session's "+
			"history to branch from", ErrConfig)
	case opts.Fork.Turn < 0:
		return nil, fmt.Errorf("%w: Options.Fork.Turn is %d; turns are numbered from 0 (0 forks before "+
			"any turn ran)", ErrConfig, opts.Fork.Turn)
	case opts.SessionID != "" && opts.SessionID == opts.Fork.SessionID:
		return nil, fmt.Errorf("%w: Options.SessionID equals Options.Fork.SessionID (%q); a fork mints "+
			"a different session id than its source — leave SessionID empty to mint one, or supply a "+
			"different one", ErrConfig, opts.Fork.SessionID)
	case opts.Snapshots == SnapshotsOff:
		return nil, fmt.Errorf("%w: Options.Snapshots is SnapshotsOff; Fork needs turn snapshots to find "+
			"the source session's tree at the chosen turn and to give the new session its own chain",
			ErrConfig)
	}
	return openSession(ctx, opts, opts.Fork)
}

// openSession is Open and Fork's shared body. fork is nil for Open (and for
// Fork's own caller-facing refusals above, which never reach here) and the
// validated source for Fork.
func openSession(ctx context.Context, opts Options, fork *ForkSource) (*Session, error) {
	if opts.Selection.ModelID == "" {
		return nil, fmt.Errorf("%w: Options.Selection carries no model id; resolve the arm with "+
			"ResolveSelection before opening a session (ADR-0007 decision 4)", ErrConfig)
	}
	if opts.Resume && opts.SessionID == "" {
		return nil, fmt.Errorf("%w: Options.Resume is set but SessionID is empty; resuming needs "+
			"the id of an existing session, and minting a fresh one would not be resuming anything", ErrConfig)
	}
	if opts.Policy != nil && opts.Consent != nil {
		return nil, fmt.Errorf("%w: Options.Policy is set together with Options.Consent; these are two "+
			"different answerers for the same question ('may this action proceed') and this package will "+
			"not guess which one a caller meant — leave Consent nil when using Options.Policy "+
			"(docs/adr/0011-unattended-invocation-policy-gate.md)", ErrConfig)
	}

	dir := opts.Dir
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("engine: resolving the working directory: %w", err)
		}
		dir = cwd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("engine: resolving %s: %w", dir, err)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}
	id := opts.SessionID
	if id == "" {
		id = newSessionID(now())
	}

	if opts.Resume {
		// A read-only check, before anything opens for write: Options.Resume is
		// the caller asserting a specific prior session exists, and a caller
		// that is wrong about that should be told so rather than silently
		// handed a new, empty session under the id it expected to continue.
		eventsPath := filepath.Join(journal.SessionDir(abs, id), journal.EventsFile)
		if _, err := os.Stat(eventsPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: Options.Resume is set for session %q, but %s does not exist; "+
					"there is no prior session to resume", ErrConfig, id, eventsPath)
			}
			return nil, fmt.Errorf("engine: checking for a resumable session at %s: %w", eventsPath, err)
		}
	}

	if fork != nil {
		// Two read-only checks, before anything opens for write, mirroring
		// Resume's own: the source Fork names must actually exist, and — the
		// direction Resume never has to check, because it *wants* an existing
		// session — the new id must not already have one. A caller-supplied id
		// that happens to collide with an unrelated existing session is
		// refused rather than silently extended with a copied prefix it never
		// held: [journal.SessionForked]'s whole argument is a session record
		// that honestly says how it began, and appending a fork marker partway
		// through someone else's history would not.
		srcEventsPath := filepath.Join(journal.SessionDir(abs, fork.SessionID), journal.EventsFile)
		if _, err := os.Stat(srcEventsPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: Options.Fork names session %q as the source, but %s does not "+
					"exist", ErrConfig, fork.SessionID, srcEventsPath)
			}
			return nil, fmt.Errorf("engine: checking for a forkable session at %s: %w", srcEventsPath, err)
		}
		newEventsPath := filepath.Join(journal.SessionDir(abs, id), journal.EventsFile)
		switch _, err := os.Stat(newEventsPath); {
		case err == nil:
			return nil, fmt.Errorf("%w: a session already exists at %q (%s); Fork mints a brand-new "+
				"session and refuses to extend one that already has a record — pick a different "+
				"SessionID or leave it empty to mint one", ErrConfig, id, newEventsPath)
		case !errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("engine: checking for a pre-existing session at %s: %w", newEventsPath, err)
		}
	}

	// Process lifecycle, not session content: which directory, which session
	// id and which arm this invocation resolved to, not anything the model or
	// the user said. The resolution itself already ran in the caller
	// (ResolveSelection) and is journaled on SessionStarted; this line is the
	// diagnostic echo of it, off by default and to stderr (CLAUDE.md, "slog
	// never duplicates the journal").
	slog.Debug("opening session",
		"dir", abs, "session", id,
		"model", opts.Selection.ModelID, "harness", opts.Selection.Config.Name)

	// The credential check first, because a missing one is the one failure
	// that costs nothing to detect and would otherwise be found after a
	// directory had been created and a record opened. It is only the check:
	// building the actual client waits until the journal exists, a few lines
	// down, because the live client's retry observer appends to it (KAN-851)
	// and nothing here may build a client before there is a journal to wire it
	// to. Splitting the two keeps ADR-0007 decision 4's ordering exactly as it
	// was — a refused Open still touches nothing.
	if err := requireProviderCredential(opts); err != nil {
		return nil, err
	}

	// Then the working tree's lock, and it is the first thing here that touches
	// disk on purpose — docs/SLICE-1.md §8 refuses a second session in one tree,
	// and ADR-0007 decision 4's ordering applies to that refusal exactly as it
	// does to an unknown model: before a prompt is assembled, before the journal
	// is opened, before SessionStarted is written. Nothing below this line can
	// leave a half-session record for a session that never started.
	//
	// It creates .kopicode/lock, which looks like a contradiction and is not:
	// the *only* way to be refused is for a holder to exist, and a holder
	// exists only because it already created the directory and the file. So a
	// refused invocation creates nothing. What creates the file is the session
	// that goes on to run.
	//
	// The root is the work tree, not the git directory. A linked worktree
	// shares .git/config, the object store and the ref store with its parent,
	// so keying on the git directory would serialise the bench runner's ten
	// concurrent task worktrees — see internal/lock's package comment.
	lockRoot := repo.WorkTreeRoot(ctx, abs)
	held, err := lock.Acquire(lockRoot, lock.Holder{
		Session: id,
		Record:  journal.SessionDir(abs, id),
	})
	if err != nil {
		// The log call lives here and not inside internal/lock: that package
		// stays a leaf with no new dependency, and the engine is the one place
		// that already knows this is a session-opening step worth narrating.
		// A refusal names the holder — a fact about another process, not about
		// this session's content — so it is exactly what CLAUDE.md's boundary
		// calls a diagnostic fact.
		var heldErr *lock.HeldError
		if errors.As(err, &heldErr) {
			slog.Debug("session lock refused", "root", lockRoot, "holder", heldErr.Holder.String())
		} else {
			slog.Debug("session lock acquisition failed", "root", lockRoot, "error", err)
		}
		return nil, err
	}
	slog.Debug("session lock acquired", "root", lockRoot, "session", id)

	s := &Session{id: id, dir: abs}
	// The lock is released last, because closers unwind in reverse: the record
	// is closed and fsynced while the tree is still ours.
	s.closers = append(s.closers, held.Release)
	// fail releases whatever has been acquired so far. Every early return below
	// goes through it, so a half-built session never leaves a file open.
	fail := func(err error) (*Session, error) {
		for i := len(s.closers) - 1; i >= 0; i-- {
			_ = s.closers[i]()
		}
		return nil, err
	}

	set, err := tools.NewSet(abs)
	if err != nil {
		// Through fail, not a bare return: the lock is already on the closer
		// stack, and a session that could not open its tool root must not leave
		// the working tree locked against the next attempt.
		return fail(fmt.Errorf("engine: opening the tool root at %s: %w", abs, err))
	}
	s.closers = append(s.closers, set.Close)

	policy, err := gatePolicy(opts, set.Root.Resolver())
	if err != nil {
		return fail(fmt.Errorf("engine: building the consent policy: %w", err))
	}
	gate, err := permission.New(set.Root.Path(), set.Root.Resolver(), policy)
	if err != nil {
		return fail(fmt.Errorf("engine: building the consent gate: %w", err))
	}

	// A session outside a repository is legitimate — no snapshots, no head on
	// the record — so this failure is not fatal for Open. What is not
	// legitimate is a repository whose state directory could not be excluded,
	// because that is the promise never to touch the user's git state
	// (ADR-0002 §3). For Fork, outside a repository *is* fatal — see
	// openRepo's own comment on why.
	head, snaps, err := openRepo(ctx, abs, id, now, opts.Snapshots, opts.Resume, fork)
	if err != nil {
		return fail(err)
	}

	jrn, err := journal.Open(abs, id, journal.WithClock(now))
	if err != nil {
		return fail(fmt.Errorf("engine: opening the session record: %w", err))
	}
	s.closers = append(s.closers, jrn.Close)

	// Read back before anything new is appended to jrn — in particular before
	// Start below writes this Open call's own SessionStarted — so "history"
	// means exactly the resumed session's prior record and nothing this
	// invocation is about to add to it. jrn.Read rehydrates blob-spilled
	// fields (unlike journal.ReadSession, which does not know this journal's
	// blob store), which matters here: the assembler needs a tool result's
	// actual content, not just that it was large.
	var history []journal.Event
	// forkedEvents is history again, kept apart because it is also what
	// recordFork duplicates into jrn *after* Start below — the assembler's
	// copy and the journal's copy of a fork's history come from one read,
	// not two, so they cannot disagree about what was found.
	var forkedEvents []journal.Event
	switch {
	case fork != nil:
		var srcMaxTurn int
		var rerr error
		forkedEvents, srcMaxTurn, rerr = readForkSource(ctx, abs, *fork)
		if rerr != nil {
			return fail(rerr)
		}
		if fork.Turn > srcMaxTurn {
			return fail(fmt.Errorf("%w: Options.Fork.Turn is %d, but session %s's own record only "+
				"reached turn %d", ErrConfig, fork.Turn, fork.SessionID, srcMaxTurn))
		}
		history = forkedEvents
	case opts.Resume:
		for ev, rerr := range jrn.Read(ctx) {
			if rerr != nil {
				return fail(fmt.Errorf("engine: replaying session %s's prior history: %w", id, rerr))
			}
			history = append(history, ev)
		}
	}

	// The teed journal exists before the provider does, and deliberately: a
	// live client's retry observer is wired to it below, so a retry the client
	// makes during the session reaches Options.Events exactly like any other
	// event, through the one journal the session has rather than a second copy
	// of it.
	teed := tee(jrn, opts.Events)

	prov, err := openProvider(opts, teed)
	if err != nil {
		return fail(err)
	}

	askFn, askSource := mustAnswerer(opts.Ask, opts.AskMode)

	eng, err := New(Config{
		SessionID: id,
		Selection: opts.Selection,
		// One binary, one identity, one call. The surface prints this for
		// --version and the record carries it on SessionStarted, and a field
		// here would be a second answer to who this is (ADR-0007 decision 7).
		Build:       buildInfo(),
		CWD:         abs,
		RepoHead:    head,
		Provider:    prov,
		Journal:     teed,
		Tools:       set,
		Permissions: gate,
		Ask:         askFn,
		AskSource:   askSource,
		Syntax:      &syntax.Gate{Root: set.Root.Path()},
		// Verify is left nil on purpose: nil means the default verifier, and
		// Options offers no way to say otherwise. See Options.
		Snapshots:     snaps,
		Stream:        streamTo(opts.Events),
		ResumeHistory: history,
	})
	if err != nil {
		return fail(err)
	}
	s.engine = eng

	if err := eng.Start(ctx); err != nil {
		return fail(err)
	}

	if fork != nil {
		// After Start, deliberately: this session's own SessionStarted (seq 1)
		// must land before the SessionForked marker and the copied block that
		// follows it, so a reader of this journal alone meets "this session
		// started" before "and here is what it started from" — see
		// journal.SessionForked's own doc comment.
		if err := eng.recordFork(ctx, *fork, forkedEvents); err != nil {
			return fail(err)
		}
	}
	return s, nil
}

// streamTo turns the observer into the per-delta callback [Config.Stream]
// wants, so live text and the record arrive on one stream in one order.
func streamTo(obs Observer) func(int, provider.Delta) {
	if obs == nil {
		return nil
	}
	return func(turn int, d provider.Delta) { obs(deltaEvent(turn, d)) }
}

// gatePolicy resolves what answers the permission gate for this session: the
// ADR-0011 declared allowlist built from [Options.Policy] when it is set, or
// [mustAskPolicy]'s Consent/ConsentMode path otherwise (KAN-885's existing
// behaviour, unchanged). openSession's own early refusal already guarantees
// the two are never both configured by the time this runs, so this function
// itself never has to choose between them — it only has to pick the one
// field that is actually set.
//
// resolver is [tools.Root.Resolver] — the one built for this session's own
// tool root a few lines above this call in openSession — which is exactly
// why this cannot happen inside [Options] validation any earlier: nothing
// resolves paths for this session until the tool root exists.
func gatePolicy(opts Options, resolver permission.Resolver) (permission.Policy, error) {
	if opts.Policy == nil {
		return mustAskPolicy(opts.Consent, opts.ConsentMode), nil
	}
	return permission.NewAllowlist(opts.Policy.Root, resolver, opts.Policy.Allow)
}

// mustAskPolicy builds the policy that forwards to Consent, attributing every
// answer per mode (KAN-885). permission.NewAsk refuses a nil asker, and this
// one is never nil: a nil Consenter is handled inside, by denying and saying
// so, which keeps "nobody wired consent" distinct from "consent was refused"
// on the record.
//
// A nil Consenter under [ConsentUnattended] is handled differently again, and
// on purpose (KAN-953's self-drive PoC is why): that combination names
// exactly one real case — a headless surface with no answerer wired, which is
// `kopicode run --print`'s ordinary state, not a caller that forgot to wire
// consent. [permission.NewUnattendedDeny] answers that case through the
// ordinary VerdictDeny path with a Reason specific enough not to invite a
// retry; the generic "no Consenter" refusal below answers through an error
// instead, which never reaches the journal as a PermissionDecided and is the
// right shape only for [ConsentInteractive] — a caller that left Consent nil
// there is a misconfigured front end, and that failure mode is what the
// generic message is for.
func mustAskPolicy(c Consenter, mode ConsentMode) permission.Policy {
	if c == nil && mode == ConsentUnattended {
		return permission.NewUnattendedDeny()
	}
	source := permission.SourceUser
	if mode == ConsentUnattended {
		source = permission.SourcePolicy
	}
	p, err := permission.NewAsk(asker{consent: c}, source)
	if err != nil {
		// Unreachable: asker{} is a non-nil value whatever c is, and source is
		// always one of the two permission.NewAsk accepts. A panic rather than
		// a swallowed error, because the alternative would be a session
		// running with no policy at all.
		panic("engine: building the consent policy: " + err.Error())
	}
	return p
}

// mustAnswerer resolves Options.Ask/Options.AskMode into what Config.Ask and
// Config.AskSource need, mirroring mustAskPolicy's shape for the identical
// reason (docs/adr/0009-ask-tool-contract.md decision 2): the source is
// derived purely from mode, independent of whether a is nil, the same way
// mustAskPolicy's source never depends on whether c is nil. A nil a is never
// left nil for Engine.runAsk to call and panic on — noAnswerer takes its
// place — and [New] would default it again anyway if this function's
// caller were ever bypassed, but Open always calls it, so that fallback is
// belt and suspenders rather than the only guard.
func mustAnswerer(a Answerer, mode AskMode) (Answerer, string) {
	source := askSourceUser
	if mode == AskUnattended {
		source = askSourcePolicy
	}
	if a == nil {
		a = noAnswerer
	}
	return a, source
}

// requireProviderCredential fails fast when the live client Open would build
// has nothing to authenticate with, without building anything.
//
// It exists apart from openProvider because ADR-0007 decision 4's ordering
// applies to this refusal exactly as it does to an unknown model — before the
// lock, before the journal — and openProvider itself cannot run that early:
// building the live client needs the journal open, so its retry observer has
// something to append to (KAN-851).
func requireProviderCredential(opts Options) error {
	if opts.Provider != nil {
		return nil
	}
	if os.Getenv(APIKeyEnv) == "" {
		return ErrNoAPIKey
	}
	return nil
}

// openProvider returns the provider the session will call.
//
// jrn is the session's own journal — the same value Config.Journal holds —
// and it exists only so the live client's retry observer can be wired to it
// before the client is built. openProvider never appends to jrn itself; it
// hands it to [journalRetryObserver], and only a retry the client makes later,
// during Session.Run, ever reaches it.
func openProvider(opts Options, jrn journal.Journal) (Provider, error) {
	if opts.Provider != nil {
		// A caller substituted its own Provider — the mock/replay path in
		// tests, or the bench runner's recorded traffic. No endpoint to name,
		// so the model id is the only fact worth echoing.
		slog.Debug("provider supplied by caller", "model", opts.Selection.ModelID)
		return opts.Provider, nil
	}

	key := os.Getenv(APIKeyEnv)
	if key == "" {
		// Unreachable through Open: requireProviderCredential already refused
		// this above. Kept so a direct caller of this function fails the same
		// way rather than building a client with an empty key.
		return nil, ErrNoAPIKey
	}

	baseURL := provider.DefaultBaseURL
	clientOpts := []provider.ClientOption{
		provider.WithRetryObserver(journalRetryObserver(jrn)),
		// The client's own attempt/status/backoff diagnostics (KAN-776's
		// WithLogger) reach the same sink as everything else in this file —
		// engine-internal, never a message, a reply or a tool call.
		provider.WithLogger(slog.Default()),
	}
	if opts.ProviderBaseURL != "" {
		baseURL = opts.ProviderBaseURL
		clientOpts = append(clientOpts, provider.WithBaseURL(opts.ProviderBaseURL))
	}
	// The endpoint and the model, never the key: provider.APIKey redacts
	// itself through every rendering path including LogValue (KAN-776), but
	// the safer rule is not to hand it to slog at all when a call site has
	// nothing to gain from trying.
	slog.Debug("connecting to provider",
		"base_url", baseURL, "model", opts.Selection.ModelID, "pin", opts.Selection.Pin.String())
	c, err := provider.NewClient(provider.NewAPIKey(key), clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("engine: building the provider client: %w", err)
	}
	return c, nil
}

// journalRetryObserver turns the live client's retry notifications into a
// journal event, wired at construction time and entirely outside
// engine.Provider's one method: Complete's return value is unaffected, and
// the loop learns about a retry storm the same way any other reader of the
// record would — by it having been journaled — rather than through anything
// Complete returns.
//
// The append error is deliberately swallowed rather than surfaced anywhere:
// this callback runs inside provider.Client.Complete, which has no channel
// back to the engine and — being outside engine.Provider on purpose — must
// not gain one just to report a broken journal. A journal that cannot accept
// an append is not going to accept the next one either, and the very next
// event this session writes (the ProviderResponse or ProviderRequest either
// side of this retry) goes through Engine.append, which does surface that
// failure and end the exchange with StopHarnessError. Nothing is lost that
// would not already be caught one event later.
func journalRetryObserver(jrn journal.Journal) provider.RetryObserver {
	return func(ctx context.Context, ev provider.RetryEvent) {
		_, _ = jrn.Append(ctx, ev.Turn, journal.ProviderRetried{
			Attempt: ev.Attempt,
			Try:     ev.Try,
			OfTries: ev.OfTries,
			DelayMS: ev.Delay.Milliseconds(),
			Cause:   ev.Cause,
		})
	}
}

// openRepo reports the commit HEAD points at and the snapshotter for the
// session, or zero values outside a repository.
//
// A directory that is not a repository is not an error **for Open**:
// SLICE-1 records RepoHead as "" there and writes no TurnSnapshot events. A
// directory that *is* one and whose exclude could not be written **is** an
// error, because the alternative is .kopicode/ showing up in the user's `git
// status` — the one thing ADR-0002 §3 promises will not happen.
//
// It **is** an error for Fork (fork != nil), and deliberately: a fork's
// source session's tree and shadow-ref chain live nowhere else, so a fork
// attempted outside a repository has nothing to restore and nothing to
// attach its own chain to — a silent no-op here would hand back a session
// whose conversation claims a tree state the working directory has no way of
// matching, which is exactly the failure [Fork]'s own doc comment argues
// automatic restore exists to prevent. [Fork] itself already refuses
// Options.Snapshots == SnapshotsOff before this is ever called, so the only
// way fork != nil reaches this function's "not a repository" branch is a
// caller pointing Dir somewhere git genuinely does not reach.
func openRepo(
	ctx context.Context, dir, sessionID string, now func() time.Time, mode SnapshotMode, resume bool,
	fork *ForkSource,
) (string, Snapshotter, error) {
	r, err := repo.Open(ctx, dir)
	if err != nil {
		if fork != nil {
			return "", nil, fmt.Errorf("engine: Options.Fork needs a git repository at %s, to read "+
				"session %s's tree and attach the new session's snapshot chain to it: %w",
				dir, fork.SessionID, err)
		}
		return "", nil, nil //nolint:nilerr // not a repository is a supported case, not a failure
	}

	if err := r.ExcludeStateDir(); err != nil {
		return "", nil, fmt.Errorf("engine: excluding %s from git: %w", repo.StateDir, err)
	}

	head, err := r.Head(ctx)
	if err != nil {
		// An unborn branch — a repository with no commit yet — has no head and
		// is an ordinary state to start a session in.
		head = ""
	}

	if mode == SnapshotsOff {
		// Unreachable with fork != nil: Fork refuses SnapshotsOff before this
		// function is ever called. The check stays absent here rather than
		// duplicated, because a duplicate that could never fire is a claim
		// this function does not actually enforce anything — Fork does, and
		// this comment is where a reader looks to confirm that.
		//
		// The head still goes on the record. It says which tree the session ran
		// against, which is true whether or not anybody is snapshotting.
		return head, nil, nil
	}

	if fork != nil {
		snaps, srcSnap, found, ferr := repo.NewForkingSnapshotter(
			ctx, r, sessionID, fork.SessionID, fork.Turn, repo.WithClock(now))
		if ferr != nil {
			return "", nil, fmt.Errorf("engine: preparing the forked session's snapshot chain: %w", ferr)
		}
		if found {
			// The restore target is r.Root(), not dir: a snapshot's tree
			// describes the *whole* work tree (Snapshotter.Snapshot always
			// runs `git add -A` at r.root, regardless of what directory a
			// session's own Dir named — see openRepo's callers), so archiving
			// that tree and extracting it anywhere other than r.Root() would
			// write the whole repository's layout into whatever subdirectory
			// dir happened to be.
			//
			// Cleared first, and unconditionally — see [Fork]'s own doc
			// comment for why forking has no "ordinary case" where skipping
			// this is safe the way resuming does, and
			// clearWorkingTreeForFork's own comment for why Restore alone is
			// not enough: it never removes a file the target tree does not
			// mention, and a fork is exactly the case that needs it to.
			if err := clearWorkingTreeForFork(r.Root()); err != nil {
				return "", nil, err
			}
			if err := r.Restore(ctx, srcSnap.Tree, r.Root()); err != nil {
				return "", nil, fmt.Errorf(
					"engine: restoring the working tree to session %s's turn %d before forking: %w",
					fork.SessionID, fork.Turn, err)
			}
		}
		// !found means sourceTurn is 0, or the source session never reached a
		// mutating turn: there is nothing recorded to restore, and snaps is
		// still a valid, parentless chain — the same starting state
		// NewSnapshotter gives any brand-new session.
		return head, snaps, nil
	}

	// resume decides which of repo's two constructors runs, and that choice is
	// the whole of KAN-939's snapshotter half: NewSnapshotter refuses a session
	// id that already has shadow refs (the right answer for two sessions that
	// collided on an id by accident), and NewResumingSnapshotter attaches to
	// them instead (the right answer when [Options.Resume] said this id is
	// being continued on purpose). Neither call is reachable from the other
	// branch, so a caller that did not ask to resume can never silently attach
	// to somebody else's chain.
	var snaps *repo.Snapshotter
	if resume {
		snaps, err = repo.NewResumingSnapshotter(ctx, r, sessionID, repo.WithClock(now))
	} else {
		snaps, err = repo.NewSnapshotter(ctx, r, sessionID, repo.WithClock(now))
	}
	if err != nil {
		return "", nil, fmt.Errorf("engine: preparing turn snapshots: %w", err)
	}
	return head, snaps, nil
}

// buildInfo maps this binary's identity onto the journal's declaration of it.
func buildInfo() journal.BuildInfo {
	b := build.Current()
	return journal.BuildInfo{
		Version:   b.Version,
		Commit:    b.Commit,
		TreeState: string(b.TreeState),
		Source:    b.Source,
	}
}

// newSessionID mints an identifier: a UTC timestamp a human can sort, and four
// random bytes so two sessions started in the same second are still distinct.
//
// The timestamp is first because the ids are directory names under
// .kopicode/sessions and sorting them by name should sort them by time. The
// randomness is crypto/rand rather than math/rand because a predictable
// suffix in a directory name shared with other processes is a symlink race
// waiting to be written up, and four bytes of it costs nothing.
func newSessionID(at time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform; if it ever
		// does, a collision-prone id is still better than refusing to start.
		return at.UTC().Format("20060102T150405Z")
	}
	return at.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}
