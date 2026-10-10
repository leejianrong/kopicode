package session

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
)

// State is what a session is doing now, as [Manager.Sessions] reports it
// (ADR-0030 decision 3). It is read from the live session table, never rebuilt
// from the journal: a session waiting on a client's answer has written nothing
// to the journal yet.
type State string

const (
	// StateIdle: open, no turn running, nothing queued that has started.
	StateIdle State = "idle"
	// StateRunning: a turn or a handoff draft is in flight.
	StateRunning State = "running"
	// StateAwaitingConsent: a turn is blocked on a consent.request the client
	// has not answered.
	StateAwaitingConsent State = "awaiting_consent"
	// StateAwaitingAnswer: a turn is blocked on an ask.request the client has
	// not answered.
	StateAwaitingAnswer State = "awaiting_answer"
	// StateEnded: the session has been closed. It stays listed, newest last and
	// bounded, so a client that reconnects can see how it finished.
	StateEnded State = "ended"
)

// Info is one row of the session table.
type Info struct {
	ID  string
	Dir string
	// State is one of the State values.
	State State
	// Turn is the session-wide turn count so far.
	Turn int
	// LastEvent is when the session last recorded an event; the zero time when
	// it has recorded none in this process.
	LastEvent time.Time
	// Usage is the session's usage now, as [Manager.Usage] reports it.
	Usage engine.Usage
	// LastStop is the stop reason the last finished turn settled on ("" before
	// the first one finishes).
	LastStop string
	// Pending is the id of the request the session is waiting on while it is
	// awaiting consent or an answer, and "" otherwise.
	Pending string
}

// Sessions lists every open session and the ended ones still remembered, sorted
// by id. It reads the live table and queues behind nothing, so it answers while
// every session is mid-turn.
func (m *Manager) Sessions() []Info {
	m.mu.Lock()
	open := make([]*managed, 0, len(m.sessions))
	rows := make([]Info, 0, len(m.sessions)+len(m.ended))
	for _, ms := range m.sessions {
		open = append(open, ms)
		rows = append(rows, Info{
			ID:       ms.id,
			Dir:      ms.dir,
			State:    ms.state(),
			LastStop: ms.lastStop,
			Pending:  ms.pending,
		})
	}
	rows = append(rows, m.ended...)
	m.mu.Unlock()

	// Usage and the turn count are the engine's own guarded numbers; read them
	// outside mu so the table lock never waits on the engine.
	byID := make(map[string]*managed, len(open))
	for _, ms := range open {
		byID[ms.id] = ms
	}
	for i := range rows {
		ms := byID[rows[i].ID]
		if ms == nil || rows[i].State == StateEnded {
			continue
		}
		rows[i].Turn = ms.sess.Turns()
		rows[i].Usage = ms.sess.Usage()
		if ns := ms.lastEvent.Load(); ns != 0 {
			rows[i].LastEvent = time.Unix(0, ns).UTC()
		}
	}
	slices.SortFunc(rows, func(a, b Info) int { return strings.Compare(a.ID, b.ID) })
	return rows
}

// state is the session's state; the caller holds m.mu.
func (ms *managed) state() State {
	switch {
	case ms.closed:
		return StateEnded
	case ms.awaiting != "":
		return ms.awaiting
	case ms.busy:
		return StateRunning
	default:
		return StateIdle
	}
}

// remember records an ended session's last state, once its worker is done with
// it, and drops the oldest past [endedKept].
func (m *Manager) remember(ms *managed) {
	info := Info{
		ID: ms.id, Dir: ms.dir, State: StateEnded,
		Turn: ms.sess.Turns(), Usage: ms.sess.Usage(),
	}
	if ns := ms.lastEvent.Load(); ns != 0 {
		info.LastEvent = time.Unix(0, ns).UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	info.LastStop = ms.lastStop
	m.ended = slices.DeleteFunc(m.ended, func(i Info) bool { return i.ID == ms.id })
	m.ended = append(m.ended, info)
	if over := len(m.ended) - endedKept; over > 0 {
		m.ended = m.ended[over:]
	}
}

// Awaiting marks a session as blocked on a question to its client: state is
// [StateAwaitingConsent] or [StateAwaitingAnswer], request the id the question
// went out under. The skin that sends the question calls it and defers the
// returned func, which clears the mark when the question is answered, times out
// or is abandoned. A session no longer open is ignored, and the returned func
// clears only the mark it set, so a later question is not wiped by an earlier
// one finishing.
func (m *Manager) Awaiting(id string, state State, request string) (clear func()) {
	m.mu.Lock()
	ms := m.sessions[id]
	if ms != nil {
		ms.awaiting, ms.pending = state, request
	}
	m.mu.Unlock()
	return func() {
		if ms == nil {
			return
		}
		m.mu.Lock()
		if ms.pending == request {
			ms.awaiting, ms.pending = "", ""
		}
		m.mu.Unlock()
	}
}

// Events replays session id's recorded events after the sequence number after,
// at most limit of them (see [engine.EventsAfter]). It serves an open session
// and one that has ended and is still remembered, and answers while a turn is
// running.
func (m *Manager) Events(ctx context.Context, id string, after uint64, limit int) (engine.EventPage, *Error) {
	m.mu.Lock()
	var dir string
	if ms := m.sessions[id]; ms != nil {
		dir = ms.dir
	} else {
		for _, e := range m.ended {
			if e.ID == id {
				dir = e.Dir
			}
		}
	}
	m.mu.Unlock()
	if dir == "" {
		return engine.EventPage{}, errf(KindUnknownSession, "no open session %q", id)
	}
	page, err := engine.EventsAfter(ctx, dir, id, after, limit)
	if err != nil {
		return engine.EventPage{}, errf(KindInternal, "%v", err)
	}
	return page, nil
}
