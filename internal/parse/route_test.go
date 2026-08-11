package parse_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/leejianrong/kopicode/internal/parse"
)

// TestRouteTextRoundTrip guards the wire form. The route reaches the journal,
// and the journal is a compatibility surface from the first commit — an
// integer that a future reordering of the constants silently redefines is
// exactly the drift that makes an old session unreadable.
func TestRouteTextRoundTrip(t *testing.T) {
	routes := []struct {
		route parse.Route
		text  string
	}{
		{parse.RouteNative, "native"},
		{parse.RouteFencedJSON, "fenced_json"},
		{parse.RouteXMLTag, "xml_tag"},
		{parse.RouteUnknown, "unknown"},
	}

	for _, r := range routes {
		t.Run(r.text, func(t *testing.T) {
			b, err := r.route.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText() = %v", err)
			}
			if string(b) != r.text {
				t.Errorf("MarshalText() = %q, want %q", b, r.text)
			}
			if r.route.String() != r.text {
				t.Errorf("String() = %q, want %q", r.route, r.text)
			}

			var back parse.Route
			if err := back.UnmarshalText(b); err != nil {
				t.Fatalf("UnmarshalText(%q) = %v", b, err)
			}
			if back != r.route {
				t.Errorf("round trip produced %v, want %v", back, r.route)
			}
		})
	}

	var r parse.Route
	if err := r.UnmarshalText([]byte("smoke_signals")); err == nil {
		t.Error("UnmarshalText accepted an unknown route")
	}

	if parse.RouteUnknown.Valid() {
		t.Error("RouteUnknown reports itself valid")
	}
}

// TestRouteJSONShape checks the route serialises as its name inside a struct,
// which is how the journal will carry it.
func TestRouteJSONShape(t *testing.T) {
	b, err := json.Marshal(struct {
		Route parse.Route `json:"route"`
	}{parse.RouteFencedJSON})
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	if string(b) != `{"route":"fenced_json"}` {
		t.Errorf("Marshal() = %s, want {\"route\":\"fenced_json\"}", b)
	}
}

// TestKindTextRoundTrip does the same for the failure classification, which the
// bench classifier reads.
func TestKindTextRoundTrip(t *testing.T) {
	kinds := []parse.Kind{
		parse.KindNoCall,
		parse.KindUnlabelledFence,
		parse.KindUnfencedCall,
		parse.KindUnclosedFence,
		parse.KindUnclosedTag,
		parse.KindEmptyBlock,
		parse.KindInvalidJSON,
		parse.KindMissingName,
		parse.KindInvalidArguments,
		parse.KindAmbiguousCall,
		parse.KindUnknownTool,
		parse.KindMissingArgument,
		parse.KindWrongArgumentType,
		parse.KindUnknownEnumValue,
	}

	seen := map[string]bool{}
	for _, k := range kinds {
		b, err := k.MarshalText()
		if err != nil {
			t.Fatalf("MarshalText() on %v = %v", k, err)
		}
		if seen[string(b)] {
			t.Errorf("two kinds share the wire name %q", b)
		}
		seen[string(b)] = true

		var back parse.Kind
		if err := back.UnmarshalText(b); err != nil {
			t.Fatalf("UnmarshalText(%q) = %v", b, err)
		}
		if back != k {
			t.Errorf("round trip of %q produced %v", b, back)
		}
	}

	var k parse.Kind
	if err := k.UnmarshalText([]byte("vibes")); err == nil {
		t.Error("UnmarshalText accepted an unknown kind")
	}
}

// TestErrorNamesRouteAndCause checks the error text carries what a human
// debugging a bad session needs, and that the cause survives for errors.Is.
func TestErrorNamesRouteAndCause(t *testing.T) {
	_, err := parse.Extract(parse.Message{Content: "```tool\n{'tool': 'read_file'}\n```"})
	if err == nil {
		t.Fatal("Extract() accepted single-quoted JSON")
	}

	var perr *parse.Error
	if !errors.As(err, &perr) {
		t.Fatalf("error %v is not a *parse.Error", err)
	}
	if perr.Route != parse.RouteFencedJSON {
		t.Errorf("route = %s, want fenced_json", perr.Route)
	}
	if perr.Snippet == "" {
		t.Error("the error carries no snippet of the offending text")
	}
	if errors.Unwrap(err) == nil {
		t.Error("the JSON decode error was not wrapped; %w is how the cause survives")
	}
	if errors.Is(err, parse.ErrNoToolCall) {
		t.Error("a malformed call reported itself as no call at all")
	}
}
