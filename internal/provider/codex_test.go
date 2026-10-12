package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/codexauth"
	"github.com/leejianrong/kopicode/internal/parse"
)

const (
	fakeToken   = "tok-FAKE-codex-0000"
	fakeAccount = "acct-FAKE-codex-1111"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeCreds builds credentials through the real reader, from a temp file, since
// codexauth.Secret cannot be built any other way.
func fakeCreds(t *testing.T) codexauth.Credentials {
	t.Helper()
	body := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"access_token":%q,"account_id":%q}}`, fakeToken, fakeAccount)
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := codexauth.Load(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func drainStream(t *testing.T, s *Stream) (content, reasoning string) {
	t.Helper()
	var c, r strings.Builder
	for s.Next() {
		d := s.Delta()
		if d.Kind == DeltaReasoning {
			r.WriteString(d.Text)
		} else {
			c.WriteString(d.Text)
		}
	}
	return c.String(), r.String()
}

func TestResponsesStreamDecodesARecordedTextTurn(t *testing.T) {
	raw := readFixture(t, "text_turn.sse")
	s := NewResponsesStream(context.Background(), strings.NewReader(string(raw)))
	content, _ := drainStream(t, s)
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
	if content != "ok" {
		t.Errorf("content = %q, want ok", content)
	}
	reply, err := s.Reply()
	if err != nil {
		t.Fatal(err)
	}
	if reply.Content != "ok" || reply.FinishReason != "stop" || len(reply.ToolCalls) != 0 {
		t.Errorf("reply = %+v", reply)
	}
	if reply.ModelID != "gpt-5.6-luna" || !strings.HasPrefix(reply.ID, "resp_") {
		t.Errorf("model %q id %q", reply.ModelID, reply.ID)
	}
	if reply.Usage.Prompt != 28 || reply.Usage.Completion != 5 || reply.Usage.Total != 33 {
		t.Errorf("usage = %+v, want the recorded 28/5/33", reply.Usage)
	}
	if reply.Cost != nil {
		t.Errorf("cost = %v; a subscription has no per-token price and none is invented", *reply.Cost)
	}
	if string(s.Transcript()) != string(raw) {
		t.Error("the transcript is not the recorded bytes verbatim")
	}
	if reply.Raw != nil {
		t.Error("a live stream has no assembled body; Raw must stay nil")
	}
}

func TestResponsesStreamDecodesARecordedToolCall(t *testing.T) {
	s := NewResponsesStream(context.Background(), strings.NewReader(string(readFixture(t, "tool_call.sse"))))
	drainStream(t, s)
	if s.Err() != nil {
		t.Fatal(s.Err())
	}
	reply, err := s.Reply()
	if err != nil {
		t.Fatal(err)
	}
	if reply.FinishReason != "tool_calls" || len(reply.ToolCalls) != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	tc := reply.ToolCalls[0]
	if tc.Name != "read_file" || !strings.HasPrefix(tc.ID, "call_") {
		t.Errorf("call = %+v", tc)
	}
	// The same encoding the chat-completions path hands the extractor: a JSON
	// string holding the arguments object.
	if string(tc.Arguments) != `"{\"path\":\"a.txt\"}"` {
		t.Errorf("arguments = %s", tc.Arguments)
	}
	msg := reply.Message()
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "read_file" {
		t.Errorf("message = %+v", msg)
	}
}

func TestResponsesStreamReportsATruncatedStream(t *testing.T) {
	raw := string(readFixture(t, "text_turn.sse"))
	cut := raw[:strings.Index(raw, "event: response.completed")]
	s := NewResponsesStream(context.Background(), strings.NewReader(cut))
	drainStream(t, s)
	if !errors.Is(s.Err(), ErrStreamIncomplete) {
		t.Errorf("err = %v, want ErrStreamIncomplete", s.Err())
	}
	if _, err := s.Reply(); err == nil {
		t.Error("a half-read stream produced a reply")
	}
}

func TestResponsesStreamReportsAnEmptyStreamAndGarbage(t *testing.T) {
	s := NewResponsesStream(context.Background(), strings.NewReader(""))
	drainStream(t, s)
	if !errors.Is(s.Err(), ErrStreamIncomplete) {
		t.Errorf("empty: err = %v", s.Err())
	}
	s = NewResponsesStream(context.Background(), strings.NewReader("<html>blocked</html>\n"))
	drainStream(t, s)
	if !errors.Is(s.Err(), ErrMalformedStream) {
		t.Errorf("html: err = %v, want ErrMalformedStream", s.Err())
	}
	s = NewResponsesStream(context.Background(), strings.NewReader("data: {not json\n"))
	drainStream(t, s)
	if !errors.Is(s.Err(), ErrMalformedStream) {
		t.Errorf("bad json: err = %v", s.Err())
	}
}

func TestResponsesStreamReportsABackendFailure(t *testing.T) {
	for name, body := range map[string]string{
		"response.failed": "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}}\n\n",
		"error event":     "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}\n\n",
	} {
		s := NewResponsesStream(context.Background(), strings.NewReader(body))
		drainStream(t, s)
		if !errors.Is(s.Err(), ErrBackendFailure) || !strings.Contains(s.Err().Error(), "rate_limit_exceeded") {
			t.Errorf("%s: err = %v", name, s.Err())
		}
	}
}

func TestResponsesStreamStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := NewResponsesStream(ctx, strings.NewReader(string(readFixture(t, "text_turn.sse"))))
	cancel()
	for s.Next() {
	}
	if !errors.Is(s.Err(), context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", s.Err())
	}
}

func TestResponsesIncompleteMapsToLength(t *testing.T) {
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n"
	s := NewResponsesStream(context.Background(), strings.NewReader(body))
	drainStream(t, s)
	reply, err := s.Reply()
	if err != nil || reply.FinishReason != "length" {
		t.Errorf("reply %+v err %v, want finish_reason length", reply, err)
	}
}

func TestCodexClientSendsTheRequestTheBackendAccepted(t *testing.T) {
	var gotPath, gotAuth, gotAccount, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccount = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header()["Content-Type"] = nil // as the live backend answered: no content type at all
		_, _ = w.Write(readFixture(t, "text_turn.sse"))
	}))
	defer srv.Close()

	c, err := NewCodexClient(fakeCreds(t), WithCodexBaseURL(srv.URL+"/backend-api/codex"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := c.Complete(context.Background(), Request{
		ModelID: "gpt-5.6-luna",
		Messages: []Message{
			{Role: RoleSystem, Content: "be brief"},
			{Role: RoleSystem, Content: "and kind"},
			{Role: RoleUser, Content: "hi"},
			{Role: RoleAssistant, Content: "hello"},
			{Role: RoleUser, Content: "again"},
		},
		Tools: []parse.Schema{{Name: "read_file", Description: "Read a file", Params: []parse.Param{{Name: "path", Type: parse.TypeString, Required: true}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainStream(t, s)
	_ = s.Close()

	if gotPath != "/backend-api/codex/responses" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer "+fakeToken || gotAccount != fakeAccount {
		t.Error("the credential headers are not what the backend takes")
	}
	var body struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Stream       bool   `json:"stream"`
		Store        bool   `json:"store"`
		Input        []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct{ Type, Text string }
		} `json:"input"`
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("the body is not JSON: %v\n%s", err, gotBody)
	}
	if body.Model != "gpt-5.6-luna" || body.Instructions != "be brief\n\nand kind" || !body.Stream || body.Store {
		t.Errorf("body header fields: %+v", body)
	}
	if len(body.Input) != 3 || body.Input[0].Role != "user" || body.Input[0].Content[0].Type != "input_text" ||
		body.Input[1].Role != "assistant" || body.Input[1].Content[0].Type != "output_text" {
		t.Errorf("input = %+v", body.Input)
	}
	if len(body.Tools) != 1 || body.Tools[0]["type"] != "function" || body.Tools[0]["name"] != "read_file" {
		t.Errorf("tools = %v", body.Tools)
	}
	for _, banned := range []string{`"temperature"`, `"provider"`, `"messages"`} {
		if strings.Contains(gotBody, banned) {
			t.Errorf("the body carries %s, which is not part of a Responses request", banned)
		}
	}
}

func TestCodexClientRefusesToolHistoryForNow(t *testing.T) {
	c, err := NewCodexClient(fakeCreds(t), WithCodexBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []Message{
		{Role: RoleTool, Content: "x", ToolCallID: "c"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c", Name: "n"}}},
	} {
		if _, err := c.Complete(context.Background(), Request{ModelID: "m", Messages: []Message{m}}); !errors.Is(err, ErrCodexToolHistory) {
			t.Errorf("err = %v, want ErrCodexToolHistory", err)
		}
	}
}

func TestCodexClientReturnsAnAPIErrorWhole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"quota"}`))
	}))
	defer srv.Close()
	c, _ := NewCodexClient(fakeCreds(t), WithCodexBaseURL(srv.URL))
	_, err := c.Complete(context.Background(), Request{ModelID: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	var api *APIError
	if !errors.As(err, &api) || api.Status != 429 || api.Body != `{"error":"quota"}` {
		t.Errorf("err = %v", err)
	}
}

func TestCodexClientRefusesAnHTMLChallengeAs200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>challenge</html>"))
	}))
	defer srv.Close()
	c, _ := NewCodexClient(fakeCreds(t), WithCodexBaseURL(srv.URL))
	_, err := c.Complete(context.Background(), Request{ModelID: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "not an event stream") {
		t.Errorf("err = %v", err)
	}
}

// The token must not reach an error, a log line or a rendering of the client.
func TestCodexCredentialNeverAppearsInErrorsOrRenderings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized"))
	}))
	url := srv.URL
	srv.Close() // connection refused: a transport error
	c, _ := NewCodexClient(fakeCreds(t), WithCodexBaseURL(url))
	_, err := c.Complete(context.Background(), Request{ModelID: "m", Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatal("no error from a closed server")
	}
	for _, out := range []string{err.Error(), c.String(), c.GoString(), fmt.Sprintf("%v %+v %#v", c, c, c)} {
		if strings.Contains(out, fakeToken) || strings.Contains(out, fakeAccount) {
			t.Errorf("a credential appeared in %q", out)
		}
	}
}

func TestNewCodexClientRefusesEmptyCredentials(t *testing.T) {
	if _, err := NewCodexClient(codexauth.Credentials{}); err == nil {
		t.Error("a client was built with no token")
	}
}
