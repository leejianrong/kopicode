package parse_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/parse"
)

// extractCase is one corpus entry: an input, and either the extraction it must
// produce or the classified failure it must produce. It is data, so KAN-777's
// recordings append to the corpus rather than growing new test functions.
type extractCase struct {
	// name is the behaviour under test, not the code path.
	name string
	// why records the model behaviour this case stands for, so a future
	// reader can tell an invented case from an observed one.
	why string
	msg parse.Message

	// Success expectations. route is RouteUnknown on a failure case.
	route parse.Route
	calls []wantCall

	// Failure expectations. kind is KindUnspecified on a success case.
	kind   parse.Kind
	noCall bool // the failure must satisfy errors.Is(err, ErrNoToolCall)
}

// wantCall is the expected shape of one extracted call. args is the compact
// JSON the arguments must normalise to; rawHas, when set, must appear in the
// call's Raw.
type wantCall struct {
	id       string
	name     string
	args     string
	encoding parse.ArgEncoding
	rawHas   string
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestExtractCorpus(t *testing.T) {
	for _, tc := range corpus {
		t.Run(tc.name, func(t *testing.T) {
			ext, err := parse.Extract(tc.msg)

			if tc.kind != parse.KindUnspecified {
				assertFailure(t, tc, ext, err)
				return
			}
			assertSuccess(t, tc, ext, err)
		})
	}
}

func assertSuccess(t *testing.T, tc extractCase, ext parse.Extraction, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("Extract() failed on a case that should extract (%s): %v", tc.why, err)
	}
	if got := ext.Route(); got != tc.route {
		t.Errorf("route = %s, want %s", got, tc.route)
	}
	if !ext.Route().Valid() {
		t.Errorf("a successful extraction reported an invalid route %v", ext.Route())
	}

	calls := ext.Calls()
	if len(calls) != len(tc.calls) {
		t.Fatalf("got %d calls, want %d: %+v", len(calls), len(tc.calls), calls)
	}
	for i, want := range tc.calls {
		got := calls[i]
		if got.Name != want.name {
			t.Errorf("call %d: name = %q, want %q", i, got.Name, want.name)
		}
		if want.id != "" && got.ID != want.id {
			t.Errorf("call %d: id = %q, want %q", i, got.ID, want.id)
		}
		if string(got.Arguments) != want.args {
			t.Errorf("call %d: arguments = %s, want %s", i, got.Arguments, want.args)
		}
		if got.ArgEncoding != want.encoding {
			t.Errorf("call %d: arg encoding = %s, want %s", i, got.ArgEncoding, want.encoding)
		}
		if want.rawHas != "" && !strings.Contains(got.Raw, want.rawHas) {
			t.Errorf("call %d: raw %q does not contain %q", i, got.Raw, want.rawHas)
		}
		if got.Raw == "" {
			t.Errorf("call %d: raw is empty; the journal needs the text as emitted", i)
		}
	}
}

func assertFailure(t *testing.T, tc extractCase, ext parse.Extraction, err error) {
	t.Helper()

	if err == nil {
		t.Fatalf("Extract() succeeded on a case that should fail (%s): %+v", tc.why, ext.Calls())
	}
	if ext.Route() != parse.RouteUnknown || len(ext.Calls()) != 0 {
		t.Errorf("a failed extraction returned calls: route=%s calls=%+v", ext.Route(), ext.Calls())
	}

	var perr *parse.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error %v is not a *parse.Error; the repair loop has nothing to classify", err)
	}
	if perr.Kind != tc.kind {
		t.Errorf("kind = %s, want %s (error: %v)", perr.Kind, tc.kind, err)
	}
	if got := errors.Is(err, parse.ErrNoToolCall); got != tc.noCall {
		t.Errorf("errors.Is(err, ErrNoToolCall) = %v, want %v", got, tc.noCall)
	}
	if !strings.Contains(err.Error(), perr.Kind.String()) {
		t.Errorf("error message %q does not name its kind %s", err, perr.Kind)
	}
}

// TestExtractRoutePrecedence pins the order in docs/SLICE-1.md §3. Which route
// a model reaches for is only a finding if the extractor's own preference is
// fixed and known.
func TestExtractRoutePrecedence(t *testing.T) {
	fenced := "```tool\n{\"tool\":\"fenced_tool\",\"arguments\":{}}\n```"
	tagged := "<tool_call>{\"name\":\"xml_tool\",\"arguments\":{}}</tool_call>"

	tests := []struct {
		name     string
		msg      parse.Message
		wantRt   parse.Route
		wantTool string
	}{
		{
			name: "native beats both text routes",
			msg: parse.Message{
				Content:   fenced + "\n" + tagged,
				ToolCalls: []parse.NativeCall{{Name: "native_tool", Arguments: raw(`{}`)}},
			},
			wantRt:   parse.RouteNative,
			wantTool: "native_tool",
		},
		{
			name:     "fenced beats xml",
			msg:      parse.Message{Content: tagged + "\n" + fenced},
			wantRt:   parse.RouteFencedJSON,
			wantTool: "fenced_tool",
		},
		{
			name:     "xml when it is the only route present",
			msg:      parse.Message{Content: tagged},
			wantRt:   parse.RouteXMLTag,
			wantTool: "xml_tool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ext, err := parse.Extract(tt.msg)
			if err != nil {
				t.Fatalf("Extract() = %v", err)
			}
			if ext.Route() != tt.wantRt {
				t.Errorf("route = %s, want %s", ext.Route(), tt.wantRt)
			}
			if got := ext.Calls()[0].Name; got != tt.wantTool {
				t.Errorf("tool = %q, want %q", got, tt.wantTool)
			}
		})
	}
}

