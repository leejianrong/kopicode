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
	SessionID string

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
	// This is KAN-885's fix: `run --print` has no terminal and no human, and
	// its Consent (see denyHeadless in cmd/kopicode) answers on its own, so it
	// sets this to [ConsentUnattended] and every resulting
	// journal.PermissionDecided is stamped permission.SourcePolicy instead of
	// permission.SourceUser. Without this field the engine had no way to tell
	// the two apart, and every headless denial was recorded as though a human
	// had made it.
	ConsentMode ConsentMode

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
	// at all, is the harness answering its own question — `run --print`'s
	// denyHeadless, for instance — and every PermissionDecided this session
	// journals is stamped permission.SourcePolicy instead. A caller sets this
	// exactly when Options.Consent has no human behind it; setting it while
	// Consent really does put the question to a person just misattributes in
	// the opposite direction.
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
func Open(ctx context.Context, opts Options) (*Session, error) {
	if opts.Selection.ModelID == "" {
		return nil, fmt.Errorf("%w: Options.Selection carries no model id; resolve the arm with "+
			"ResolveSelection before opening a session (ADR-0007 decision 4)", ErrConfig)
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

	gate, err := permission.New(set.Root.Path(), set.Root.Resolver(), mustAskPolicy(opts.Consent, opts.ConsentMode))
	if err != nil {
		return fail(fmt.Errorf("engine: building the consent gate: %w", err))
	}

	// A session outside a repository is legitimate — no snapshots, no head on
	// the record — so this failure is not fatal. What is not legitimate is a
	// repository whose state directory could not be excluded, because that is
	// the promise never to touch the user's git state (ADR-0002 §3).
	head, snaps, err := openRepo(ctx, abs, id, now, opts.Snapshots)
	if err != nil {
		return fail(err)
	}

	jrn, err := journal.Open(abs, id, journal.WithClock(now))
	if err != nil {
		return fail(fmt.Errorf("engine: opening the session record: %w", err))
	}
	s.closers = append(s.closers, jrn.Close)

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
		Syntax:      &syntax.Gate{Root: set.Root.Path()},
		// Verify is left nil on purpose: nil means the default verifier, and
		// Options offers no way to say otherwise. See Options.
		Snapshots: snaps,
		Stream:    streamTo(opts.Events),
	})
	if err != nil {
		return fail(err)
	}
	s.engine = eng

	if err := eng.Start(ctx); err != nil {
		return fail(err)
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

// mustAskPolicy builds the policy that forwards to Consent, attributing every
// answer per mode (KAN-885). permission.NewAsk refuses a nil asker, and this
// one is never nil: a nil Consenter is handled inside, by denying and saying
// so, which keeps "nobody wired consent" distinct from "consent was refused"
// on the record.
func mustAskPolicy(c Consenter, mode ConsentMode) permission.Policy {
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
// A directory that is not a repository is not an error: SLICE-1 records
// RepoHead as "" there and writes no TurnSnapshot events. A directory that *is*
// one and whose exclude could not be written **is** an error, because the
// alternative is .kopicode/ showing up in the user's `git status` — the one
// thing ADR-0002 §3 promises will not happen.
func openRepo(ctx context.Context, dir, sessionID string, now func() time.Time, mode SnapshotMode) (string, Snapshotter, error) {
	r, err := repo.Open(ctx, dir)
	if err != nil {
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
		// The head still goes on the record. It says which tree the session ran
		// against, which is true whether or not anybody is snapshotting.
		return head, nil, nil
	}

	snaps, err := repo.NewSnapshotter(ctx, r, sessionID, repo.WithClock(now))
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
