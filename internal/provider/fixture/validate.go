package fixture

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalid is the cause every validation failure wraps, so a caller can tell
// a malformed fixture from a missing one without matching on text.
var ErrInvalid = errors.New("invalid fixture")

// Validate checks a fixture against itself.
//
// It cannot check a fixture against the provider — only a recording does that,
// and there are none yet (see the package doc). What it can do is refuse the
// failure modes a hand-edited data file actually has: a marker left off, a pin
// that is not a pin, a stream that stopped agreeing with the body it sits next
// to, a declared finish reason nobody updated after changing the body.
//
// Every rule below is checked over every fixture in the data directory by the
// package's tests, discovered by walking the directory rather than from a list,
// so a fixture added tomorrow is checked by these rules without anyone
// remembering to register it.
func Validate(f Fixture) error {
	if f.FormatVersion != Version {
		return invalid(f, "format_version is %d, want %d", f.FormatVersion, Version)
	}
	if !f.Origin.Valid() {
		return invalid(f, "origin is %q, want %q or %q — a fixture that does not say where it came from "+
			"is one somebody will eventually quote as evidence about the provider",
			f.Origin, OriginHandAuthored, OriginRecorded)
	}
	if f.Origin.Synthetic() && strings.TrimSpace(f.OriginNote) == "" {
		return invalid(f, "origin is %q with an empty origin_note — a synthetic fixture must say what it "+
			"models and on what basis, or the next reader cannot judge it", f.Origin)
	}
	if strings.TrimSpace(f.Name) == "" {
		return invalid(f, "name is empty")
	}
	if strings.TrimSpace(f.Description) == "" {
		return invalid(f, "description is empty")
	}
	if strings.TrimSpace(f.ModelID) == "" {
		return invalid(f, "model_id is empty")
	}
	if err := validatePin(f); err != nil {
		return err
	}
	if len(f.Exchanges) == 0 {
		return invalid(f, "no exchanges")
	}
	for i, ex := range f.Exchanges {
		if err := validateExchange(f, i, ex); err != nil {
			return err
		}
	}
	return validateSequence(f)
}

// validatePin holds ADR-0005 §2 to the fixture: an unpinned recording is not
// evidence, so a fixture that claims to be one has to carry a pin that could
// have produced it.
func validatePin(f Fixture) error {
	if len(f.Pin.Order) == 0 {
		return invalid(f, "pin.order is empty — every benchmark request sets provider.order to a single "+
			"slug (docs/adr/0005-benchmark-and-ab-methodology.md §2)")
	}
	if len(f.Pin.Order) != 1 {
		return invalid(f, "pin.order names %d providers; a benchmark arm pins exactly one", len(f.Pin.Order))
	}
	if f.Pin.AllowFallbacks {
		return invalid(f, "pin.allow_fallbacks is true — a session that was allowed to fall back is not a "+
			"record of the arm it names")
	}
	if len(f.Pin.Quantizations) == 0 {
		return invalid(f, "pin.quantizations is empty — the quantization is fixed per arm, and two runs at "+
			"different quantizations are two experiments")
	}
	return nil
}

// validateExchange checks one request/response pair, including the agreement
// between its two representations.
func validateExchange(f Fixture, i int, ex Exchange) error {
	where := fmt.Sprintf("exchange %d (turn %d attempt %d)", i, ex.Turn, ex.Attempt)

	if ex.Turn < 1 {
		return invalid(f, "%s: turn is %d, want 1 or more", where, ex.Turn)
	}
	if ex.Attempt < 1 {
		return invalid(f, "%s: attempt is %d; it is 1 for the first send and increments per retry", where, ex.Attempt)
	}
	if strings.TrimSpace(ex.Note) == "" {
		return invalid(f, "%s: note is empty — an exchange nobody explained is one nobody can replace", where)
	}
	if ex.Response.Status == 0 {
		return invalid(f, "%s: response.status is unset", where)
	}
	for name := range ex.Response.Headers {
		if !allowedResponseHeaders[strings.ToLower(name)] {
			return invalid(f, "%s: response header %q is not on the allowlist — recorded headers are "+
				"allowlisted rather than denylisted, because the header nobody thought to exclude is the "+
				"one that carries the credential", where, name)
		}
	}

	body, err := decodeBody(ex.Response.Body)
	if err != nil {
		return invalid(f, "%s: %v", where, err)
	}
	if err := validateCompletion(f, where, body); err != nil {
		return err
	}
	if err := validateExpect(f, where, ex, body); err != nil {
		return err
	}
	if !ex.Response.Streamed() {
		return nil
	}
	return validateStreamAgrees(f, where, ex, body)
}

