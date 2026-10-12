package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The Responses API stream (ADR-0035, KOP-194). The ChatGPT/Codex backend speaks
// it and only it (docs/research/2026-10-codex-backend-verification.md): SSE with
// an `event: <name>` line and a `data: {"type": "<name>", ...}` line per event, and
// no [DONE] sentinel: the stream ends after `response.completed`.
//
// What was observed live, and what is handled here:
//
//   - `response.output_text.delta` carries assistant text: a [DeltaContent].
//   - `response.output_item.done` carries each finished item. A `function_call`
//     item (name, call_id, arguments as a JSON *string*) becomes a [ToolCall]. The
//     item list on `response.completed` is empty, so items are collected here and
//     nowhere else.
//   - `response.completed` carries the status, the model and the usage, and ends
//     the stream.
//   - `response.failed`, `response.incomplete` and `error` are terminal; the first
//     and last are errors.
//   - Every other event (`response.created`, `.in_progress`, `.content_part.*`,
//     `.output_item.added`, `.function_call_arguments.*`, `.output_text.done`) is
//     read and ignored: it carries nothing the finished items do not.
//
// Reasoning text arrives as `response.reasoning_summary_text.delta` when the
// request asks for a summary (`reasoning: {"summary": "auto"}`; checked live, with
// a `reasoning` item beside the message). `response.reasoning_text.delta` is the
// open-source client's other name and was not observed. A stream that uses a name
// not listed here loses its reasoning text, not its answer.

// ErrBackendFailure reports a stream the backend itself ended with an error
// event. The code and message it sent are in the text; they are not secrets.
var ErrBackendFailure = errors.New("provider: the backend reported a failure")

// NewResponsesStream reads a streamed Responses-API reply from an SSE body.
//
// [Stream.Transcript] returns the bytes exactly as they arrived, event lines
// included, so a recorded fixture holds what the backend sent and not a re-encoding
// (ADR-0035 decision 2). Reply.Raw is nil, as for any live stream.
func NewResponsesStream(ctx context.Context, body io.Reader) *Stream {
	s := NewSSEStream(ctx, body, nil)
	s.rsp = &responsesDecoder{}
	return s
}

// responsesDecoder folds Responses-API events into a reply.
type responsesDecoder struct {
	id               string
	model            string
	status           string
	incompleteReason string // incomplete_details.reason, when status is incomplete
	content          strings.Builder
	reasoning        strings.Builder
	calls            []ToolCall
	usage            *responsesUsage
	completed        bool
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

// responsesEvent is the subset of an event's data this decoder reads.
type responsesEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Item  *struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *struct {
		ID                string          `json:"id"`
		Model             string          `json:"model"`
		Status            string          `json:"status"`
		Usage             *responsesUsage `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *responsesError `json:"error"`
	} `json:"response"`
	// An `error` event carries code and message at the top level.
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// line folds one SSE line. It reports whether the line carried a data event, which
// is what tells a stream that carried nothing from one that ended early.
func (d *responsesDecoder) line(line string) (deltas []Delta, sawData bool, err error) {
	switch {
	case line == "", strings.HasPrefix(line, sseCommentPrefix):
		return nil, false, nil
	case strings.HasPrefix(line, "event:"):
		// The data line repeats the name in its "type"; the event line is ignored
		// rather than trusted, so the two cannot disagree.
		return nil, false, nil
	case !strings.HasPrefix(line, sseDataPrefix):
		return nil, false, fmt.Errorf("%w: line is neither an event, a %q line, an SSE comment nor blank: %q",
			ErrMalformedStream, sseDataPrefix, line)
	}
	if d.completed {
		return nil, false, fmt.Errorf("%w: an event followed response.completed: %q", ErrMalformedStream, line)
	}

	var ev responsesEvent
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, sseDataPrefix)), &ev); err != nil {
		return nil, true, fmt.Errorf("%w: decoding event: %w", ErrMalformedStream, err)
	}

	switch ev.Type {
	case "response.created":
		if ev.Response != nil {
			d.id, d.model = ev.Response.ID, ev.Response.Model
		}
	case "response.output_text.delta":
		if ev.Delta != "" {
			d.content.WriteString(ev.Delta)
			return []Delta{{Kind: DeltaContent, Text: ev.Delta}}, true, nil
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if ev.Delta != "" {
			d.reasoning.WriteString(ev.Delta)
			return []Delta{{Kind: DeltaReasoning, Text: ev.Delta}}, true, nil
		}
	case "response.output_item.done":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			d.calls = append(d.calls, ToolCall{
				ID:   ev.Item.CallID,
				Name: ev.Item.Name,
				// The backend's `arguments` is a JSON string holding JSON, the same
				// shape the chat-completions path re-quotes to, so the extractor
				// sees one encoding from both.
				Arguments: jsonString(ev.Item.Arguments),
			})
		}
	case "response.completed", "response.incomplete":
		d.completed = true
		if r := ev.Response; r != nil {
			if r.ID != "" {
				d.id = r.ID
			}
			if r.Model != "" {
				d.model = r.Model
			}
			d.status, d.usage = r.Status, r.Usage
			if r.IncompleteDetails != nil {
				d.incompleteReason = r.IncompleteDetails.Reason
			}
		}
	case "response.failed":
		if ev.Response != nil && ev.Response.Error != nil {
			return nil, true, backendFailure(ev.Response.Error.Code, ev.Response.Error.Message)
		}
		return nil, true, backendFailure("", "response.failed with no error body")
	case "error":
		return nil, true, backendFailure(ev.Code, ev.Message)
	}
	return nil, true, nil
}

func backendFailure(code, message string) error {
	return fmt.Errorf("%w: %s: %s", ErrBackendFailure, orUnknown(code), orUnknown(message))
}

func orUnknown(s string) string {
	if s == "" {
		return "(none given)"
	}
	return s
}

// reply builds the assembled reply.
func (d *responsesDecoder) reply() Reply {
	r := Reply{
		ID:           d.id,
		ModelID:      d.model,
		FinishReason: d.finishReason(),
		Content:      d.content.String(),
		Reasoning:    d.reasoning.String(),
		ToolCalls:    d.calls,
	}
	if u := d.usage; u != nil {
		r.Usage = Usage{Prompt: u.InputTokens, Completion: u.OutputTokens, Total: u.TotalTokens}
		if det := u.InputTokensDetails; det != nil {
			r.Usage.CacheRead, r.Usage.CacheWrite = det.CachedTokens, det.CacheWriteTokens
		}
	}
	// Cost stays nil: a subscription has no per-token price, and an unknown cost
	// is never filled in (ADR-0021).
	return r
}

// finishReason maps the Responses status onto the chat-completions vocabulary
// the loop already reads: tool_calls, length, stop.
func (d *responsesDecoder) finishReason() string {
	switch {
	case len(d.calls) > 0:
		return "tool_calls"
	case d.status == "incomplete" && d.incompleteReason == "max_output_tokens":
		return "length"
	case d.status == "completed", d.status == "":
		return "stop"
	default:
		return d.status
	}
}
