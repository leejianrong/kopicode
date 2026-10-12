package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/leejianrong/kopicode/internal/codexauth"
)

// CodexBaseURL is the ChatGPT/Codex backend's API root (ADR-0035,
// docs/research/2026-10-codex-backend-verification.md).
const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// responsesPath is the Responses endpoint under the base URL.
const responsesPath = "/responses"

// ErrCodexToolHistory reports a request whose history holds a tool call or a tool
// result. Slice 1 of the Codex client (KOP-194) sends text turns only; the shape of
// a tool result going back as input is unverified and is slice 2 (KOP-197).
var ErrCodexToolHistory = errors.New("provider: the Codex client does not yet send tool calls or tool results in the history")

// CodexClient is the provider for a ChatGPT/Codex subscription (ADR-0035). It
// satisfies engine.Provider structurally, like [Client], and keeps the same
// promise about credentials: the bearer token is read in one place, the HTTP
// request builder, and nowhere else in this file.
//
// What it does not do yet, by slice: tool calls and tool results in the history
// (KOP-197), quota and expiry failure shapes (KOP-199), retries, and any wiring
// into the engine (KOP-198). A failed request is returned as an [APIError], whole,
// and not retried.
//
// Sampling is not sent. The live calls that worked sent none, and a reasoning
// model may refuse a temperature; whether it is accepted is not known.
type CodexClient struct {
	creds   codexauth.Credentials
	baseURL string
	http    *http.Client
}

// CodexOption configures a [CodexClient].
type CodexOption func(*CodexClient)

// WithCodexBaseURL points the client at another root, for tests.
func WithCodexBaseURL(u string) CodexOption {
	return func(c *CodexClient) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithCodexHTTPClient replaces the HTTP client, for tests.
func WithCodexHTTPClient(h *http.Client) CodexOption {
	return func(c *CodexClient) { c.http = h }
}

// NewCodexClient builds a client from a login read by [codexauth.Load].
func NewCodexClient(creds codexauth.Credentials, opts ...CodexOption) (*CodexClient, error) {
	if creds.AccessToken.IsZero() {
		return nil, errors.New("provider: the Codex credentials hold no access token")
	}
	c := &CodexClient{creds: creds, baseURL: CodexBaseURL, http: &http.Client{Transport: defaultTransport()}}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// String and GoString never include the credential.
func (c *CodexClient) String() string   { return "provider.CodexClient{" + c.baseURL + "}" }
func (c *CodexClient) GoString() string { return c.String() }

// CloseIdleConnections closes the keep-alive connections this client holds.
func (c *CodexClient) CloseIdleConnections() { c.http.CloseIdleConnections() }

// Complete sends one request and returns the reply as a stream.
func (c *CodexClient) Complete(ctx context.Context, req Request) (*Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := encodeResponses(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := c.newHTTPRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq) //nolint:bodyclose // closed by drain on a failure and by Stream.Close on success.
	if err != nil {
		// *url.Error names the method and URL and carries no headers.
		return nil, &TransportError{Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, drain(resp)
	}
	// The 200 observed live carried no content type at all, so an absent one is
	// not a refusal. A type that is present and is not an event stream (an HTML
	// challenge page, say) is, and is reported as such rather than as a malformed
	// stream; anything else goes to the decoder, which rejects a line that is not
	// an event, a data line, a comment or blank.
	if ct := resp.Header.Get("Content-Type"); ct != "" && !isEventStream(ct) {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("provider: the Codex backend answered 200 with content type %q, not an event stream", ct)
	}
	return NewResponsesStream(ctx, resp.Body), nil
}

// newHTTPRequest builds the request. This is the only place the credential is
// read.
func (c *CodexClient) newHTTPRequest(ctx context.Context, body []byte) (*http.Request, error) {
	url := c.baseURL + responsesPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider: building request for %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.creds.AccessToken.Reveal())
	if !c.creds.AccountID.IsZero() {
		req.Header.Set("ChatGPT-Account-ID", c.creds.AccountID.Reveal())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

// responsesRequest is the Responses-API request body, as observed to work.
type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        []responsesItem `json:"input"`
	Tools        []responsesTool `json:"tools,omitempty"`
	Stream       bool            `json:"stream"`
	Store        bool            `json:"store"`
}

type responsesItem struct {
	Type    string             `json:"type"`
	Role    string             `json:"role"`
	Content []responsesContent `json:"content"`
}

type responsesContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// responsesTool is a function tool: the same name, description and schema as the
// chat-completions definition, flattened one level.
type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  ToolParameters `json:"parameters"`
	Strict      bool           `json:"strict"`
}

// encodeResponses renders a [Request] as a Responses-API body.
//
// System messages become the top-level `instructions`, joined in order; the
// Responses API has no system role in `input`. The pin is ignored: a subscription
// has none, and the caller has already refused to treat the run as an arm.
func encodeResponses(req Request) ([]byte, error) {
	if req.ModelID == "" {
		return nil, errors.New("provider: request names no model")
	}
	w := responsesRequest{Model: req.ModelID, Stream: true, Store: false, Input: []responsesItem{}}

	var system []string
	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 || m.Role == RoleTool {
			return nil, ErrCodexToolHistory
		}
		switch m.Role {
		case RoleSystem:
			system = append(system, m.Content)
		case RoleUser:
			w.Input = append(w.Input, responsesItem{Type: "message", Role: "user",
				Content: []responsesContent{{Type: "input_text", Text: m.Content}}})
		case RoleAssistant:
			w.Input = append(w.Input, responsesItem{Type: "message", Role: "assistant",
				Content: []responsesContent{{Type: "output_text", Text: m.Content}}})
		default:
			return nil, fmt.Errorf("provider: unknown message role %q", m.Role)
		}
	}
	w.Instructions = strings.Join(system, "\n\n")

	for _, d := range RenderTools(req.Tools) {
		w.Tools = append(w.Tools, responsesTool{
			Type: "function", Name: d.Function.Name, Description: d.Function.Description,
			Parameters: d.Function.Parameters,
		})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(w); err != nil {
		return nil, fmt.Errorf("provider: encoding request: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
