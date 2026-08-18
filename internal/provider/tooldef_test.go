package provider_test

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/leejianrong/kopicode/internal/parse"
	"github.com/leejianrong/kopicode/internal/provider"
)

// sampleSchemas is a small, hand-built catalogue exercising every shape
// RenderTools has to handle: a required argument, an optional one, an enum,
// and parse.TypeAny (the zero value, which no shipped tool actually produces
// but which RenderTools still has to render honestly rather than inventing a
// JSON Schema type for it).
func sampleSchemas() []parse.Schema {
	return []parse.Schema{
		{
			Name:        "read_file",
			Description: "Read a text file.",
			Params: []parse.Param{
				{Name: "path", Type: parse.TypeString, Required: true, Description: "the file's path"},
				{Name: "offset", Type: parse.TypeInteger, Description: "1-based line to start at"},
			},
		},
		{
			Name:        "run_shell",
			Description: "Run a shell command.",
			Params: []parse.Param{
				{Name: "command", Type: parse.TypeString, Required: true, Description: "the command line"},
				{Name: "mode", Type: parse.TypeString, Enum: []string{"fast", "safe"}, Description: "which mode to run in"},
				{Name: "whatever", Type: parse.TypeAny, Description: "unconstrained"},
			},
		},
	}
}

// TestRenderToolsShape checks the wire bytes against OpenAI's documented
// function-calling contract field by field, not just that something decodes.
func TestRenderToolsShape(t *testing.T) {
	out := provider.RenderTools(sampleSchemas())
	if len(out) != 2 {
		t.Fatalf("RenderTools returned %d definitions, want 2", len(out))
	}

	raw, err := json.Marshal(out[0])
	if err != nil {
		t.Fatalf("marshalling the first definition: %v", err)
	}

	var decoded struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  struct {
				Type       string `json:"type"`
				Properties map[string]struct {
					Type        string   `json:"type"`
					Description string   `json:"description"`
					Enum        []string `json:"enum"`
				} `json:"properties"`
				Required []string `json:"required"`
			} `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding RenderTools' own output as OpenAI's documented shape: %v\n%s", err, raw)
	}

	if decoded.Type != "function" {
		t.Errorf("type = %q, want %q", decoded.Type, "function")
	}
	if decoded.Function.Name != "read_file" {
		t.Errorf("function.name = %q, want %q", decoded.Function.Name, "read_file")
	}
	if decoded.Function.Description != "Read a text file." {
		t.Errorf("function.description = %q", decoded.Function.Description)
	}
	if decoded.Function.Parameters.Type != "object" {
		t.Errorf("parameters.type = %q, want %q", decoded.Function.Parameters.Type, "object")
	}
	if req := decoded.Function.Parameters.Required; len(req) != 1 || req[0] != "path" {
		t.Errorf("parameters.required = %v, want [path]", req)
	}
	path, ok := decoded.Function.Parameters.Properties["path"]
	if !ok {
		t.Fatal(`parameters.properties has no "path" key — properties must render as a genuine ` +
			"JSON object keyed by argument name, not an array")
	}
	if path.Type != "string" || path.Description != "the file's path" {
		t.Errorf("properties.path = %+v", path)
	}

	// The enum and TypeAny cases live on the second tool.
	raw2, err := json.Marshal(out[1])
	if err != nil {
		t.Fatalf("marshalling the second definition: %v", err)
	}
	if err := json.Unmarshal(raw2, &decoded); err != nil {
		t.Fatalf("decoding: %v\n%s", err, raw2)
	}
	mode, ok := decoded.Function.Parameters.Properties["mode"]
	if !ok {
		t.Fatal(`no "mode" property`)
	}
	if len(mode.Enum) != 2 || mode.Enum[0] != "fast" || mode.Enum[1] != "safe" {
		t.Errorf("mode.enum = %v, want [fast safe]", mode.Enum)
	}
	whatever, ok := decoded.Function.Parameters.Properties["whatever"]
	if !ok {
		t.Fatal(`no "whatever" property`)
	}
	if whatever.Type != "" {
		t.Errorf("parse.TypeAny rendered as JSON Schema type %q; the zero value declares no "+
			"constraint and a rendering that invented one would be lying about what the tool accepts",
			whatever.Type)
	}
}

// TestRenderToolsOmitsTheKeyWhenThereIsNothingToAdvertise holds
// [provider.Request.Tools]' own doc comment: nil in must produce nil out, not
// an empty array, because "advertise nothing" and "advertise an empty
// catalogue" are not the same claim to a provider — see wireRequest's
// `omitempty` on the Tools field, which only omits a nil slice.
func TestRenderToolsOmitsTheKeyWhenThereIsNothingToAdvertise(t *testing.T) {
	if out := provider.RenderTools(nil); out != nil {
		t.Errorf("RenderTools(nil) = %#v, want nil", out)
	}
	if out := provider.RenderTools([]parse.Schema{}); out != nil {
		t.Errorf("RenderTools(empty) = %#v, want nil", out)
	}
}

// TestRenderToolsIsDeterministic proves the rendering does not depend on Go's
// randomised map iteration order — the property internal/harness's
// TestConfigHoldsNoMap rests the whole hash preimage on.
//
// There is no map anywhere in RenderTools' own code (a `go vet`-style claim a
// reader can check by reading tooldef.go, the same way parse.Schema's own doc
// comment argues its Params order is "the tool's choice, not sorted here"
// rather than proving it at runtime). What this test drives instead is the
// *consequence*: rendering the same input many times, including from
// concurrent goroutines racing the same schemas through the race detector,
// must produce byte-identical JSON every time. A map in the rendering path
// would not reliably fail this test — Go's iteration order is randomised per
// *process*, not per call — but it is cheap insurance against a future change
// introducing one and it is what the accumulator in wire.go and the
// preimage in internal/harness/hash.go are both held to by their own tests.
func TestRenderToolsIsDeterministic(t *testing.T) {
	schemas := sampleSchemas()

	first, err := json.Marshal(provider.RenderTools(schemas))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	const iterations = 200
	results := make([][]byte, iterations)
	var wg sync.WaitGroup
	for i := range iterations {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, err := json.Marshal(provider.RenderTools(schemas))
			if err != nil {
				t.Errorf("marshalling on iteration %d: %v", i, err)
				return
			}
			results[i] = raw
		}(i)
	}
	wg.Wait()

	for i, got := range results {
		if string(got) != string(first) {
			t.Fatalf("RenderTools produced different bytes on iteration %d than the first call\n"+
				"first: %s\niteration %d: %s", i, first, i, got)
		}
	}
}
