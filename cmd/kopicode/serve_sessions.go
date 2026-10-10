package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// server.sessions and session.events (ADR-0030 decisions 3 and 4): what each open
// session is doing, and what it has recorded since a point the client remembers.
// Neither queues behind a turn: they exist to be asked while sessions are busy.
// server.sessions is read inline on the read loop, like session.usage; a replay
// reads the journal and so runs beside it.

// sessionRow is one session in server.sessions' result.
type sessionRow struct {
	Session string `json:"session"`
	Dir     string `json:"dir"`
	// State is idle, running, awaiting_consent, awaiting_answer or ended.
	State string `json:"state"`
	// Turn is the session-wide turn count so far.
	Turn int `json:"turn"`
	// LastEventAt is the time of the last recorded event, RFC 3339 in UTC;
	// absent when the session has recorded none in this process.
	LastEventAt string `json:"last_event_at,omitempty"`
	// Usage is what session.usage returns for the session.
	Usage *usageSummary `json:"usage"`
	// LastStop is the stop reason of the last finished turn; absent before one.
	LastStop string `json:"last_stop,omitempty"`
	// PendingRequest is the id of the consent.request or ask.request the session
	// is waiting on, present only in the two awaiting states.
	PendingRequest string `json:"pending_request,omitempty"`
}

type sessionsResult struct {
	Sessions []sessionRow `json:"sessions"`
}

// eventsParams / eventsResult are session.events' shapes. AfterSeq is the last
// sequence number the client already holds (0 for everything); Limit bounds the
// page and is optional.
type eventsParams struct {
	Session  string `json:"session"`
	AfterSeq uint64 `json:"after_seq"`
	Limit    int    `json:"limit"`
}

type eventsResult struct {
	Session string   `json:"session"`
	Events  []record `json:"events"`
	// LastSeq is the sequence number to pass as after_seq next time: the last
	// event returned, or the after_seq asked for when there were none.
	LastSeq uint64 `json:"last_seq"`
	// More is true when the record holds events past this page.
	More bool `json:"more"`
	// Problems lists events whose spilled text could not be read back.
	Problems []string `json:"problems,omitempty"`
}

// handleSessions answers server.sessions from the live session table.
func (s *server) handleSessions(req rpcRequest) {
	infos := s.mgr.Sessions()
	rows := make([]sessionRow, 0, len(infos))
	for _, in := range infos {
		row := sessionRow{
			Session:        in.ID,
			Dir:            in.Dir,
			State:          string(in.State),
			Turn:           in.Turn,
			Usage:          usageSummaryOf(in.Usage),
			LastStop:       in.LastStop,
			PendingRequest: in.Pending,
		}
		if !in.LastEvent.IsZero() {
			row.LastEventAt = in.LastEvent.UTC().Format(time.RFC3339Nano)
		}
		rows = append(rows, row)
	}
	s.writeResult(req.ID, sessionsResult{Sessions: rows})
}

// handleEvents answers session.events from the session's journal.
func (s *server) handleEvents(req rpcRequest) {
	var p eventsParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.writeError(req.ID, codeInvalidParams, fmt.Sprintf("session.events params: %v", err))
		return
	}
	if p.Session == "" {
		s.writeError(req.ID, codeInvalidParams, "session.events needs a session id")
		return
	}
	if p.Limit < 0 {
		s.writeError(req.ID, codeInvalidParams, "session.events limit cannot be negative")
		return
	}
	// A page reads the journal from its start, so a long session's is real work:
	// it runs off the read loop, which session.cancel must never wait behind.
	go func() {
		page, err := s.mgr.Events(context.Background(), p.Session, p.AfterSeq, p.Limit)
		if err != nil {
			s.writeSessionError(req.ID, err)
			return
		}
		res := eventsResult{Session: p.Session, Events: make([]record, 0, len(page.Events)),
			LastSeq: p.AfterSeq, More: page.More, Problems: page.Problems}
		for _, ev := range page.Events {
			res.Events = append(res.Events, recordOf(ev))
			res.LastSeq = ev.Seq
		}
		s.writeResult(req.ID, res)
	}()
}
