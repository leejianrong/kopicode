package fixture_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/provider/fixture"
)

// The validator's own tests.
//
// Every rule in validate.go is here as a mutation of a real fixture: load a
// good one, break exactly one thing, and require the error. A validator whose
// rules have never been watched reject anything is a validator asserting
// nothing, which is the same vacuous green the discovery tests are written
// against.
//
// The mutations are deliberately small — one field each — because a mutation
// that breaks two rules at once passes whichever rule is checked first and
// proves nothing about the other.

// good is the fixture the mutations start from. It is re-loaded for every
// subtest, so a mutation cannot leak into the next one through a shared slice.
const good = "two_turn_native_tool_call"

func load(t *testing.T) fixture.Fixture {
	t.Helper()
	f, err := fixture.Load(fixture.FS(), good)
	if err != nil {
		t.Fatalf("loading %s: %v", good, err)
	}
	return f
}

func TestValidateAcceptsTheUnmutatedFixture(t *testing.T) {
	// The control. Without it, every case below could be passing because
	// Validate rejects everything.
	if err := fixture.Validate(load(t)); err != nil {
		t.Fatalf("the unmutated fixture does not validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		// why says which failure the rule exists to catch, so a deleted case
		// reads as a lost guarantee rather than as one less test.
		why    string
		mutate func(*fixture.Fixture)
		want   string
	}{
		{
			name:   "origin missing",
			why:    "an unmarked fixture is one somebody eventually quotes as evidence about the provider",
			mutate: func(f *fixture.Fixture) { f.Origin = "" },
			want:   "origin",
		},
		{
			name:   "origin note missing on a synthetic fixture",
			why:    "a synthetic fixture that does not say what it models cannot be judged or replaced",
			mutate: func(f *fixture.Fixture) { f.OriginNote = "  " },
			want:   "origin_note",
		},
		{
			name:   "format version from the future",
			why:    "a container this build cannot read must fail rather than decode into zero values",
			mutate: func(f *fixture.Fixture) { f.FormatVersion = fixture.Version + 1 },
			want:   "format_version",
		},
		{
			name:   "fallbacks allowed",
			why:    "a session allowed to fall back is not a record of the arm it names (ADR-0005 §2)",
			mutate: func(f *fixture.Fixture) { f.Pin.AllowFallbacks = true },
			want:   "allow_fallbacks",
		},
		{
			name:   "no pin order",
			why:    "an unpinned result is not evidence",
			mutate: func(f *fixture.Fixture) { f.Pin.Order = nil },
			want:   "pin.order",
		},
		{
			name:   "two providers pinned",
			why:    "an arm pins one provider; two is two experiments pooled",
			mutate: func(f *fixture.Fixture) { f.Pin.Order = []string{"a", "b"} },
			want:   "pin.order",
		},
		{
			name:   "no quantization",
			why:    "two runs at different quantizations are two experiments",
			mutate: func(f *fixture.Fixture) { f.Pin.Quantizations = nil },
			want:   "pin.quantizations",
		},
		{
			name: "response header off the allowlist",
			why:  "recorded headers are allowlisted, because the header nobody excluded is the one with the key",
			mutate: func(f *fixture.Fixture) {
				f.Exchanges[0].Response.Headers["x-secret-token"] = "anything"
			},
			want: "allowlist",
		},
		{
			name:   "declared finish reason drifts from the body",
			why:    "expect is a convenience, and a convenience nobody checks becomes a rival source of truth",
			mutate: func(f *fixture.Fixture) { f.Exchanges[0].Expect.FinishReason = "stop" },
			want:   "expect.finish_reason",
		},
		{
			name:   "declared usage drifts from the body",
			why:    "same, on the numbers a cost report is computed from",
			mutate: func(f *fixture.Fixture) { f.Exchanges[0].Expect.Usage.Completion = 999 },
			want:   "expect.usage",
		},
		{
			name:   "declared served-by drifts from the body",
			why:    "a result served outside the declared pin is discarded, so the two must agree",
			mutate: func(f *fixture.Fixture) { f.Exchanges[0].Expect.ServedBy = "SomeoneElse" },
			want:   "expect.served_by",
		},
		{
			name:   "declared tools drift from the native array",
			why:    "a fixture that names a tool the body never calls drives a different session than it claims",
			mutate: func(f *fixture.Fixture) { f.Exchanges[0].Expect.Tools = []string{"grep"} },
			want:   "expect.tools",
		},
		{
			name: "finish reason outside the vocabulary",
			why:  "OpenRouter normalises to five values; a sixth is a typo or a misunderstanding",
			mutate: func(f *fixture.Fixture) {
				f.Exchanges[0].Response.Body = replaceIn(f.Exchanges[0].Response.Body,
					`"finish_reason": "tool_calls"`, `"finish_reason": "finished"`)
				f.Exchanges[0].Expect.FinishReason = "finished"
			},
			want: "normalised values",
		},
		{
			name: "usage that does not add up",
			why:  "a hand-typed token count is exactly the field that gets edited and not re-totalled",
			mutate: func(f *fixture.Fixture) {
				f.Exchanges[0].Response.Body = replaceIn(f.Exchanges[0].Response.Body,
					`"total_tokens": 1313`, `"total_tokens": 1300`)
				f.Exchanges[0].Expect.Usage.Total = 1300
			},
			want: "do not add up",
		},
		{
			name: "the stream stops agreeing with the body",
			why:  "carrying two representations means two sessions under one name unless they are held together",
			mutate: func(f *fixture.Fixture) {
				s := f.Exchanges[0].Response.Stream
				for i, line := range s {
					s[i] = strings.Replace(line, "greet.go", "greet_test.go", 1)
				}
			},
			want: "assembles arguments",
		},
		{
			name: "the stream never terminates",
			why:  "a truncated capture must not read as a complete response",
			mutate: func(f *fixture.Fixture) {
				s := f.Exchanges[0].Response.Stream
				for i, line := range s {
					if strings.HasPrefix(line, "data: [DONE]") {
						s[i] = ""
					}
				}
			},
			want: "sentinel",
		},
		{
			name: "a stream line is neither data nor a comment",
			why:  "an SSE parser handed junk should be told, not left to guess",
			mutate: func(f *fixture.Fixture) {
				f.Exchanges[0].Response.Stream[0] = "OPENROUTER PROCESSING"
			},
			want: "neither a `data: ` line",
		},
		{
			name:   "turns run backwards",
			why:    "a fixture is a session in order, not a bag of replies",
			mutate: func(f *fixture.Fixture) { f.Exchanges[1].Turn = 1 },
			want:   "repeats turn",
		},
		{
			name: "the session never stops",
			why:  "a fixture set with no terminal reply cannot show a loop finishing",
			mutate: func(f *fixture.Fixture) {
				// The body and the stream both have to move, or this trips the
				// stream-agreement rule instead and proves nothing about the
				// terminal one.
				last := &f.Exchanges[len(f.Exchanges)-1]
				last.Response.Body = replaceIn(last.Response.Body,
					`"finish_reason": "stop"`, `"finish_reason": "length"`)
				for i, line := range last.Response.Stream {
					last.Response.Stream[i] = strings.ReplaceAll(line,
						`"finish_reason":"stop"`, `"finish_reason":"length"`)
				}
				last.Expect.FinishReason = "length"
			},
			want: "stops the loop",
		},
		{
			name: "the last reply still calls a tool",
			why:  "a terminal reply asks for no more work, or the loop was cut off rather than finished",
			mutate: func(f *fixture.Fixture) {
				f.Exchanges[len(f.Exchanges)-1].Expect.Tools = []string{"read_file"}
			},
			want: "asks for no more work",
		},
		{
			name:   "an exchange explains nothing",
			why:    "an unexplained exchange is one nobody can replace with a recording",
			mutate: func(f *fixture.Fixture) { f.Exchanges[0].Note = "" },
			want:   "note is empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := load(t)
			tc.mutate(&f)

			err := fixture.Validate(f)
			if err == nil {
				t.Fatalf("Validate accepted a fixture with %s\nthis rule exists because %s", tc.name, tc.why)
			}
			if !errors.Is(err, fixture.ErrInvalid) {
				t.Fatalf("Validate returned %v, which does not wrap ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate said %q, which does not mention %q — an error a reader cannot act "+
					"on gets deleted by whoever hits it", err, tc.want)
			}
		})
	}
}

// replaceIn swaps one substring inside a raw JSON body, keeping it raw. The
// fixtures are indented, so the needles here carry the space after the colon
// that encoding/json writes.
func replaceIn(body []byte, from, to string) []byte {
	out := strings.Replace(string(body), from, to, 1)
	return []byte(out)
}