// TestExtractionCarriesItsRoute is the structural half of the card: a
// successful extraction that does not say which route produced it must not be
// representable outside this package.
func TestExtractionCarriesItsRoute(t *testing.T) {
	var zero parse.Extraction
	if zero.Route() != parse.RouteUnknown {
		t.Errorf("the zero Extraction reports route %s; it must report unknown", zero.Route())
	}
	if zero.Route().Valid() {
		t.Error("the zero Extraction reports a valid route")
	}
	if len(zero.Calls()) != 0 {
		t.Error("the zero Extraction carries calls")
	}

	ext, err := parse.Extract(parse.Message{ToolCalls: []parse.NativeCall{{Name: "list_dir"}}})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}
	if !ext.Route().Valid() {
		t.Fatal("Extract() returned a successful extraction with no route")
	}
}

// TestCallsReturnsACopy keeps the extraction a record rather than a mutable
// buffer: it is evidence about what the model asked for.
func TestCallsReturnsACopy(t *testing.T) {
	ext, err := parse.Extract(parse.Message{Content: "```tool\n{\"tool\":\"read_file\",\"arguments\":{\"path\":\"a.go\"}}\n```"})
	if err != nil {
		t.Fatalf("Extract() = %v", err)
	}

	got := ext.Calls()
	got[0].Name = "rm_rf"

	if again := ext.Calls()[0].Name; again != "read_file" {
		t.Errorf("mutating the returned slice changed the extraction: %q", again)
	}
}

// TestRawIsNotHTMLEscaped covers all three routes with arguments carrying `<`,
// `>` and `&` — a comparison operator, an HTML fragment, a shell `&&`.
//
// Raw's whole purpose is to be what the model sent, and it reaches the journal
// as ToolCallRequested. encoding/json escapes those three characters by default,
// so a Raw built with json.Marshal is valid JSON over different bytes than the
// model emitted, in the one field defined by being byte-identical. That is the
// trap journal.Marshal exists for (KAN-884), one layer upstream of the journal.
//
// The text routes carry a substring of the reply and so were never at risk; they
// are asserted here anyway, because the property belongs to Raw rather than to
// whichever route happened to produce it.
func TestRawIsNotHTMLEscaped(t *testing.T) {
	const args = `{"command":"grep -n '<div>' *.go && echo done > out.txt"}`

	tests := []struct {
		name  string
		msg   parse.Message
		route parse.Route
		// rawIsJSON marks the routes whose Raw is a JSON object. On the text
		// routes it is the block including its fence or its tags, which is what
		// the model wrote and therefore not JSON.
		rawIsJSON bool
	}{
		{
			name:      "native",
			msg:       parse.Message{ToolCalls: []parse.NativeCall{{ID: "call_1", Name: "run_shell", Arguments: raw(args)}}},
			route:     parse.RouteNative,
			rawIsJSON: true,
		},
		{
			name:  "fenced json",
			msg:   parse.Message{Content: "```tool\n{\"name\":\"run_shell\",\"arguments\":" + args + "}\n```"},
			route: parse.RouteFencedJSON,
		},
		{
			name:  "xml tag",
			msg:   parse.Message{Content: "<tool_call>{\"name\":\"run_shell\",\"arguments\":" + args + "}</tool_call>"},
			route: parse.RouteXMLTag,
		},
	}

	// The escapes encoding/json produces by default, and the literal each one
	// stands for.
	escapes := map[string]string{`\u003c`: "<", `\u003e`: ">", `\u0026`: "&"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ext, err := parse.Extract(tt.msg)
			if err != nil {
				t.Fatalf("Extract() = %v", err)
			}
			if ext.Route() != tt.route {
				t.Fatalf("route = %s, want %s", ext.Route(), tt.route)
			}

			call := ext.Calls()[0]
			for escape, literal := range escapes {
				if strings.Contains(call.Raw, escape) {
					t.Errorf("Raw HTML-escapes %q as %s; it must hold what the model sent:\n%s",
						literal, escape, call.Raw)
				}
				if strings.Contains(string(call.Arguments), escape) {
					t.Errorf("Arguments HTML-escape %q as %s:\n%s", literal, escape, call.Arguments)
				}
			}

			// The positive half: the operators survive as themselves rather
			// than merely not appearing in their escaped spelling.
			const verbatim = `grep -n '<div>' *.go && echo done > out.txt`
			if !strings.Contains(call.Raw, verbatim) {
				t.Errorf("Raw does not carry the command verbatim:\n%s", call.Raw)
			}
			if !strings.Contains(string(call.Arguments), verbatim) {
				t.Errorf("Arguments do not carry the command verbatim:\n%s", call.Arguments)
			}

			// Where Raw is JSON it must still be the JSON it claims to be: not
			// escaping is not a licence to emit something a reader cannot
			// decode.
			if tt.rawIsJSON {
				var probe map[string]json.RawMessage
				if err := json.Unmarshal([]byte(call.Raw), &probe); err != nil {
					t.Errorf("Raw does not decode as JSON: %v\n%s", err, call.Raw)
				}
			}
		})
	}
}

// TestArgumentsAreAlwaysAnObject is the invariant every tool downstream will
// assume. A tool handed a JSON string where it expects an object is a harness
// failure that would be attributed to the model.
func TestArgumentsAreAlwaysAnObject(t *testing.T) {
	for _, tc := range corpus {
		if tc.kind != parse.KindUnspecified {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			ext, err := parse.Extract(tc.msg)
			if err != nil {
				t.Fatalf("Extract() = %v", err)
			}
			for i, call := range ext.Calls() {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(call.Arguments, &obj); err != nil {
					t.Errorf("call %d: arguments %s do not decode as an object: %v", i, call.Arguments, err)
				}
				if call.Name == "" {
					t.Errorf("call %d: extracted with an empty tool name", i)
				}
			}
		})
	}
}
