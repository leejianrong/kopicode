package parse

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// This file is the completeness guard for ArgEncoding's wire vocabulary, and it
// is internal because argEncodingText is unexported.
//
// It exists because KAN-838 made those strings a compatibility surface: the
// engine writes ArgEncoding.String() into journal.ToolCallParsed's arg_encoding
// field, and the journal is a compatibility surface from the first commit. A
// constant added to the type without an entry in argEncodingText still
// compiles, still passes every extraction test, and ships "arg_encoding(2)"
// into the session record — a value that looks like a bug report rather than a
// measurement, on a field a bench query is grouping by.
//
// Route and Kind are guarded by round-trip tests in route_test.go over lists of
// their constants. Those lists are hand-written, so a constant added to either
// without a row goes unchecked. This one derives the set from the source, which
// is the shape cmd/kopicode/lineedit/key_internal_test.go settled on for the
// same problem. Widening it to cover Route and Kind is worth a card of its own.

// argEncodingByName maps every declared ArgEncoding constant to its value. A
// constant missing from here fails TestEveryArgEncodingConstantIsMapped, which
// is what stops the check below from silently narrowing.
var argEncodingByName = map[string]ArgEncoding{
	"ArgsObject":     ArgsObject,
	"ArgsJSONString": ArgsJSONString,
}

// declaredArgEncodings parses parse.go and returns the name of every constant
// declared with type ArgEncoding.
func declaredArgEncodings(t *testing.T) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "parse.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing parse.go: %v", err)
	}

	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// The type is written on the first spec of an iota block and implied by
		// the rest, so the block is taken whole once its first spec names it.
		first, ok := gen.Specs[0].(*ast.ValueSpec)
		if !ok {
			continue
		}
		ident, ok := first.Type.(*ast.Ident)
		if !ok || ident.Name != "ArgEncoding" {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if name.Name != "_" {
					names = append(names, name.Name)
				}
			}
		}
	}

	// The positive control. If the traversal finds nothing — parse.go moved,
	// the const block grew a form this does not recognise — every assertion
	// below would iterate an empty slice and report success.
	if len(names) == 0 {
		t.Fatal("found no ArgEncoding constants in parse.go; the AST walk is broken, " +
			"so this guard is asserting nothing")
	}
	return names
}

// TestEveryArgEncodingConstantIsMapped keeps argEncodingByName honest, so the
// wire-form check below covers the whole type rather than the part someone
// remembered.
func TestEveryArgEncodingConstantIsMapped(t *testing.T) {
	for _, name := range declaredArgEncodings(t) {
		if _, ok := argEncodingByName[name]; !ok {
			t.Errorf("ArgEncoding constant %s has no entry in argEncodingByName — add one, "+
				"and give it a wire name in argEncodingText", name)
		}
	}
}

// TestEveryArgEncodingHasADistinctWireName is the guard proper: the value the
// journal records must be a name, it must be stable, and two encodings must
// never share one — a collision would merge two findings into one number
// without failing anything else.
func TestEveryArgEncodingHasADistinctWireName(t *testing.T) {
	seen := map[string]string{}

	for _, name := range declaredArgEncodings(t) {
		value, ok := argEncodingByName[name]
		if !ok {
			continue // reported by the test above
		}

		text, ok := argEncodingText[value]
		if !ok || text == "" {
			t.Errorf("%s has no entry in argEncodingText, so it reaches the journal as %q — "+
				"a placeholder, on a field the bench groups by", name, value.String())
			continue
		}
		if got := value.String(); got != text {
			t.Errorf("%s stringifies as %q but argEncodingText says %q", name, got, text)
		}
		if b, err := value.MarshalText(); err != nil {
			t.Errorf("%s does not marshal: %v", name, err)
		} else if string(b) != text {
			t.Errorf("%s marshals as %q but stringifies as %q; the journal writes the "+
				"latter, so they must agree", name, b, text)
		}
		if other, dup := seen[text]; dup {
			t.Errorf("%s and %s share the wire name %q", name, other, text)
		}
		seen[text] = name
	}

	if len(seen) != len(argEncodingText) {
		t.Errorf("argEncodingText has %d entries but only %d constants reached it — "+
			"an entry with no constant is a wire name nothing can produce",
			len(argEncodingText), len(seen))
	}
}

// TestUnknownArgEncodingRefusesToMarshal. String falls back to a placeholder so
// a debug print never panics, but MarshalText must fail instead: a value that
// serialises to "arg_encoding(9)" in a session record is worse than a session
// that failed to write, because it pools with real measurements.
func TestUnknownArgEncodingRefusesToMarshal(t *testing.T) {
	unknown := ArgEncoding(200)
	if _, err := unknown.MarshalText(); err == nil {
		t.Error("MarshalText accepted an ArgEncoding with no wire name")
	}
	if got := unknown.String(); got == "" {
		t.Error("String returned empty for an unknown encoding; it must be recognisable, not blank")
	}
}
