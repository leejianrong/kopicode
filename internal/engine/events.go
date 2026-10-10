package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/leejianrong/kopicode/internal/journal"
)

// DefaultEventLimit is how many events one [EventsAfter] call returns when its
// caller names no limit, and MaxEventLimit the most it will return however
// large a limit is asked for. The bound is on events, not bytes: a replayed
// event carries its whole text (nothing is truncated), so a page is as large as
// the events in it, and a client that wants less asks for fewer.
const (
	DefaultEventLimit = 500
	MaxEventLimit     = 5000
)

// EventPage is one page of a session's recorded events.
type EventPage struct {
	// Events are the recorded events after the requested sequence number, in
	// order, each with its blob-spilled text read back: unlike the event a
	// session announces as it happens (whose Text is empty and Size is the real
	// size for a value that spilled), a replayed event holds the whole value.
	Events []Event
	// More is true when events remain past the last one returned; ask again with
	// that event's Seq.
	More bool
	// Problems names each event whose spilled text could not be read back — a
	// blob that is missing or does not match its name. The event is still in
	// Events, with its Size and without its Text, so a reader sees what is gone
	// rather than an empty string that reads as "the tool said nothing".
	Problems []string
}

// EventsAfter reads the recorded events of session id, run in directory dir,
// whose sequence number is greater than after, at most limit of them (zero means
// [DefaultEventLimit]; more than [MaxEventLimit] is clamped).
//
// It reads the journal, the one record, and builds nothing of its own: the
// events are the ones the session announced, mapped the same way, so a client
// that missed some while disconnected sees exactly what it would have seen live.
// It never writes, takes no lock, and is safe while the session is running: an
// event whose line the writer has not finished is the end of what can be read
// now and arrives on the next call, not an error.
func EventsAfter(ctx context.Context, dir, id string, after uint64, limit int) (EventPage, error) {
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	limit = min(limit, MaxEventLimit)

	var page EventPage
	for ev, err := range journal.ReadSessionBlobs(ctx, dir, id) {
		if err != nil {
			switch {
			case errors.Is(err, journal.ErrTruncatedLine):
				// The writer is mid-line; what it is writing is not yet in the
				// record, and the next call reads it whole.
				return page, nil
			case errors.Is(err, journal.ErrBlobMissing), errors.Is(err, journal.ErrBlobCorrupt):
				if ev.Seq > after && len(page.Events) < limit {
					page.Problems = append(page.Problems, fmt.Sprintf("seq %d: %v", ev.Seq, err))
				}
			default:
				return page, fmt.Errorf("reading session %q: %w", id, err)
			}
		}
		if ev.Seq <= after {
			continue
		}
		if len(page.Events) == limit {
			page.More = true
			return page, nil
		}
		page.Events = append(page.Events, eventOf(ev))
	}
	return page, nil
}

// EventsAfter is the package function for this session's own record.
func (s *Session) EventsAfter(ctx context.Context, after uint64, limit int) (EventPage, error) {
	return EventsAfter(ctx, s.dir, s.id, after, limit)
}
