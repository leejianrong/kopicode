package mock_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// This is the seam every engine test will inherit, so the file is written to
// fail on the two ways a seam like this rots.
//
// The first is a fixture nobody drives. Every test here discovers fixtures by
// walking the embedded set rather than from a list, counts what it drove, and
// fails on zero — the same shape internal/provider/fixture's own tests use, for
// the same reason: a check that iterates nothing reports green.
//
// The second is a replay that improvises. Asking for a reply the recording does
// not have must fail loudly, or a broken loop passes its test against a
// provider that made something up. Those cases are the second half of the file.

// satisfiesTheEngineInterface is the assertion that this package is what the
// loop will actually be handed. It lives here rather than in internal/engine
// because the interface belongs to its consumer and the implementation is what
// has to keep up — and it is a compile-time check, so a signature drift is a
// build failure rather than a test failure.
var _ engine.Provider = (*mock.Provider)(nil)

// minFixtures matches internal/provider/fixture's floor: one per extraction
// route, so no route is left undriven by the primary seam.
const minFixtures = 3

// fixedClock is the injected clock the journal checks use. Byte-identical
// output is only a meaningful claim when the timestamps are not free to move.
var fixedClock = time.Date(2026, time.August, 12, 9, 0, 0, 0, time.UTC)

const fixedSessionID = "kan773-replay"

func allFixtures(t *testing.T) []fixture.Fixture {
	t.Helper()
	all, err := fixture.LoadAll(fixture.FS())
	if err != nil {
		t.Fatalf("loading fixtures: %v", err)
	}
	if len(all) < minFixtures {
		t.Fatalf("loaded %d fixtures, want at least %d — a replay suite that drives nothing reports green",
			len(all), minFixtures)
	}
	return all
}

// request builds the request the loop would send for one exchange.
func request(f fixture.Fixture, ex fixture.Exchange) provider.Request {
	return provider.Request{
		ModelID: f.ModelID,
		Pin: provider.Pin{
			Order:          f.Pin.Order,
			AllowFallbacks: f.Pin.AllowFallbacks,
			Quantizations:  f.Pin.Quantizations,
		},
		Sampling: provider.Sampling{Temperature: 0.2, TopP: 0.95, MaxTokens: 4096},
		Messages: []provider.Message{{Role: provider.RoleUser, Content: "why does Greet(\"\") misbehave?"}},
		Turn:     ex.Turn,
		Attempt:  ex.Attempt,
	}
}

// replay pulls one exchange through the provider and returns the assembled
// reply along with the deltas it streamed.
func replay(t *testing.T, ctx context.Context, p *mock.Provider, req provider.Request) (provider.Reply, []provider.Delta) {
	t.Helper()

	s, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete(turn %d attempt %d): %v", req.Turn, req.Attempt, err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing the stream: %v", err)
		}
	}()

	var deltas []provider.Delta
	for s.Next() {
		deltas = append(deltas, s.Delta())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("streaming turn %d attempt %d: %v", req.Turn, req.Attempt, err)
	}
	reply, err := s.Reply()
	if err != nil {
		t.Fatalf("assembling turn %d attempt %d: %v", req.Turn, req.Attempt, err)
	}
	return reply, deltas
}

// TestEveryFixtureReplaysEndToEnd is the card's core claim: the recorded
// traffic goes through the provider and comes out as the reply the fixture says
// it is — checked against the fixture's own Expect block, which is itself held
// to the response body by fixture.Validate.
func TestEveryFixtureReplaysEndToEnd(t *testing.T) {
	replayed := 0

	for _, f := range allFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			p := mock.New(f)
			if p.Len() != len(f.Exchanges) {
				t.Fatalf("Len() = %d, want %d", p.Len(), len(f.Exchanges))
			}

			for i, ex := range f.Exchanges {
				reply, deltas := replay(t, t.Context(), p, request(f, ex))

				if reply.FinishReason != ex.Expect.FinishReason {
					t.Errorf("exchange %d: finish reason %q, fixture expects %q",
						i, reply.FinishReason, ex.Expect.FinishReason)
				}
				if reply.ServedBy != ex.Expect.ServedBy {
					t.Errorf("exchange %d: served by %q, fixture expects %q",
						i, reply.ServedBy, ex.Expect.ServedBy)
				}
				want := provider.Usage{
					Prompt:     ex.Expect.Usage.Prompt,
					Completion: ex.Expect.Usage.Completion,
					Total:      ex.Expect.Usage.Total,
				}
				if reply.Usage != want {
					t.Errorf("exchange %d: usage %+v, fixture expects %+v", i, reply.Usage, want)
				}
				if string(reply.Raw) != string(ex.Response.Body) {
					t.Errorf("exchange %d: Raw is not the recorded body verbatim, so the journal would "+
						"record a re-encoding rather than what the provider sent", i)
				}
				if ex.Response.Streamed() && reply.Content != "" && len(deltas) == 0 {
					t.Errorf("exchange %d: a streamed reply with content produced no deltas; the seam "+
						"would let every streaming bug through", i)
				}

				replayed++
			}

			if err := p.Drained(); err != nil {
				t.Errorf("after replaying every exchange: %v", err)
			}
			if p.Consumed() != p.Len() {
				t.Errorf("consumed %d of %d exchanges", p.Consumed(), p.Len())
			}
		})
	}

	if replayed == 0 {
		t.Fatal("replayed no exchanges")
	}
	t.Logf("replayed %d exchanges", replayed)
}

