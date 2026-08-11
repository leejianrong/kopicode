// Package mock serves recorded provider traffic in place of a live provider.
//
// It is the primary test seam (docs/SLICE-1.md affordance P2): the engine is
// driven through its interface by this provider against a temp git-repo
// fixture, which is what makes the plumbing suite deterministic and free. Every
// reply it serves comes from internal/provider/fixture — it synthesises
// nothing, and a fixture that does not carry the reply being asked for is an
// error rather than an improvisation.
//
// # Replay level, and why this one
//
// A fixture carries each response twice over: the raw SSE lines as they arrive
// on the wire, and the assembled body. Either could be the thing replayed, and
// the choice is a real trade-off.
//
// Replaying at the *transport* — an httptest server or an http.RoundTripper
// handing back the recorded bytes — exercises the most real path: status codes,
// header handling, the HTTP body's own framing, and eventually the retry and
// backoff logic. It is also the only level that can test the client itself.
//
// Replaying at the *provider interface* — this package — couples the engine's
// primary seam to nothing but the provider's data types. No listener, no port,
// no accept loop, no goroutine that the race detector and the byte-identical
// journal criterion then have to be argued about.
//
// This card takes the interface level, and takes the SSE lines rather than the
// assembled body:
//
//   - The engine is what needs a seam now. The client it would drive at the
//     transport level does not exist yet (KAN-776), so a transport-level replay
//     today would be an HTTP server talking to nothing.
//   - Determinism is the acceptance bar. An HTTP round trip inside the primary
//     seam adds a listener, a second goroutine and a network buffer between the
//     fixture and the loop — three sources of ordering that the criterion would
//     then rest on rather than exclude.
//   - Serving the assembled body instead would be simpler still and would be
//     wrong: SLICE-1's provider streams, Ctrl-C has to cancel mid-reply, and a
//     mock that only ever returned a finished answer lets that whole class of
//     bug through the seam untested. So the fixture's recorded frames are
//     decoded by [provider.NewSSEStream] — the same decoder the real client
//     will use — keep-alive comments, split argument fragments, [DONE] sentinel
//     and all.
//
// Replaying the stream rather than the body is safe because fixture.Validate
// refuses any fixture whose frames do not fold back into the body beside them.
// The two representations are held equal at load time, so this package can take
// the one that exercises more of the path.
//
// KAN-776 should still add a transport-level replay for the *client's* own
// tests — the fixtures already carry what it needs, including status and the
// allowlisted response headers. That is a different seam for a different
// subject, not a replacement for this one.
//
// # Failing loudly
//
// A replay that improvises is worse than no replay: a broken engine test passes
// and the harness looks reliable. So every way of asking for something the
// fixture does not have is an error naming what was asked for and what the
// fixture holds — [ErrExhausted] when the replies run out, [ErrOutOfOrder] when
// the loop asks for a turn the fixture records elsewhere, [ErrPinMismatch] and
// [ErrModelMismatch] when the request belongs to a different arm. And a session
// that ends with replies unconsumed is usually a test that stopped early, which
// [Provider.Drained] reports rather than leaving silent.
package mock

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// Sentinel causes, for errors.Is. Every one of them means the same class of
// thing: the loop asked this provider for traffic the recording does not have,
// which is a bug in the test or in the loop and never something to paper over.
var (
	// ErrExhausted reports a request after the fixture's last reply.
	ErrExhausted = errors.New("mock provider: no recorded reply left")

	// ErrOutOfOrder reports a request whose turn and attempt do not match the
	// exchange the fixture records at this position.
	ErrOutOfOrder = errors.New("mock provider: request is not the one recorded here")

	// ErrPinMismatch reports a request whose provider pin differs from the one
	// the fixture was recorded under. A result taken under a different pin is
	// discarded rather than adjusted (ADR-0005 §2), and replaying one arm's
	// traffic under another arm's pin is the same mistake made earlier.
	ErrPinMismatch = errors.New("mock provider: request pin is not the fixture's pin")

	// ErrModelMismatch reports a request for a different model than the fixture
	// recorded.
	ErrModelMismatch = errors.New("mock provider: request model is not the fixture's model")

	// ErrUnconsumed reports replies left over at the end of a session.
	ErrUnconsumed = errors.New("mock provider: recorded replies were never consumed")
)

// Provider serves one fixture's exchanges, in recorded order.
//
// It is safe for concurrent use — the cursor is guarded — but a loop that calls
// it concurrently is not a loop this replay can check the ordering of, and the
// engine's is sequential. The mutex is there so the race detector has nothing
// to say about a provider shared with a cancelling goroutine, not to invite
// concurrent turns.
type Provider struct {
	fx fixture.Fixture

	mu     sync.Mutex
	cursor int
}

// New serves the given fixture.
func New(f fixture.Fixture) *Provider { return &Provider{fx: f} }

// Load reads a built-in fixture by name and serves it.
//
// The fixture is validated on the way in by fixture.Load, so a provider that
// exists is a provider whose traffic is internally consistent.
func Load(name string) (*Provider, error) {
	f, err := fixture.Load(fixture.FS(), name)
	if err != nil {
		return nil, fmt.Errorf("mock provider: %w", err)
	}
	return New(f), nil
}

// Fixture reports the traffic this provider serves.
func (p *Provider) Fixture() fixture.Fixture { return p.fx }

// ModelID is the model the fixture was recorded against.
func (p *Provider) ModelID() string { return p.fx.ModelID }

// Pin is the routing the fixture was recorded under, in the provider's terms.
func (p *Provider) Pin() provider.Pin {
	return provider.Pin{
		Order:          append([]string(nil), p.fx.Pin.Order...),
		AllowFallbacks: p.fx.Pin.AllowFallbacks,
		Quantizations:  append([]string(nil), p.fx.Pin.Quantizations...),
	}
}