// validateCompletion checks the assembled body is a completion kopicode could
// have received.
func validateCompletion(f Fixture, where string, body completion) error {
	if body.Object != "chat.completion" {
		return invalid(f, "%s: body object is %q, want %q", where, body.Object, "chat.completion")
	}
	if body.Model != f.ModelID {
		return invalid(f, "%s: body model is %q but the fixture's model_id is %q", where, body.Model, f.ModelID)
	}
	if len(body.Choices) != 1 {
		return invalid(f, "%s: body carries %d choices; kopicode sends n=1 and the loop is written for one",
			where, len(body.Choices))
	}
	c := body.Choices[0]
	if c.Message == nil {
		return invalid(f, "%s: body choice carries no message", where)
	}
	if !finishReasons[c.finishReason()] {
		return invalid(f, "%s: finish_reason is %q, which is not one of OpenRouter's normalised values",
			where, c.finishReason())
	}
	if body.Usage == nil {
		return invalid(f, "%s: body carries no usage — OpenRouter always includes it, so a fixture without "+
			"one is modelling a response that does not occur", where)
	}
	if got, want := body.Usage.PromptTokens+body.Usage.CompletionTokens, body.Usage.TotalTokens; got != want {
		return invalid(f, "%s: usage totals do not add up: prompt %d + completion %d is %d, but total_tokens "+
			"is %d", where, body.Usage.PromptTokens, body.Usage.CompletionTokens, got, want)
	}
	for j, tc := range c.Message.ToolCalls {
		if tc.Type != "function" {
			return invalid(f, "%s: tool call %d has type %q, want %q", where, j, tc.Type, "function")
		}
		if tc.ID == "" {
			return invalid(f, "%s: tool call %d has no id; the id is echoed back on the tool result", where, j)
		}
		if tc.Function.Name == "" {
			return invalid(f, "%s: tool call %d names no function", where, j)
		}
	}
	return nil
}

// validateExpect holds the fixture's stated facts to the body they describe.
//
// The Expect block exists so a consumer can assert without decoding, which
// makes it a second copy of facts the body already carries — and a second copy
// nobody checks is a second copy that goes stale. These are the checks that
// keep it a convenience rather than a rival source of truth.
func validateExpect(f Fixture, where string, ex Exchange, body completion) error {
	c := body.Choices[0]
	if ex.Expect.FinishReason != c.finishReason() {
		return invalid(f, "%s: expect.finish_reason is %q but the body says %q",
			where, ex.Expect.FinishReason, c.finishReason())
	}
	if ex.Expect.ServedBy != body.Provider {
		return invalid(f, "%s: expect.served_by is %q but the body's provider is %q",
			where, ex.Expect.ServedBy, body.Provider)
	}
	want := Usage{
		Prompt:     body.Usage.PromptTokens,
		Completion: body.Usage.CompletionTokens,
		Total:      body.Usage.TotalTokens,
	}
	if ex.Expect.Usage != want {
		return invalid(f, "%s: expect.usage is %+v but the body reports %+v", where, ex.Expect.Usage, want)
	}

	var names []string
	for _, tc := range c.Message.ToolCalls {
		names = append(names, tc.Function.Name)
	}
	if len(names) > 0 && strings.Join(ex.Expect.Tools, "\x00") != strings.Join(names, "\x00") {
		return invalid(f, "%s: expect.tools is %v but the body's native tool_calls name %v",
			where, ex.Expect.Tools, names)
	}
	if ex.Expect.Route == "" && len(names) > 0 {
		return invalid(f, "%s: the body carries native tool calls but expect.route is empty", where)
	}
	if ex.Expect.Route == routeNative && len(names) == 0 {
		return invalid(f, "%s: expect.route is %q but the body carries no native tool_calls",
			where, routeNative)
	}
	if ex.Expect.Route != "" && len(ex.Expect.Tools) == 0 {
		return invalid(f, "%s: expect.route is %q but expect.tools is empty — a reply that took a "+
			"route called something", where, ex.Expect.Route)
	}
	return nil
}

// routeNative is parse.RouteNative's wire form.
//
// It is a string here rather than a parse.Route because this package must not
// import the extractor: the fixture package is data, and a data package that
// pulls in the parser makes the parser's tests depend on fixtures and the
// fixtures' tests depend on the parser. The tests take that dependency in one
// direction only and assert the strings line up, the same way the journal's
// wire strings are held to internal/tools and internal/parse by test rather
// than by an import.
const routeNative = "native"

