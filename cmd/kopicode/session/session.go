// Package session is the protocol-independent core of the resident front ends
// (ADR-0015 decision 3): `kopicode serve`'s JSON-RPC wire and `kopicode mcp`'s
// MCP server are two skins over this one session lifecycle, so the two cannot
// drift into independently-maintained implementations of it.
//
// It owns what has nothing to do with message framing: the registry of open
// sessions, the FIFO queue and worker goroutine behind each one, start / submit
// / cancel / close against engine.Open, the consent-mode resolution that decides
// who answers a permission request, and shutdown. It does not own how an
// outcome reaches a client. Results come back through callbacks the skin
// supplies, and a session's events through the engine.Observer the skin hands
// in, so this package imports nothing but the engine (ADR-0003's import
// allowlist is untouched) and holds no transcript of its own: the journal is
// still the one record.
//
// Every turn of one session runs on that session's single worker, in order;
// different sessions' workers run concurrently. A submit while a turn is in
// flight waits its place rather than failing.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// The consent_mode values a session may declare (ADR-0016 decision 3,
// ADR-0017). There is no default: the field is required, and a missing or
// unrecognised value is a usage error, not a fallback.
const (
	ConsentRemoteInteractive = "remote_interactive"
	ConsentUnattendedPolicy  = "unattended_policy"
	ConsentAuto              = "auto"
)

// Kind classifies an [Error] so a skin can map it onto its own vocabulary — a
// JSON-RPC error code, an MCP tool error — without parsing a message.
type Kind uint8

const (
	// KindUsage is the caller's own mistake: an unresolvable arm, a missing or
	// unrecognised consent_mode, a missing containment acknowledgment.
	KindUsage Kind = iota + 1
	// KindUnknownSession names an id with no open session.
	KindUnknownSession
	// KindSessionExists is a start for an id already open in this process.
	KindSessionExists
	// KindOpenFailed is engine.Open refusing: a bad model, a missing credential.
	KindOpenFailed
	// KindSessionLocked is a start whose working tree another live session holds.
	KindSessionLocked
	// KindInternal is a session that could not be closed cleanly.
	KindInternal
)

// Error is a refusal this package returns, carrying its [Kind].
type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

func errf(k Kind, format string, args ...any) *Error {
	return &Error{Kind: k, Message: fmt.Sprintf(format, args...)}
}

// StartParams is what opening a session takes.
type StartParams struct {
	ID            string
	Dir           string
	Prompt        string
	Model         string
	Harness       string
	HarnessConfig string

	// MaxTurns and TokenBudget override the harness limits for this session
	// (ADR-0022). Nil MaxTurns / TokenBudget mean not given.
	MaxTurns    *int
	TokenBudget *int

	// ConsentMode is required — one of the Consent* constants.
	ConsentMode string

	// ContainmentProvided must be true under ConsentUnattendedPolicy (ADR-0011
	// decision 4).
	ContainmentProvided bool

	// NeverAllow adds entries to ConsentAuto's never-allow list (ADR-0017);
	// supplying it under any other mode is a usage error.
	NeverAllow []string

	// AskMode is "" (today's behaviour: the process ask policy, else the fixed
	// refusal) or AskModeRemote (ADR-0020): the model's ask is put to the
	// client live. Only meaningful under ConsentRemoteInteractive.
	AskMode string

	// ReadOnly refuses every file write for the session (ADR-0019). It is a
	// usage error under ConsentAuto, which would run shell unasked.
	ReadOnly bool

	// ConsentTimeout overrides the process-wide live-consent timeout for this
	// session alone. Zero means "use the process default"; it is meaningful
	// only under ConsentRemoteInteractive, and the caller has already checked
	// its bounds.
	ConsentTimeout time.Duration
}

// Turn is a queued turn's callbacks. Begin, when set, is called from the
// session's worker immediately before the turn runs — a skin that routes events
// to whichever call is in flight uses it to learn which one that is. Done is
// called once, from the worker, when the turn settles.
type Turn struct {
	Begin func()
	Done  func(TurnResult)
}

