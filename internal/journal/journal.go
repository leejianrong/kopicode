package journal

import (
	"context"
	"iter"
	"time"
)

// Session identifies the session a Journal records.
type Session struct {
	// ID is the session identifier, injected rather than generated inside the
	// engine so a replay can reproduce a journal byte for byte.
	ID string
	// StartedAt is when the session opened, from the engine's clock.
	StartedAt time.Time
	// Dir is the session directory, .kopicode/sessions/<id>.
	Dir string
}

// Journal is the session record, behind an interface so a different
// implementation can be written later without the engine noticing
// (docs/adr/0002-no-durable-runtime-own-journal.md decision 4).
//
// FileJournal is the implementation: JSONL, one event per line, appended and
// fsynced. The 64 KiB blob spill is the card after it.
type Journal interface {
	// Append stamps the envelope (schema version, session, the next seq, the
	// time) and records payload durably. It returns the event as written, so a
	// caller never has to guess what was recorded.
	Append(ctx context.Context, turn int, payload Payload) (Event, error)

	// Read yields every event in seq order. It is an iterator rather than a
	// slice because a journal is never truncated and a long session's record
	// does not have to fit in memory to be read.
	//
	// A malformed line yields a non-nil error and stops iteration; it is never
	// skipped silently.
	Read(ctx context.Context) iter.Seq2[Event, error]

	// Session reports which session this journal records.
	Session() Session
}