// validateStreamAgrees is the check that makes carrying both representations
// safe: the SSE frames must fold back into the body sitting next to them.
//
// Without it a fixture is two sessions under one name, and whichever one a test
// happened to read would look correct.
func validateStreamAgrees(f Fixture, where string, ex Exchange, body completion) error {
	got, err := assembleStream(ex.Response.Stream)
	if err != nil {
		return invalid(f, "%s: %v", where, err)
	}
	want := body.Choices[0]

	if got.ID != body.ID {
		return invalid(f, "%s: the stream's id is %q but the body's is %q", where, got.ID, body.ID)
	}
	if got.Model != body.Model {
		return invalid(f, "%s: the stream's model is %q but the body's is %q", where, got.Model, body.Model)
	}
	if got.Provider != body.Provider {
		return invalid(f, "%s: the stream's provider is %q but the body's is %q", where, got.Provider, body.Provider)
	}
	if got.Created != body.Created {
		return invalid(f, "%s: the stream's created is %d but the body's is %d", where, got.Created, body.Created)
	}
	if got.Choices[0].finishReason() != want.finishReason() {
		return invalid(f, "%s: the stream finishes with %q but the body says %q",
			where, got.Choices[0].finishReason(), want.finishReason())
	}
	if got.Usage == nil {
		return invalid(f, "%s: the stream carried no usage chunk", where)
	}
	if *got.Usage != *body.Usage {
		return invalid(f, "%s: the stream's usage is %+v but the body's is %+v", where, *got.Usage, *body.Usage)
	}

	gotMsg, wantMsg := got.Choices[0].Message, want.Message
	if gotMsg.text() != wantMsg.text() {
		return invalid(f, "%s: the stream's assembled content is %q but the body's is %q",
			where, gotMsg.text(), wantMsg.text())
	}
	if len(gotMsg.ToolCalls) != len(wantMsg.ToolCalls) {
		return invalid(f, "%s: the stream assembles %d tool calls but the body carries %d",
			where, len(gotMsg.ToolCalls), len(wantMsg.ToolCalls))
	}
	for i := range gotMsg.ToolCalls {
		g, w := gotMsg.ToolCalls[i], wantMsg.ToolCalls[i]
		if g.ID != w.ID || g.Type != w.Type || g.Function.Name != w.Function.Name {
			return invalid(f, "%s: tool call %d assembles as %s/%s/%s but the body has %s/%s/%s",
				where, i, g.ID, g.Type, g.Function.Name, w.ID, w.Type, w.Function.Name)
		}
		if g.Function.Arguments != w.Function.Arguments {
			return invalid(f, "%s: tool call %d assembles arguments %q but the body has %q",
				where, i, g.Function.Arguments, w.Function.Arguments)
		}
	}
	return nil
}

// validateSequence checks the fixture describes a session rather than a bag of
// replies: turns run forwards, attempts run forwards within a turn, and the
// last reply ends the loop.
func validateSequence(f Fixture) error {
	prevTurn, prevAttempt := 0, 0
	for i, ex := range f.Exchanges {
		switch {
		case ex.Turn < prevTurn:
			return invalid(f, "exchange %d goes back to turn %d after turn %d", i, ex.Turn, prevTurn)
		case ex.Turn == prevTurn && ex.Attempt <= prevAttempt:
			return invalid(f, "exchange %d repeats turn %d attempt %d", i, ex.Turn, ex.Attempt)
		case ex.Turn > prevTurn && ex.Attempt != 1:
			return invalid(f, "exchange %d opens turn %d at attempt %d, want 1", i, ex.Turn, ex.Attempt)
		}
		prevTurn, prevAttempt = ex.Turn, ex.Attempt
	}

	last := f.Exchanges[len(f.Exchanges)-1]
	if !terminalFinishReasons[last.Expect.FinishReason] {
		return invalid(f, "the last exchange finishes with %q; a session fixture ends on a reason that "+
			"stops the loop, or it is not a session — it is one that was cut off", last.Expect.FinishReason)
	}
	if len(last.Expect.Tools) != 0 {
		return invalid(f, "the last exchange still calls %v; a terminal reply asks for no more work",
			last.Expect.Tools)
	}
	return nil
}

// invalid builds the one error shape this file returns.
func invalid(f Fixture, format string, args ...any) error {
	return fmt.Errorf("fixture %q: %w: %s", f.Name, ErrInvalid, fmt.Sprintf(format, args...))
}
