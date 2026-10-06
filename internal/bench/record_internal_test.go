package bench

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/leejianrong/kopicode/internal/corpus"
	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// recordKey is a credential-shaped string the test sends and then searches the
// written fixture for. Self-describing, so a grep hit proves something, and
// unlike a real OpenRouter key it does not trip a secret scanner.
const recordKey = "kopicode-fake-key-not-a-credential-0002"

// TestRecordingWritesALoadableFixtureWithoutTheKey drives a whole session
// through EngineAgent with RecordDir set, against an httptest server that
// replays a shipped fixture's traffic, and holds the written file to the three
// things KAN-774 promises: it loads under the same validator as a shipped
// fixture, it is marked recorded, and the credential the request carried is not
// anywhere in it.
func TestRecordingWritesALoadableFixtureWithoutTheKey(t *testing.T) {
	src, err := fixture.Load(fixture.FS(), SmokeFixture)
	if err != nil {
		t.Fatalf("loading %s: %v", SmokeFixture, err)
	}

	var seenAuth atomic.Bool
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), recordKey) {
			seenAuth.Store(true)
		}
		i := int(served.Add(1)) - 1
		if i >= len(src.Exchanges) {
			http.Error(w, "out of exchanges", http.StatusInternalServerError)
			return
		}
		resp := src.Exchanges[i].Response
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(resp.Status)
		for _, line := range resp.Stream {
			_, _ = w.Write([]byte(line + "\n"))
		}
	}))
	defer srv.Close()

	t.Setenv("OPENROUTER_API_KEY", recordKey)
	sel, err := engine.ResolveSelection(t.TempDir(), engine.SelectionOverrides{})
	if err != nil {
		t.Fatalf("resolving the default selection: %v", err)
	}

	recDir := t.TempDir()
	agent := EngineAgent{Provider: ProviderLive, RecordDir: recDir, recordBaseURL: srv.URL}
	if _, err := agent.Run(t.Context(), SessionSpec{
		Task:      corpus.Task{ID: "go-record-me", Statement: "read internal/greet/greet.go and describe it"},
		Dir:       t.TempDir(),
		Home:      t.TempDir(),
		OutDir:    t.TempDir(),
		SessionID: "record-sess",
		Selection: sel,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !seenAuth.Load() {
		t.Fatal("the server never saw the credential, so this test cannot show the recorder drops it")
	}

	name := RecordedName("recorded_", "go-record-me")
	raw, err := os.ReadFile(filepath.Join(recDir, name+".json"))
	if err != nil {
		t.Fatalf("no recording was written: %v", err)
	}
	if strings.Contains(string(raw), recordKey) {
		t.Fatal("the recorded fixture contains the credential")
	}

	got, err := fixture.Load(os.DirFS(recDir), name)
	if err != nil {
		t.Fatalf("the recording does not load: %v", err)
	}
	if got.Origin != fixture.OriginRecorded {
		t.Errorf("origin = %q, want %q", got.Origin, fixture.OriginRecorded)
	}
	if len(got.Exchanges) != len(src.Exchanges) {
		t.Errorf("recorded %d exchanges, want %d", len(got.Exchanges), len(src.Exchanges))
	}
	for i, ex := range got.Exchanges {
		if ex.Turn != i+1 {
			t.Errorf("exchange %d filed as turn %d, want %d: Turn is the engine's bookkeeping and has to reach the recorder", i, ex.Turn, i+1)
		}
	}
}

func TestRecordDirRefusesAMockRun(t *testing.T) {
	_, err := RunCorpus(t.Context(), Options{
		CorpusDir: t.TempDir(), Provider: ProviderMock, RecordDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "live run") {
		t.Fatalf("err = %v, want a refusal naming that recording needs a live run", err)
	}
}

// TestFillExtractorFactsLabelsAFencedJSONCall: a reply whose tool call is fenced
// JSON in prose must be filed under the route the extractor takes, not left with
// an empty one, which is how the shipped tests would read "called nothing".
func TestFillExtractorFactsLabelsAFencedJSONCall(t *testing.T) {
	body := `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"Reading it.\n` + "```tool" + `\n{\"name\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n` + "```" + `"},` +
		`"finish_reason":"stop"}]}`
	f := fixture.Fixture{Exchanges: []fixture.Exchange{{Response: fixture.Response{Body: []byte(body)}}}}
	if err := fillExtractorFacts(&f); err != nil {
		t.Fatalf("fillExtractorFacts: %v", err)
	}
	got := f.Exchanges[0].Expect
	if got.Route != "fenced_json" || len(got.Tools) != 1 || got.Tools[0] != "read_file" {
		t.Errorf("expect = route %q tools %v, want fenced_json [read_file]", got.Route, got.Tools)
	}
}
