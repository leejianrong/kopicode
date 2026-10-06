package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/leejianrong/kopicode/internal/engine"
	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/provider"
	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// recordingProvider is the live client with a [fixture.Recorder] underneath it
// (KAN-774).
//
// The Recorder sits under the client as an http.RoundTripper, so the request
// the client builds, the pin it sends and the retry policy it applies are the
// ones a real session uses; the only thing added here is the engine's own
// bookkeeping. Turn and Attempt are not on the wire, so each Complete call
// attaches them to the context the Recorder reads them back from.
type recordingProvider struct {
	inner engine.Provider
	rec   *fixture.Recorder
	task  string
}

func (p recordingProvider) Complete(ctx context.Context, req provider.Request) (*provider.Stream, error) {
	note := fmt.Sprintf("Recorded from a live session on corpus task %s, turn %d attempt %d.",
		p.task, req.Turn, req.Attempt)
	return p.inner.Complete(fixture.WithMeta(ctx, req.Turn, req.Attempt, note), req)
}

// newRecordingClient builds the live client with a recorder wrapped around its
// transport. baseURL is "" for OpenRouter and only set by a test pointing at an
// httptest server.
func newRecordingClient(key provider.APIKey, task, baseURL string, opts ...provider.ClientOption) (recordingProvider, error) {
	rec := fixture.NewRecorder(nil)
	opts = append(opts, provider.WithHTTPClient(&http.Client{Transport: rec}))
	if baseURL != "" {
		opts = append(opts, provider.WithBaseURL(baseURL))
	}
	c, err := provider.NewClient(key, opts...)
	if err != nil {
		return recordingProvider{}, fmt.Errorf("bench: building the recording provider client: %w", err)
	}
	return recordingProvider{inner: c, rec: rec, task: task}, nil
}

// RecordedName is the fixture name a task's recording is filed under: the
// prefix, then the task id with every character a fixture name does not like
// folded to an underscore.
func RecordedName(prefix, taskID string) string {
	return prefix + strings.NewReplacer("-", "_", ".", "_").Replace(taskID)
}

// finish assembles what was recorded, validates it as any shipped fixture is
// validated, and writes it into dir. A recording that fails Validate is not
// written: a fixture the loader would refuse is worse than none.
//
// It must be called after the engine is closed, because a streamed reply is
// only complete once its body has been read to the end or closed.
func (p recordingProvider) finish(dir, name string, model string) (string, error) {
	f, err := p.rec.Fixture(name, fmt.Sprintf(
		"A live %s session on corpus task %s, recorded by `kopibench run --record-dir`.", model, p.task))
	if err != nil {
		return "", fmt.Errorf("bench: assembling the recording for %s: %w", p.task, err)
	}
	if err := fillExtractorFacts(&f); err != nil {
		return "", fmt.Errorf("bench: the recording for %s: %w", p.task, err)
	}
	if len(f.Exchanges) == 0 {
		return "", fmt.Errorf("bench: nothing was recorded for %s: the session made no request that got a reply", p.task)
	}
	if err := fixture.Validate(f); err != nil {
		return "", fmt.Errorf("bench: the recording for %s does not validate, so it was not written: %w", p.task, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("bench: creating the recording directory: %w", err)
	}
	path, err := fixture.Write(dir, f)
	if err != nil {
		return "", fmt.Errorf("bench: %w", err)
	}
	return filepath.Clean(path), nil
}

// fillExtractorFacts sets each exchange's route and tool names from what the real
// extractor makes of the reply.
//
// The recorder itself only knows a reply is native when the wire says so, and it
// cannot import internal/parse. But a model that writes its call as fenced JSON
// or an XML tag inside prose is the case the harness's repair routes exist for,
// and the shipped tests hold every fixture's declared route to the extractor's
// own answer. Left empty, a recording of exactly the interesting replies would
// claim they called nothing.
func fillExtractorFacts(f *fixture.Fixture) error {
	for i := range f.Exchanges {
		ex := &f.Exchanges[i]
		var body struct {
			Choices []struct {
				Message struct {
					Content   *string `json:"content"`
					ToolCalls []struct {
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(ex.Response.Body, &body); err != nil {
			return fmt.Errorf("exchange %d: decoding the body: %w", i, err)
		}
		if len(body.Choices) != 1 {
			continue // an error body or an empty reply: nothing was called
		}
		m := body.Choices[0].Message
		var msg parse.Message
		if m.Content != nil {
			msg.Content = *m.Content
		}
		for _, tc := range m.ToolCalls {
			enc, err := json.Marshal(tc.Function.Arguments)
			if err != nil {
				return fmt.Errorf("exchange %d: re-encoding arguments: %w", i, err)
			}
			msg.ToolCalls = append(msg.ToolCalls, parse.NativeCall{ID: tc.ID, Name: tc.Function.Name, Arguments: enc})
		}
		ext, err := parse.Extract(msg)
		if errors.Is(err, parse.ErrNoToolCall) {
			ex.Expect.Route, ex.Expect.Tools = "", nil
			continue
		}
		if err != nil {
			return fmt.Errorf("exchange %d: the extractor failed on a recorded reply: %w", i, err)
		}
		var names []string
		for _, c := range ext.Calls() {
			names = append(names, c.Name)
		}
		ex.Expect.Route, ex.Expect.Tools = ext.Route().String(), names
	}
	return nil
}