// TestReplayFeedsTheExtractor drives the real mapping from a reply onto
// parse.Message.
//
// This is what makes Reply.Message() more than a convenience: the route each
// fixture declares has to be the route the extractor takes when the reply comes
// through the provider, not when a test file destructures the body its own way.
// KAN-772 left a twenty-line mapping in its test marked "not the real one" —
// this is the real one, and it differs where it matters: arguments reach the
// extractor as the bytes that arrived, so parse.ArgEncoding still records how
// the model sent them.
func TestReplayFeedsTheExtractor(t *testing.T) {
	// covered is keyed by parse route wire name, so the completeness check
	// below enumerates the extractor rather than a list written here.
	covered := map[string]int{}
	checked := 0

	for _, f := range allFixtures(t) {
		p := mock.New(f)
		for i, ex := range f.Exchanges {
			reply, _ := replay(t, t.Context(), p, request(f, ex))
			ext, err := parse.Extract(reply.Message())

			if ex.Expect.Route == "" {
				if err == nil {
					t.Errorf("%s exchange %d: the fixture declares no route but extraction found %d call(s) via %s",
						f.Name, i, len(ext.Calls()), ext.Route())
				} else if !errors.Is(err, parse.ErrNoToolCall) {
					t.Errorf("%s exchange %d: the fixture declares no route but extraction failed with a "+
						"real error: %v", f.Name, i, err)
				}
				checked++
				continue
			}

			if err != nil {
				t.Errorf("%s exchange %d: fixture declares route %q but extraction failed: %v",
					f.Name, i, ex.Expect.Route, err)
				continue
			}
			if got := ext.Route().String(); got != ex.Expect.Route {
				t.Errorf("%s exchange %d: extraction took %q, fixture declares %q",
					f.Name, i, got, ex.Expect.Route)
			}
			var names []string
			for _, c := range ext.Calls() {
				names = append(names, c.Name)
			}
			if strings.Join(names, ",") != strings.Join(ex.Expect.Tools, ",") {
				t.Errorf("%s exchange %d: extraction found %v, fixture declares %v",
					f.Name, i, names, ex.Expect.Tools)
			}
			covered[ext.Route().String()]++
			checked++
		}
	}

	if checked == 0 {
		t.Fatal("checked no exchanges")
	}

	// parse.Route is a uint8, so this enumerates the whole space rather than a
	// list that can drift from the constants. A fourth extraction route fails
	// here until the replay seam drives it.
	routes := 0
	for i := range 256 {
		r := parse.Route(uint8(i))
		if !r.Valid() {
			continue
		}
		routes++
		if covered[r.String()] == 0 {
			t.Errorf("no replayed reply reaches the extractor by route %q\n"+
				"a route the primary seam never drives is a route the engine's tests never exercise",
				r.String())
		}
	}
	if routes == 0 {
		t.Fatal("enumerated no valid parse routes; this check would pass over anything")
	}
	t.Logf("drove %d exchanges over %d routes: %v", checked, routes, covered)
}