// TurnResult is what a turn settled on, projected the way `run --print`'s last
// line is, so every surface reads an outcome identically. Record is set only on
// a session's opening turn, the one call that opened the journal.
type TurnResult struct {
	Session  string
	Record   string
	Stop     string
	ExitCode int
	Turns    int
	// Usage is the session's usage as the turn settled.
	Usage engine.Usage
}

// AskModeRemote is the one non-default StartParams.AskMode.
const AskModeRemote = "remote"

// RemoteAsk builds the live answerer for one session's ask tool (ADR-0020), with
// the same timeout meaning as [RemoteConsent]: zero is the process default.
type RemoteAsk func(sessionID string, timeout time.Duration) engine.Answerer

// RemoteConsent builds the live consenter for one session (ADR-0016), or is nil
// when the front end has no way to ask its peer — in which case a session
// declaring remote_interactive is refused rather than left to deny everything.
type RemoteConsent func(sessionID string, timeout time.Duration) engine.Consenter

// Manager is the resident state: the open sessions.
type Manager struct {
	ctx    context.Context
	base   engine.Options
	stderr io.Writer
	remote RemoteConsent
	// remoteAsk is nil for a front end with no live ask (kopicode mcp).
	remoteAsk RemoteAsk

	// mu guards the sessions map and every mutable field of a managed session —
	// its queue, its current-turn cancel handle and its closed flag — and backs
	// each session's cond. wg tracks the per-session worker goroutines so
	// Shutdown waits for them before closing anything.
	mu       sync.Mutex
	sessions map[string]*managed
	wg       sync.WaitGroup

	// startMu makes Start's duplicate-id check and its registration one atomic
	// step, so two concurrent starts for one id cannot both open a session.
	startMu sync.Mutex
}

// SetRemoteAsk gives the manager a live ask, called once after New and before
// any session starts.
func (m *Manager) SetRemoteAsk(f RemoteAsk) { m.remoteAsk = f }

// New builds a Manager. base carries what every session inherits (the declared
// policies, the provider base URL); Dir, Selection and the consent fields are
// per-session. remote may be nil.
func New(ctx context.Context, base engine.Options, stderr io.Writer, remote RemoteConsent) *Manager {
	return &Manager{ctx: ctx, base: base, stderr: stderr, remote: remote, sessions: map[string]*managed{}}
}

// managed is one open engine session and its queue of turns.
type managed struct {
	id   string
	sess *engine.Session

	cond    *sync.Cond
	queue   []job
	cancel  context.CancelFunc // the running turn's cancel, or nil between turns
	closed  bool               // shutdown asked this worker to drain and exit
	closing bool               // a Close is queued; no further turns are accepted
}

// job is one queued unit of work: a turn, or the end of the session.
type job struct {
	prompt    string
	isStart   bool
	turn      Turn
	isClose   bool
	closeDone func(*Error)
}

// Start opens a session and queues its first turn. engine.Open runs inline — it
// is a fast, local step — so the working-tree lock and its collision are decided
// before any worker exists; only after Open succeeds is the session registered,
// so a failed Open leaves nothing behind and its id stays free to retry.
//
// events is the session's observer (nil announces nothing). turn's callbacks run
// from the session's worker. A non-nil *Error means nothing was opened and
// neither callback will be called.
func (m *Manager) Start(p StartParams, events engine.Observer, turn Turn) *Error {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	m.mu.Lock()
	_, exists := m.sessions[p.ID]
	m.mu.Unlock()
	if exists {
		return errf(KindSessionExists, "session %q is already open in this process", p.ID)
	}

	selection, err := engine.ResolveSelection(p.Dir, engine.SelectionOverrides{
		Model:         p.Model,
		Harness:       p.Harness,
		HarnessConfig: p.HarnessConfig,
		MaxTurns:      p.MaxTurns,
		TokenBudget:   p.TokenBudget,
	})
	if err != nil {
		kind := KindOpenFailed
		if engine.IsSelectionUsageError(err) {
			kind = KindUsage
		}
		return &Error{Kind: kind, Message: err.Error()}
	}

	opts := m.base
	opts.Dir = p.Dir
	opts.SessionID = p.ID
	opts.Selection = selection
	opts.Events = events
	if rerr := m.buildConsentOptions(p, &opts); rerr != nil {
		return rerr
	}

	sess, err := engine.Open(m.ctx, opts)
	if err != nil {
		kind := KindOpenFailed
		if errors.Is(err, engine.ErrSessionLocked) {
			kind = KindSessionLocked
		}
		return &Error{Kind: kind, Message: err.Error()}
	}

	ms := &managed{id: p.ID, sess: sess}
	ms.cond = sync.NewCond(&m.mu)
	ms.queue = []job{{prompt: p.Prompt, isStart: true, turn: turn}}
	m.mu.Lock()
	m.sessions[p.ID] = ms
	m.mu.Unlock()

	m.wg.Add(1)
	go m.worker(ms)
	return nil
}

