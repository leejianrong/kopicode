package engine_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
	"github.com/leejianrong/kopicode/internal/provider/mock"
)

// TestLiveStreamJournalsTheWireTranscript drives the loop through the real
// OpenRouter client rather than the replay provider, because the two differ in
// exactly the place the record is written from.
//
// A recording carries an assembled body beside its frames, so provider.Reply.Raw
// is populated on replay and a loop that journaled Raw unconditionally looks
// perfectly correct in every other test in this package. A live streamed reply
// has no assembled body — OpenRouter never sends one — so Raw is nil, and that
// loop would record an empty provider response against the real client and
// nothing would notice until somebody read a real session's journal.
//
// The server here serves the shipped fixture's own frames, which fixture.Validate
// has already held to the assembled body beside them, so what is being tested is
// the loop's choice of source and not the frames.
func TestLiveStreamJournalsTheWireTranscript(t *testing.T) {
	fx, err := fixture.Load(fixture.FS(), "two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}

	var mu sync.Mutex
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		i := served
		served++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if i >= len(fx.Exchanges) {
			return
		}
		for _, line := range fx.Exchanges[i].Response.Stream {
			io.WriteString(w, line+"\n") //nolint:errcheck // a short write fails the assertions below.
		}
	}))
	t.Cleanup(srv.Close)

	client, err := provider.NewClient(
		provider.NewAPIKey("kopicode-fake-key-not-a-credential-0789"),
		provider.WithBaseURL(srv.URL),
		provider.WithHTTPClient(srv.Client()),
	)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	// The mock is here only to supply the arm identity the fixture was recorded
	// under; withProvider is what the loop actually drives.
	identity, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture provider: %v", err)
	}
	h := newHarness(t, identity, map[string]string{"internal/greet/greet.go": greetGo},
		withProvider(client), withMaxTurns(4))

	res, err := h.eng.Run(t.Context(), "why is it wrong?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stop != engine.StopCompleted {
		t.Fatalf("stop = %s, want %s", res.Stop, engine.StopCompleted)
	}

	responses := payloadsOf[journal.ProviderResponse](t, h.events())
	if len(responses) != 2 {
		t.Fatalf("got %d ProviderResponse events, want 2", len(responses))
	}
	for i, resp := range responses {
		body := resp.Body.Inline
		if body == "" {
			t.Fatalf("ProviderResponse %d recorded an empty body: a live streamed reply has no "+
				"assembled body, so the wire transcript is the only record of what arrived", i)
		}
		// A transcript, not a re-encoding: SSE framing survives verbatim.
		if missing, ok := containsAll(body, "data: ", "data: [DONE]"); !ok {
			t.Errorf("ProviderResponse %d does not carry the wire frames (%q missing):\n%s", i, missing, body)
		}
		if strings.HasPrefix(strings.TrimSpace(body), "{") {
			t.Errorf("ProviderResponse %d starts as an assembled JSON object, which the live path "+
				"never receives and must not synthesise:\n%s", i, body)
		}
		// The usage the provider reported still lands, decoded from the frames.
		if resp.Tokens.Total == 0 {
			t.Errorf("ProviderResponse %d reports no token usage", i)
		}
	}
}

// TestReplayJournalsTheAssembledBody is the other half of the pair, and the
// positive control for the test above: on the replay path Raw *is* populated,
// and the record must hold the recording's own bytes rather than a transcript.
func TestReplayJournalsTheAssembledBody(t *testing.T) {
	p, err := mock.Load("two_turn_native_tool_call")
	if err != nil {
		t.Fatalf("loading fixture: %v", err)
	}
	h := newHarness(t, p, map[string]string{"internal/greet/greet.go": greetGo})
	if _, err := h.eng.Run(t.Context(), "why is it wrong?"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for i, resp := range payloadsOf[journal.ProviderResponse](t, h.events()) {
		body := strings.TrimSpace(resp.Body.Inline)
		if !strings.HasPrefix(body, "{") {
			t.Errorf("ProviderResponse %d is not the recorded assembled body:\n%s", i, body)
		}
		if strings.Contains(body, "data: ") {
			t.Errorf("ProviderResponse %d carries SSE framing on the replay path, where an "+
				"assembled body exists and is what the recording holds:\n%s", i, body)
		}
	}
}