// TestReplayProducesAByteIdenticalJournalFragment is this card's half of the
// slice acceptance criterion.
//
// The criterion is a *session* replaying to a byte-identical journal, and the
// engine that would write one does not exist yet (KAN-789), so this proves what
// can be proved now: the events the provider is responsible for are byte for
// byte the same across repeated replays of one recording, given a fixed clock,
// a fixed session id and a fixed sequence. Everything else the criterion needs
// — turn ordering, tool results, snapshots — is the loop's to hold up, and this
// says nothing about it.
func TestReplayProducesAByteIdenticalJournalFragment(t *testing.T) {
	for _, f := range allFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			p := mock.New(f)

			first := journalFragment(t, p, f)
			p.Reset()
			second := journalFragment(t, p, f)

			if first != second {
				t.Fatalf("two replays of one recording produced different journals:\n%s\nvs\n%s", first, second)
			}
			// A second provider built from the same fixture, in case anything
			// stateful crept into construction.
			third := journalFragment(t, mock.New(f), f)
			if third != first {
				t.Fatalf("a fresh provider produced a different journal:\n%s\nvs\n%s", third, first)
			}
			if !strings.Contains(first, `"ProviderResponse"`) {
				t.Fatalf("the fragment carries no provider events; this check would pass over anything:\n%s", first)
			}
		})
	}
}

// journalFragment replays a whole fixture and renders the events the provider
// is responsible for, exactly as the journal would write them.
//
// It deliberately uses journal.Marshal rather than json.Marshal: the latter
// re-escapes HTML in whatever MarshalJSON returned and fills the record with
// escape noise, which would make this comparison pass over a bug in the bytes
// it is supposed to be checking.
func journalFragment(t *testing.T, p *mock.Provider, f fixture.Fixture) string {
	t.Helper()

	var (
		out strings.Builder
		seq uint64
	)
	emit := func(turn int, payload journal.Payload) {
		seq++
		line, err := journal.Marshal(journal.Event{
			SchemaVersion: journal.SchemaVersion,
			SessionID:     fixedSessionID,
			Seq:           seq,
			Turn:          turn,
			Time:          fixedClock.Add(time.Duration(seq) * time.Second),
			Payload:       payload,
		})
		if err != nil {
			t.Fatalf("marshalling event %d: %v", seq, err)
		}
		out.Write(line)
		out.WriteString("\n")
	}

	for _, ex := range f.Exchanges {
		req := request(f, ex)
		pin := journal.ProviderPin{
			Order:          req.Pin.Order,
			AllowFallbacks: req.Pin.AllowFallbacks,
			Quantizations:  req.Pin.Quantizations,
		}
		emit(ex.Turn, journal.ProviderRequest{
			ModelID: req.ModelID,
			Provider: journal.ProviderPin{
				Order:          pin.Order,
				AllowFallbacks: pin.AllowFallbacks,
				Quantizations:  pin.Quantizations,
			},
			Sampling: journal.Sampling{
				Temperature: req.Sampling.Temperature,
				TopP:        req.Sampling.TopP,
				MaxTokens:   req.Sampling.MaxTokens,
			},
			Attempt: req.Attempt,
		})

		reply, deltas := replay(t, t.Context(), p, req)
		emit(ex.Turn, journal.ProviderResponse{
			Body:         journal.InlineText(string(reply.Raw)),
			Tokens:       journal.TokenCounts(reply.Usage),
			FinishReason: reply.FinishReason,
			ServedBy:     reply.ServedBy,
		})

		// The deltas are folded in as well, because arrival order is exactly
		// what a streaming seam could get wrong without changing the assembled
		// reply at all.
		var streamed strings.Builder
		for _, d := range deltas {
			fmt.Fprintf(&streamed, "%s:%s|", d.Kind, d.Text)
		}
		emit(ex.Turn, journal.AssistantMessage{Text: journal.InlineText(streamed.String())})

		if ext, err := parse.Extract(reply.Message()); err == nil {
			for _, c := range ext.Calls() {
				// Note for KAN-789: parse.ArgEncoding says it exists "for the
				// journal", and journal.ToolCallParsed has nowhere to put it.
				// Whichever card wires extraction into the loop has to add the
				// field or drop the claim; the encoding a model used is a
				// per-model finding, so adding it looks right.
				emit(ex.Turn, journal.ToolCallParsed{
					CallID: c.ID,
					Tool:   c.Name,
					Args:   c.Arguments,
					Route:  ext.Route().String(),
				})
			}
		}
	}
	return out.String()
}