// Submit queues the next turn on an open session. It never runs the turn itself
// and never rejects a busy session. turn's callbacks run from the session's
// worker.
func (m *Manager) Submit(id, prompt string, turn Turn) *Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.sessions[id]
	if ms == nil {
		return errf(KindUnknownSession, "no open session %q; start one first", id)
	}
	if ms.closing {
		return errf(KindUnknownSession, "session %q is closing; start a new one", id)
	}
	ms.queue = append(ms.queue, job{prompt: prompt, turn: turn})
	ms.cond.Signal()
	return nil
}

// Cancel cancels the turn running now on a session, and only that one: a turn
// still queued keeps its place. A session with no turn in flight is a no-op,
// still acknowledged — the signal was delivered to the session either way.
func (m *Manager) Cancel(id string) *Error {
	m.mu.Lock()
	ms := m.sessions[id]
	var cancel context.CancelFunc
	if ms != nil {
		cancel = ms.cancel
	}
	m.mu.Unlock()

	if ms == nil {
		return errf(KindUnknownSession, "no open session %q to cancel", id)
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

// Usage reports an open session's usage now, including while a turn is
// running: the engine guards the numbers it reads for exactly this call.
func (m *Manager) Usage(id string) (engine.Usage, *Error) {
	m.mu.Lock()
	ms := m.sessions[id]
	m.mu.Unlock()
	if ms == nil {
		return engine.Usage{}, errf(KindUnknownSession, "no open session %q", id)
	}
	return ms.sess.Usage(), nil
}

// Close queues the end of a session behind the turns it has already accepted, so
// they finish and answer first. It never waits: the worker does the closing, so
// a slow turn ahead of it cannot stall the caller's read loop. done is called
// from the worker once SessionEnded is written and the tree lock is released.
func (m *Manager) Close(id string, done func(*Error)) *Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := m.sessions[id]
	if ms == nil || ms.closing {
		return errf(KindUnknownSession, "no open session %q to close", id)
	}
	ms.closing = true
	ms.queue = append(ms.queue, job{isClose: true, closeDone: done})
	ms.cond.Signal()
	return nil
}

// finishClose ends one session on its own worker. The session leaves the map
// first, under the lock, so a concurrent Shutdown cannot close it a second time
// and its id is reusable the moment SessionEnded is written.
func (m *Manager) finishClose(ms *managed, j job) {
	m.mu.Lock()
	delete(m.sessions, ms.id)
	ms.closed = true
	m.mu.Unlock()

	if err := ms.sess.Close(m.ctx); err != nil {
		_, _ = fmt.Fprintf(m.stderr, "kopicode: closing session %q: %v\n", ms.id, err)
		j.closeDone(errf(KindInternal, "closing session %q: %v", ms.id, err))
		return
	}
	j.closeDone(nil)
}

// worker drains one session's queue in order until the session is closed. Each
// turn gets its own cancellable context, installed as the session's current-turn
// cancel before it runs and cleared after, so a Cancel always finds the turn
// running now.
func (m *Manager) worker(ms *managed) {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		for len(ms.queue) == 0 && !ms.closed {
			ms.cond.Wait()
		}
		if ms.closed {
			// Shutdown: abandon any queued turns (their client has gone) and
			// exit. A turn already running finished above before we got here.
			ms.queue = nil
			m.mu.Unlock()
			return
		}
		j := ms.queue[0]
		ms.queue = ms.queue[1:]
		if j.isClose {
			m.mu.Unlock()
			m.finishClose(ms, j)
			return
		}
		turnCtx, cancel := context.WithCancel(m.ctx)
		ms.cancel = cancel
		m.mu.Unlock()

		if j.turn.Begin != nil {
			j.turn.Begin()
		}
		res, _ := ms.sess.Run(turnCtx, j.prompt)
		cancel()

		m.mu.Lock()
		ms.cancel = nil
		m.mu.Unlock()

		out := TurnResult{
			Session:  ms.id,
			Stop:     res.Stop.String(),
			ExitCode: res.Stop.ExitCode(),
			Turns:    res.Turns,
			Usage:    ms.sess.Usage(),
		}
		if j.isStart {
			out.Record = ms.sess.Path()
		}
		j.turn.Done(out)
	}
}

