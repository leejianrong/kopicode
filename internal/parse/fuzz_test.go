package parse_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/leejianrong/kopicode/internal/parse"
)

// FuzzExtract holds the two invariants that make this package safe to point at
// untrusted input, which every model reply is by definition: it never panics,
// and it never returns a half-parsed call.
//
// The seed corpus is the extraction corpus plus a few adversarial shapes, so
// `go test` exercises them on every run even without -fuzz.
func FuzzExtract(f *testing.F) {
	for _, tc := range corpus {
		f.Add(tc.msg.Content)
	}
	for _, s := range []string{
		"```tool",
		"```tool\n[[[[[[[[[[{\"tool\":\"x\"}]]]]]]]]]]\n```",
		"<tool_call><tool_call><tool_call>",
		"```tool\n{\"tool\":\"x\",\"arguments\":\"\\\"\"}\n```",
		"{{{{{{{{{{{{{{{{{{{{",
		"```tool\n{\"function\":{\"function\":{\"name\":\"x\"}}}\n```",
		"```tool\n\x00\x01\xff\n```",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, content string) {
		ext, err := parse.Extract(parse.Message{Content: content})

		if err != nil {
			var perr *parse.Error
			if !errors.As(err, &perr) {
				t.Fatalf("failure was not classified: %v", err)
			}
			if perr.Kind == parse.KindUnspecified {
				t.Fatalf("failure classified as unspecified, which names a bug here: %v", err)
			}
			if ext.Route() != parse.RouteUnknown || len(ext.Calls()) != 0 {
				t.Fatalf("a failed extraction returned calls: %+v", ext.Calls())
			}
			return
		}

		if !ext.Route().Valid() {
			t.Fatalf("success with route %v", ext.Route())
		}
		calls := ext.Calls()
		if len(calls) == 0 {
			t.Fatal("success with no calls")
		}
		for i, call := range calls {
			if call.Name == "" {
				t.Fatalf("call %d has no tool name", i)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(call.Arguments, &obj); err != nil {
				t.Fatalf("call %d: arguments %q are not a JSON object: %v", i, call.Arguments, err)
			}
		}
	})
}