// TestExhaustionIsLoud. A replay asked for a turn the recording does not have
// is a bug in the test or in the loop. Serving a zero value, or looping back to
// the first reply, would make a loop that never terminates look correct.
func TestExhaustionIsLoud(t *testing.T) {
	f := allFixtures(t)[0]
	p := mock.New(f)

	for _, ex := range f.Exchanges {
		replay(t, t.Context(), p, request(f, ex))
	}

	last := f.Exchanges[len(f.Exchanges)-1]
	over := request(f, last)
	over.Turn, over.Attempt = last.Turn+1, 1

	s, err := p.Complete(t.Context(), over)
	if s != nil {
		t.Error("Complete returned a stream after the fixture ran out")
	}
	if !errors.Is(err, mock.ErrExhausted) {
		t.Fatalf("Complete after the last exchange returned %v, want ErrExhausted", err)
	}
	for _, want := range []string{f.Name, fmt.Sprintf("turn %d", over.Turn)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the exhaustion error does not name %q: %v", want, err)
		}
	}
}

func TestOutOfOrderRequestsAreRefused(t *testing.T) {
	f := allFixtures(t)[0]

	cases := []struct {
		name             string
		turn, attempt    int
		wantTurnInDetail bool
	}{
		{name: "a turn the fixture records later", turn: 2, attempt: 1, wantTurnInDetail: true},
		{name: "a repair attempt the fixture never recorded", turn: 1, attempt: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mock.New(f)
			req := request(f, f.Exchanges[0])
			req.Turn, req.Attempt = tc.turn, tc.attempt

			_, err := p.Complete(t.Context(), req)
			if !errors.Is(err, mock.ErrOutOfOrder) {
				t.Fatalf("Complete = %v, want ErrOutOfOrder", err)
			}
			if p.Consumed() != 0 {
				t.Errorf("consumed %d exchange(s) on a refused request; a refusal must not burn a reply",
					p.Consumed())
			}
		})
	}
}

// TestAnUnstatedPositionFallsBackToRecordedOrder. Turn and attempt are optional
// on the request: a test that does not care about the loop's position should
// not have to lie about it, and the recorded order is then the only key.
func TestAnUnstatedPositionFallsBackToRecordedOrder(t *testing.T) {
	f := allFixtures(t)[0]
	p := mock.New(f)

	for range f.Exchanges {
		req := request(f, f.Exchanges[0])
		req.Turn, req.Attempt = 0, 0
		replay(t, t.Context(), p, req)
	}
	if err := p.Drained(); err != nil {
		t.Fatalf("after replaying with no stated position: %v", err)
	}
}

// TestAnotherArmsRequestIsRefused. A result served outside the declared pin is
// discarded rather than adjusted (ADR-0005 §2); replaying one arm's recording
// under another arm's pin is the same error, made one step earlier.
func TestAnotherArmsRequestIsRefused(t *testing.T) {
	f := allFixtures(t)[0]

	t.Run("a different pin", func(t *testing.T) {
		p := mock.New(f)
		req := request(f, f.Exchanges[0])
		req.Pin.Order = []string{"some-other-provider"}

		_, err := p.Complete(t.Context(), req)
		if !errors.Is(err, mock.ErrPinMismatch) {
			t.Fatalf("Complete = %v, want ErrPinMismatch", err)
		}
		if !strings.Contains(err.Error(), "some-other-provider") {
			t.Errorf("the error does not name the pin that was asked for: %v", err)
		}
	})

	t.Run("fallbacks allowed", func(t *testing.T) {
		p := mock.New(f)
		req := request(f, f.Exchanges[0])
		req.Pin.AllowFallbacks = true

		if _, err := p.Complete(t.Context(), req); !errors.Is(err, mock.ErrPinMismatch) {
			t.Fatalf("Complete = %v, want ErrPinMismatch — a request that permits fallbacks is not the "+
				"request this traffic was recorded for", err)
		}
	})

	t.Run("a different model", func(t *testing.T) {
		p := mock.New(f)
		req := request(f, f.Exchanges[0])
		req.ModelID = "some/other-model"

		if _, err := p.Complete(t.Context(), req); !errors.Is(err, mock.ErrModelMismatch) {
			t.Fatalf("Complete = %v, want ErrModelMismatch", err)
		}
	})
}