// Shutdown ends every open session once the client has gone: it marks each
// closed, cancels its in-flight turn, wakes its worker, waits for the workers to
// exit, then closes each session so its SessionEnded is written — the record's
// other bookend, owed even to a session that was mid-turn when the client left.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	for _, ms := range m.sessions {
		ms.closed = true
		if ms.cancel != nil {
			ms.cancel()
		}
		ms.cond.Signal()
	}
	m.mu.Unlock()

	m.wg.Wait()

	m.mu.Lock()
	open := make([]*managed, 0, len(m.sessions))
	for _, ms := range m.sessions {
		open = append(open, ms)
	}
	m.sessions = map[string]*managed{}
	m.mu.Unlock()

	for _, ms := range open {
		if ms.sess == nil {
			continue
		}
		if err := ms.sess.Close(m.ctx); err != nil {
			_, _ = fmt.Fprintf(m.stderr, "kopicode: closing session %q: %v\n", ms.id, err)
		}
	}
}

// --- consent modes (ADR-0016, ADR-0017) --------------------------------------

// buildConsentOptions resolves the required consent_mode into the Consent /
// ConsentMode fields engine.Open reads. Every refusal is a usage error decided
// before engine.Open is ever called, the same ordering ADR-0007 decision 4
// holds every other surface to.
//
// Ask/AskMode are wired identically under every mode — ADR-0016 reopens only the
// Consenter/permission half of consent, not the ask tool's, so a declared
// AskPolicy still wins where set and the fixed headless refusal answers
// otherwise.
func (m *Manager) buildConsentOptions(p StartParams, opts *engine.Options) *Error {
	if p.ReadOnly && p.ConsentMode == ConsentAuto {
		return errf(KindUsage, "read_only cannot be combined with consent_mode %q: auto runs shell inside the "+
			"root unasked, and a shell line can write, so the session would not be read-only (ADR-0019); "+
			"use %q or %q", ConsentAuto, ConsentRemoteInteractive, ConsentUnattendedPolicy)
	}
	opts.ReadOnly = p.ReadOnly
	if p.ConsentTimeout != 0 && p.ConsentMode != ConsentRemoteInteractive {
		// Nothing would consult it: only a live consent request waits on a
		// timeout. A caller who sent one is relying on it, so refuse rather than
		// ignore, the same rule never_allow is held to.
		return errf(KindUsage, "consent_timeout is only meaningful under consent_mode %q, which is the only "+
			"mode that waits for a live answer; got %q", ConsentRemoteInteractive, p.ConsentMode)
	}
	if len(p.NeverAllow) > 0 && p.ConsentMode != ConsentAuto {
		return errf(KindUsage, "never_allow only applies to consent_mode %q; this session declared %q "+
			"and nothing would consult the list", ConsentAuto, p.ConsentMode)
	}

	switch p.ConsentMode {
	case ConsentAuto:
		if err := engine.ValidateAutoNeverAllow(p.NeverAllow); err != nil {
			return errf(KindUsage, "never_allow: %v", err)
		}
		// The process-level --policy-file belongs to unattended_policy sessions
		// (ADR-0011) and stays out of this one: engine.Open refuses a Policy
		// beside ConsentAuto rather than guessing which answerer was meant.
		opts.Policy = nil
		opts.Consent = nil
		opts.ConsentMode = engine.ConsentAuto
		opts.AutoNeverAllow = p.NeverAllow

	case ConsentUnattendedPolicy:
		if !p.ContainmentProvided {
			return errf(KindUsage, "consent_mode is %q but containment_provided is not true; ADR-0011 "+
				"decision 4 requires the caller to explicitly acknowledge it is supplying real "+
				"process/container containment for this session", ConsentUnattendedPolicy)
		}
		// Policy (ADR-0011's declared allowlist) answers instead of Consent when
		// it is set on the base options; Consent stays nil either way. No human
		// is attributed a decision on either path (KAN-885).
		opts.ConsentMode = engine.ConsentUnattended

	case ConsentRemoteInteractive:
		if m.remote == nil {
			return errf(KindUsage, "consent_mode %q needs a way to ask the client, and this front end has "+
				"none; use %q or %q", ConsentRemoteInteractive, ConsentAuto, ConsentUnattendedPolicy)
		}
		// --policy-file belongs to unattended_policy sessions. Left set beside
		// the live consenter it makes engine.Open refuse the session as two
		// answerers for one question.
		opts.Policy = nil
		opts.Consent = m.remote(p.ID, p.ConsentTimeout)
		opts.ConsentMode = engine.ConsentRemote

	default:
		return errf(KindUsage, "a consent_mode of %q, %q or %q is required; got %q",
			ConsentRemoteInteractive, ConsentUnattendedPolicy, ConsentAuto, p.ConsentMode)
	}

	switch p.AskMode {
	case "":
	case AskModeRemote:
		if p.ConsentMode != ConsentRemoteInteractive {
			return errf(KindUsage, "ask_mode %q needs consent_mode %q: a session with no live client for "+
				"permissions has none for questions either (ADR-0020); got %q",
				AskModeRemote, ConsentRemoteInteractive, p.ConsentMode)
		}
		if m.remoteAsk == nil {
			return errf(KindUsage, "ask_mode %q needs a way to ask the client, and this front end has none", AskModeRemote)
		}
		// An explicit per-session opt-in outranks the process-wide --ask-policy-file,
		// the way remote_interactive outranks --policy-file: left set beside the live
		// answerer, Open would refuse the pair as two answerers for one question.
		opts.AskPolicy = nil
		opts.Ask = m.remoteAsk(p.ID, p.ConsentTimeout)
		opts.AskMode = engine.AskRemote
		return nil
	default:
		return errf(KindUsage, "ask_mode %q is not recognised; the only value is %q (or omit it)", p.AskMode, AskModeRemote)
	}

	if opts.AskPolicy == nil {
		// No declared ask policy: dead-end ask the way headless does, attributed
		// to the policy since nobody was asked (ADR-0009 decision 4).
		opts.Ask = DenyAsk
		opts.AskMode = engine.AskUnattended
	}
	// When AskPolicy is set, Ask must stay nil — Open refuses both at once — and
	// mustAnswerer attributes the answer to the policy on its own (KAN-1028).
	return nil
}

// DenyAsk answers every ask request by saying nobody is present — the same
// posture `run --print` takes (ADR-0009 decisions 2 and 4): there is nobody to
// ask, so the honest answer names that rather than inventing one, and it is not
// an error that ends the session.
func DenyAsk(context.Context, engine.AskRequest) (engine.AskAnswer, error) {
	return engine.AskAnswer{}, errors.New("no human is present to answer this question")
}
