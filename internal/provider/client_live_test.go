//go:build live

package provider_test

import (
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/provider"
)

// The one test in this repository that reaches openrouter.ai and spends money.
//
// It is behind the `live` build tag, which no make target in `ci` and no step in
// the pre-push hook sets, so `make test` and `make test-all` stay hermetic and
// free. Run it with:
//
//	make smoke-live
//
// It exists to answer questions the hand-authored fixtures cannot. Every fixture
// in internal/provider/fixture says `"origin": "hand_authored"` and the package
// doc is explicit that "what none of that can do is prove the shape is right.
// Only a recording can." Two shapes in particular are open:
//
//   - The response's top-level `provider` field, whose casing OpenRouter does
//     not document. docs/provider-pin.md says the pin's response name should be
//     `Parasail` and to compare case-insensitively; this test prints what
//     actually arrived so that file can record it rather than infer it.
//   - Whether provider.order accepts the suffixed slug form (`parasail/bf16`) on
//     a real completion. That is from the documentation, not from traffic.
//
// It cannot answer the third open question — the streaming tool-call delta
// shape — because Request carries no tool catalogue yet. Rendering one needs
// per-tool and per-argument descriptions parse.Schema does not hold, which is a
// decision about the model-facing contract rather than a transcription. Until
// that lands there is no way to make the provider emit a tool call, and the
// fixtures' claim that the deltas follow OpenAI's contract stays unverified.

// TestLiveCompletion sends one very small pinned request and reports what came
// back.
func TestLiveCompletion(t *testing.T) {
	key, err := provider.APIKeyFromEnv()
	if err != nil {
		t.Fatalf("%v — this test needs a real credential; that is why it is behind a build tag", err)
	}

	f := loadFixture(t)
	req := request(f)
	req.Sampling = provider.Sampling{Temperature: 0, TopP: 1, MaxTokens: 16}
	req.Messages = []provider.Message{
		{Role: provider.RoleUser, Content: `Reply with exactly the word "pong" and nothing else.`},
	}

	c, err := provider.NewClient(key)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	stream, err := c.Complete(t.Context(), req)
	if err != nil {
		t.Fatalf("Complete against the live provider: %v", err)
	}
	defer stream.Close() //nolint:errcheck // the assertions below cover the stream's own error.

	var text strings.Builder
	for stream.Next() {
		text.WriteString(stream.Delta().Text)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("draining the live stream: %v", err)
	}
	reply, err := stream.Reply()
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}

	t.Logf("model requested: %s", req.ModelID)
	t.Logf("model reported:  %s", reply.ModelID)
	t.Logf("provider field:  %q  <- record the exact casing in docs/provider-pin.md", reply.ServedBy)
	t.Logf("finish reason:   %q", reply.FinishReason)
	t.Logf("usage:           %+v", reply.Usage)
	t.Logf("content:         %q", text.String())
	t.Logf("wire transcript:\n%s", stream.Transcript())

	if reply.ServedBy == "" {
		t.Log("the response carried no provider field at all — that is itself a finding, " +
			"and ADR-0005 §2's discard check cannot rest on a field that is absent")
	}
	if text.Len() == 0 {
		t.Error("the live provider returned no content")
	}
}