// TestDrainedReportsWhatWasLeft. A session that ends with replies unconsumed is
// usually a test that stopped early — and a loop that bailed on turn one still
// satisfies every assertion about turn one.
func TestDrainedReportsWhatWasLeft(t *testing.T) {
	f := allFixtures(t)[0]
	p := mock.New(f)

	replay(t, t.Context(), p, request(f, f.Exchanges[0]))

	err := p.Drained()
	if !errors.Is(err, mock.ErrUnconsumed) {
		t.Fatalf("Drained() = %v, want ErrUnconsumed", err)
	}
	last := f.Exchanges[len(f.Exchanges)-1]
	if !strings.Contains(err.Error(), fmt.Sprintf("turn %d attempt %d", last.Turn, last.Attempt)) {
		t.Errorf("the error does not name the unserved turn: %v", err)
	}
	if !strings.Contains(err.Error(), last.Note) {
		t.Errorf("the error does not quote the exchange's note, so a failure does not say what went "+
			"unserved: %v", err)
	}
}

func TestCancellationBeforeTheRequest(t *testing.T) {
	f := allFixtures(t)[0]
	p := mock.New(f)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	s, err := p.Complete(ctx, request(f, f.Exchanges[0]))
	if s != nil {
		t.Error("Complete returned a stream on a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete = %v, want it to wrap context.Canceled", err)
	}
	if p.Consumed() != 0 {
		t.Errorf("a cancelled request consumed %d reply; a turn that never ran must not burn one",
			p.Consumed())
	}
}

// TestCancellationMidReply is Ctrl-C during a streaming answer, driven through
// the seam the engine will use rather than through a hand-built stream.
func TestCancellationMidReply(t *testing.T) {
	f := allFixtures(t)[0]
	// The terminal exchange is the one that streams prose in several chunks,
	// which is where a user actually reaches for Ctrl-C.
	ex := f.Exchanges[len(f.Exchanges)-1]

	p := mock.New(f)
	for _, before := range f.Exchanges[:len(f.Exchanges)-1] {
		replay(t, t.Context(), p, request(f, before))
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	s, err := p.Complete(ctx, request(f, ex))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("closing a cancelled stream: %v", err)
		}
	}()

	seen := 0
	for s.Next() {
		seen++
		cancel()
	}
	if seen != 1 {
		t.Fatalf("read %d deltas after cancelling on the first, want 1", seen)
	}
	if !errors.Is(s.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want it to wrap context.Canceled", s.Err())
	}
	if _, err := s.Reply(); !errors.Is(err, context.Canceled) {
		t.Errorf("Reply() = %v, want the cancellation — a cancelled reply is not a model answer", err)
	}
}

// TestConcurrentRequestsServeEachReplyOnce exists for the race detector.
//
// The loop is sequential, but the provider is reachable from a cancelling
// goroutine and this package will be embedded in tests nobody here writes. Two
// callers must never be handed the same recorded reply.
func TestConcurrentRequestsServeEachReplyOnce(t *testing.T) {
	f := allFixtures(t)[0]
	p := mock.New(f)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]int{}
	)
	for range len(f.Exchanges) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := request(f, f.Exchanges[0])
			req.Turn, req.Attempt = 0, 0

			s, err := p.Complete(t.Context(), req)
			if err != nil {
				t.Errorf("Complete: %v", err)
				return
			}
			defer func() { _ = s.Close() }()

			for s.Next() { //revive:disable-line:empty-block
			}
			reply, err := s.Reply()
			if err != nil {
				t.Errorf("Reply: %v", err)
				return
			}
			mu.Lock()
			ids[reply.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(ids) != len(f.Exchanges) {
		t.Fatalf("%d concurrent requests were served %d distinct replies: %v",
			len(f.Exchanges), len(ids), ids)
	}
	for id, n := range ids {
		if n != 1 {
			t.Errorf("reply %s was served %d times", id, n)
		}
	}
}

func TestLoadReportsAMissingFixture(t *testing.T) {
	if _, err := mock.Load("no_such_fixture"); !errors.Is(err, fixture.ErrNotFound) {
		t.Fatalf("Load of a missing fixture returned %v, want ErrNotFound", err)
	}
	p, err := mock.Load(allFixtures(t)[0].Name)
	if err != nil {
		t.Fatalf("Load of a real fixture: %v", err)
	}
	if p.Len() == 0 {
		t.Fatal("the loaded fixture holds no exchanges")
	}
	if p.ModelID() == "" || len(p.Pin().Order) == 0 {
		t.Fatalf("the loaded provider reports model %q and pin %s", p.ModelID(), p.Pin())
	}
}