// Complete serves the next recorded reply.
//
// It satisfies engine.Provider. The interface is not imported here: the engine
// declares what it consumes and this package satisfies it structurally, which
// is what keeps internal/provider free of any dependency on the loop. The
// assertion that the two still line up lives in this package's tests.
func (p *Provider) Complete(ctx context.Context, req provider.Request) (*provider.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		// A cancelled context never reaches a live provider either. Reporting
		// it here rather than opening a stream keeps a cancelled turn from
		// looking like a turn that got a reply.
		return nil, fmt.Errorf("mock provider: %w", err)
	}

	ex, err := p.take(req)
	if err != nil {
		return nil, err
	}

	if ex.Response.Status != 200 {
		return nil, &provider.APIError{
			Status: ex.Response.Status,
			Body:   string(ex.Response.Body),
		}
	}

	raw := append([]byte(nil), ex.Response.Body...)
	if !ex.Response.Streamed() {
		// A fixture recorded without streaming. Nothing shipped today is one,
		// and a recording made without stream=true would be, so the path exists
		// rather than panicking on the first one.
		return provider.NewBodyStream(ctx, raw), nil
	}

	// The frames are rejoined into the bytes they arrived as — blank separator
	// lines included — and decoded by the same reader the live client uses, so
	// the framing itself is part of what the seam drives. A recorded line list
	// that lost its trailing newline still ends a frame, which is why the
	// terminating newline is added rather than assumed.
	body := strings.NewReader(strings.Join(ex.Response.Stream, "\n") + "\n")
	return provider.NewSSEStream(ctx, body, raw), nil
}

// take advances the cursor and returns the exchange this request is owed, or an
// error naming exactly what disagreed.
func (p *Provider) take(req provider.Request) (fixture.Exchange, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cursor >= len(p.fx.Exchanges) {
		return fixture.Exchange{}, fmt.Errorf(
			"%w: fixture %q records %d exchange(s) and all of them have been served; "+
				"the loop asked for turn %d attempt %d\n"+
				"a replay that invented a reply here would let a loop that runs too long pass its test",
			ErrExhausted, p.fx.Name, len(p.fx.Exchanges), req.Turn, req.Attempt)
	}

	ex := p.fx.Exchanges[p.cursor]
	if err := p.check(req, ex); err != nil {
		return fixture.Exchange{}, err
	}

	p.cursor++
	return ex, nil
}

// check holds a request to the exchange about to answer it.
func (p *Provider) check(req provider.Request, ex fixture.Exchange) error {
	if req.ModelID != "" && req.ModelID != p.fx.ModelID {
		return fmt.Errorf("%w: the request asks for %q, fixture %q was recorded against %q",
			ErrModelMismatch, req.ModelID, p.fx.Name, p.fx.ModelID)
	}

	// An empty pin is the zero Request, which a test that does not care about
	// pinning will build. A *populated* pin that disagrees is the dangerous
	// case: it means the caller believes it is running one arm and is being
	// served another's traffic.
	if len(req.Pin.Order) > 0 && !req.Pin.Equal(p.Pin()) {
		return fmt.Errorf("%w: the request pins %s, fixture %q was recorded under %s\n"+
			"a result served outside the declared pin is discarded rather than adjusted "+
			"(docs/adr/0005-benchmark-and-ab-methodology.md §2), and replaying across pins is the "+
			"same mistake made earlier",
			ErrPinMismatch, req.Pin, p.fx.Name, p.Pin())
	}

	// Turn and attempt are optional on the request — zero means the caller did
	// not state its position, and strict recorded order is then the only key.
	// When they are stated they are checked, because a loop that skipped a turn
	// or re-sent one is exactly the bug a seam that served blindly would hide.
	if req.Turn == 0 && req.Attempt == 0 {
		return nil
	}
	if req.Turn != ex.Turn || req.Attempt != ex.Attempt {
		return fmt.Errorf(
			"%w: the loop asked for turn %d attempt %d, but fixture %q records turn %d attempt %d at "+
				"position %d (%s)",
			ErrOutOfOrder, req.Turn, req.Attempt, p.fx.Name, ex.Turn, ex.Attempt, p.cursor, ex.Note)
	}
	return nil
}

// Consumed reports how many exchanges have been served.
func (p *Provider) Consumed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursor
}

// Len reports how many exchanges the fixture holds.
func (p *Provider) Len() int { return len(p.fx.Exchanges) }

// Drained reports whether every recorded reply was consumed.
//
// A session that ends with replies left over is usually a test that stopped
// early — a loop that bailed on turn one still passes an assertion about turn
// one — so this is worth asserting at the end of a replay rather than trusting.
// The error names what was left, so a failure says which turn went unserved.
func (p *Provider) Drained() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cursor >= len(p.fx.Exchanges) {
		return nil
	}

	var left []string
	for _, ex := range p.fx.Exchanges[p.cursor:] {
		left = append(left, fmt.Sprintf("turn %d attempt %d (%s)", ex.Turn, ex.Attempt, ex.Note))
	}
	return fmt.Errorf("%w: fixture %q has %d of %d exchange(s) unserved: %s",
		ErrUnconsumed, p.fx.Name, len(left), len(p.fx.Exchanges), strings.Join(left, "; "))
}

// Reset returns the provider to the start of the fixture, so one recording can
// drive a replay twice. It exists for the determinism check — replay, replay
// again, compare the two — which needs the second run to start where the first
// did.
func (p *Provider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cursor = 0
}
