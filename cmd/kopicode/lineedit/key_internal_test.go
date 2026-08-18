package lineedit

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// This file is the completeness guard for the key table, and it is here rather
// than in the external test package because the thing being guarded is
// unexported.
//
// The problem it solves is specific. A key binding has three independent
// halves — a byte sequence that decodes to it, a name, and a case in
// (*session).handle — and getting two of the three right produces a key that
// *looks* implemented. A decoder entry with no binding swallows the sequence
// and does nothing. A binding with no decoder entry is dead code that reviews
// as a working feature. Neither fails a hand-written table test, because
// whoever forgot the binding also forgot the row.
//
// So the set of keys is derived from the source rather than listed: the const
// block in key.go is parsed, and every constant found there must appear in
// keySamples below. One map, three assertions over it, and a key added without
// all three halves fails the suite instead of shipping.
//
// # Three go/ast walks, and why this one stays a copy
//
// This file, internal/parse/wirename_internal_test.go and
// internal/arch/subprocess_test.go (plus imports_test.go and ldflags_test.go)
// are all completeness guards written over go/ast, and the first two are close
// enough to be the same bug twice: KAN-841 found that
// internal/parse/wirename_internal_test.go's declaredConstants indexed
// gen.Specs[0] with no length check, so `const ()` — a legal, empty const
// block — panicked the test instead of failing it. declaredKeys below had the
// identical defect (KAN-848), because it was written the same way against the
// same problem: walk a source file, find every CONST GenDecl whose first spec
// names a given type, collect the rest of that block's names.
//
// That is two copies of one shape, which is a real argument for a shared
// helper — and it stays unshared anyway, because sharing one costs more than a
// tidiness objection. cmd/kopicode/lineedit is a front end under ADR-0003's
// import allowlist (docs/adr/0003-single-repo-internal-engine.md decision 3),
// enforced by internal/arch/imports_test.go's
// TestFrontEndsOnlyImportEngineInterface, which walks every .go file under
// cmd/ — test files included — and fails on an import of anything internal/
// other than internal/engine, internal/bench and internal/build.
// wirename_internal_test.go's logic would have to live somewhere either side
// import from, and both directions are already refused: putting it under
// internal/ and importing it from this test file needs a fourth allowlist
// entry, which CLAUDE.md is explicit needs an ADR rather than a helper
// extraction; putting it under cmd/ and importing it from
// internal/parse would trip TestEngineDoesNotImportSurfaces the same test file
// enforces the other way. So this is not a boundary chosen for this card's
// sake — it is already enforced code, and crossing it costs an ADR, not a
// refactor. A one-point bug fix is not that occasion.
//
// internal/arch's walks are a different shape besides: they parse
// import-only or whole-function bodies across the entire tree looking for
// import edges and exec.Cmd construction, not one file's const block for one
// named type, so a three-way unification was never really on the table —
// the live question was only ever whether these two near-identical copies
// merge, and the answer above is no, for a structural reason rather than a
// preference. If a fourth walk of this same const-block shape turns up, that
// is the point to revisit this, not before: three data points made KAN-848
// worth deciding, and a decision made once should not be re-litigated by the
// next person who has not read this comment — which is why it is written down
// here instead of left to be re-discovered.

// keySamples maps every key to a byte sequence a terminal would send for it.
// A key constant missing from here fails TestEveryKeyConstantHasASample.
var keySamples = map[key]string{
	keyRune:          "a",
	keyEnter:         "\r",
	keyInterrupt:     "\x03",
	keyEOF:           "\x04",
	keyBackspace:     "\x7f",
	keyDeleteForward: "\x1b[3~",
	keyLeft:          "\x1b[D",
	keyRight:         "\x1b[C",
	keyUp:            "\x1b[A",
	keyDown:          "\x1b[B",
	keyHome:          "\x01",
	keyEnd:           "\x05",
	keyKillToEnd:     "\x0b",
	keyKillToStart:   "\x15",
	keyIgnored:       "\x1b[15~", // F5: recognised, consumed whole, dropped
}

// fataler is the sliver of *testing.T that constantsOfType needs. It exists so
// TestConstantsOfTypeFailsCleanlyOnEmptyConstBlock can pass a fake in place of
// a real *testing.T: a genuine t.Run subtest that calls t.Fatal marks its
// *parent* failed too, which is the wrong signal for a test whose entire point
// is confirming that the failure is clean rather than a panic. *testing.T
// satisfies this interface already, so every other caller is unaffected.
type fataler interface {
	Helper()
	Fatalf(format string, args ...any)
}

// declaredKeys parses key.go and returns the names of every constant declared
// with type key. Parsing the source is the point: a hand-maintained list of
// key names would go stale in exactly the same way the binding does.
func declaredKeys(t *testing.T) []string {
	t.Helper()
	return constantsOfType(t, "key.go", nil, "key")
}

// constantsOfType parses filename — reading it from disk when src is nil, or
// parsing src directly otherwise, exactly as [parser.ParseFile] does — and
// returns the name of every constant declared with the given type.
//
// It takes src as a parameter, rather than being declaredKeys's body inlined,
// so TestConstantsOfTypeFailsCleanlyOnEmptyConstBlock below can drive it
// against a literal string instead of key.go: that is what proves the
// `const ()` guard a few lines down without having to edit key.go to break it.
func constantsOfType(t fataler, filename string, src any, typeName string) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
		return nil // fataler.Fatalf need not halt the caller; see fatalRecorder.
	}

	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		// `const ()` is legal, empty Go and parses to a GenDecl with zero
		// Specs. Indexing Specs[0] below without this check panics the test
		// instead of failing it (KAN-848, the same defect KAN-841 found in
		// internal/parse/wirename_internal_test.go's declaredConstants).
		if !ok || gen.Tok != token.CONST || len(gen.Specs) == 0 {
			continue
		}
		// The type appears on the first spec of an iota block and is
		// implied by the rest, so the block is taken as a whole once its
		// first spec names `key`.
		first, ok := gen.Specs[0].(*ast.ValueSpec)
		if !ok {
			continue
		}
		ident, ok := first.Type.(*ast.Ident)
		if !ok || ident.Name != typeName {
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

	if len(names) == 0 {
		t.Fatalf("found no %s constants in %s: the AST walk is broken, not the "+
			"type. Every check built on it would pass having asserted nothing.",
			typeName, filename)
	}
	return names
}

// fatalRecorder is the fake fataler TestConstantsOfTypeFailsCleanlyOnEmptyConstBlock
// drives constantsOfType with. It records a Fatalf call as data — failed and
// message — instead of stopping the goroutine the way *testing.T does, which
// is what lets the test that follows tell "failed cleanly" apart from
// "panicked": if constantsOfType still indexed gen.Specs[0] with no length
// check, this recorder would not turn that into a tidy false; the panic would
// propagate out of constantsOfType and fail the test the ordinary, loud way.
type fatalRecorder struct {
	failed  bool
	message string
}

func (f *fatalRecorder) Helper() {}

func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.failed = true
	f.message = fmt.Sprintf(format, args...)
}

// TestConstantsOfTypeFailsCleanlyOnEmptyConstBlock is the regression case for
// KAN-848: a source file containing only `const ()` — legal, empty Go — used
// to panic constantsOfType at gen.Specs[0] instead of failing the test. The
// fix a few lines up skips a GenDecl with no Specs, so the walk instead finds
// zero key constants and reports that the ordinary way, through the same
// "found no constants" Fatalf every other broken-walk case goes through.
func TestConstantsOfTypeFailsCleanlyOnEmptyConstBlock(t *testing.T) {
	const src = `package lineedit

const ()
`
	rec := &fatalRecorder{}
	names := constantsOfType(rec, "key.go", src, "key")

	if !rec.failed {
		t.Fatalf("constantsOfType(%q) returned %v with no Fatalf call; an empty "+
			"const block should still fail the walk, just without panicking", src, names)
	}
	if !strings.Contains(rec.message, "found no key constants") {
		t.Fatalf("constantsOfType failed with message %q; want it to say it found "+
			"no key constants, so the failure explains itself rather than reading "+
			"as an unrelated parse error", rec.message)
	}
}

// byName inverts keyNames so a constant name parsed out of the source can be
// turned back into the value it names.
func byName(t *testing.T) map[string]key {
	t.Helper()
	out := make(map[string]key, len(keyNames))
	for k, name := range keyNames {
		if prev, dup := out[name]; dup {
			t.Errorf("keyNames gives %q to both %d and %d", name, int(prev), int(k))
		}
		out[name] = k
	}
	return out
}

// TestEveryKeyConstantIsNamed is the first of the three halves: a key with no
// entry in keyNames stringifies as an integer, and every failure message in
// this package would then name a number.
func TestEveryKeyConstantIsNamed(t *testing.T) {
	named := byName(t)
	declared := declaredKeys(t)

	for _, name := range declared {
		if _, ok := named[name]; !ok {
			t.Errorf("key constant %s has no entry in keyNames; add one, "+
				"or every test failure mentioning it prints an integer", name)
		}
		delete(named, name)
	}
	for name := range named {
		t.Errorf("keyNames has an entry for %q, which is not a key constant in key.go", name)
	}
}

// TestEveryKeyConstantHasASample is the second half: the key table below is
// derived from the source, so a constant added without a sample sequence fails
// here rather than going untested.
func TestEveryKeyConstantHasASample(t *testing.T) {
	named := byName(t)
	sampled := make(map[string]bool, len(keySamples))
	for k := range keySamples {
		sampled[k.String()] = true
	}

	for _, name := range declaredKeys(t) {
		if !sampled[name] {
			t.Errorf("key constant %s has no row in keySamples; add the byte "+
				"sequence a terminal sends for it, so the decoder and the "+
				"binding are both asserted for it", name)
		}
		delete(sampled, name)
	}
	for name := range sampled {
		if _, ok := named[name]; !ok {
			t.Errorf("keySamples has a row for %q, which is not a key constant", name)
		}
	}
}

// TestSamplesDecodeToTheirKey is the decoder half: the sequence really does
// produce the key it is filed under.
func TestSamplesDecodeToTheirKey(t *testing.T) {
	for want, sample := range keySamples {
		t.Run(want.String(), func(t *testing.T) {
			kr := &keyReader{r: bufio.NewReader(strings.NewReader(sample))}
			got, r, err := kr.next()
			if err != nil {
				t.Fatalf("decoding %q: %v", sample, err)
			}
			if got != want {
				t.Fatalf("%q decoded to %v, want %v", sample, got, want)
			}
			if got == keyRune && r == 0 {
				t.Errorf("%q decoded to keyRune with a zero rune", sample)
			}
			// The whole sequence must be consumed. A decoder that stops
			// halfway through an escape sequence leaves its tail to be
			// typed into the user's line, which is how F5 becomes "15~".
			if rest, _ := kr.r.Peek(1); len(rest) != 0 {
				t.Errorf("%q left %q unconsumed; the rest of the sequence "+
					"would be inserted as text", sample, rest)
			}
		})
	}
}

// TestEveryKeyHasABinding is the third half, and the one that catches the
// silent failure: a key that decodes but has no case in (*session).handle does
// nothing at all, forever, and looks like a working binding from the outside.
func TestEveryKeyHasABinding(t *testing.T) {
	for k, sample := range keySamples {
		t.Run(k.String(), func(t *testing.T) {
			s := newSession(nil)
			if got := s.handle(k, 'x'); got == outcomeUnhandled {
				t.Fatalf("(*session).handle has no case for %v (sent as %q); "+
					"a key that decodes and does nothing looks like a working "+
					"binding and never fails on its own", k, sample)
			}
		})
	}
}

// TestUnhandledIsReachable keeps the guard above honest. If handle ever grows a
// default that swallows unknown keys, TestEveryKeyHasABinding passes for a key
// with no binding — so the failure signal itself is asserted.
func TestUnhandledIsReachable(t *testing.T) {
	s := newSession(nil)
	notAKey := key(len(keyNames) + 1000)
	if got := s.handle(notAKey, 0); got != outcomeUnhandled {
		t.Fatalf("handle(%v) = %v, want outcomeUnhandled; TestEveryKeyHasABinding "+
			"is asserting nothing if an unbound key does not report itself", notAKey, got)
	}
}

// TestUnknownEscapeSequencesAreConsumedWhole is the reason csi() parses the
// CSI grammar rather than matching known strings. F5 is ESC [ 1 5 ~; a decoder
// that only knows the arrows sees ESC [ and gives up, and "15~" ends up in the
// prompt.
func TestUnknownEscapeSequencesAreConsumedWhole(t *testing.T) {
	unknown := []string{
		"\x1b[15~",  // F5
		"\x1b[1;2P", // Shift-F1, with a modifier parameter
		"\x1b[?1u",  // a private sequence, parameter bytes and all
		"\x1bOZ",    // an SS3 nobody binds
		"\x1b\x1b",  // ESC ESC
	}
	for _, seq := range unknown {
		t.Run(strings.ReplaceAll(seq, "\x1b", "ESC"), func(t *testing.T) {
			kr := &keyReader{r: bufio.NewReader(strings.NewReader(seq + "!"))}
			for {
				k, r, err := kr.next()
				if err != nil {
					t.Fatalf("decoding %q: %v", seq, err)
				}
				if k == keyRune {
					if r != '!' {
						t.Fatalf("%q produced the text rune %q; part of the "+
							"escape sequence leaked into the line", seq, r)
					}
					return
				}
				if k != keyIgnored {
					t.Fatalf("%q decoded to %v, want keyIgnored", seq, k)
				}
			}
		})
	}
}

// TestTruncatedEscapeSequenceReportsTheEOF covers input that stops in the
// middle of a sequence — a pipe closing, a terminal disconnecting. The read
// ends rather than blocking or inventing a key, and the caller turns that into
// io.EOF for the line.
func TestTruncatedEscapeSequenceReportsTheEOF(t *testing.T) {
	for _, seq := range []string{"\x1b[", "\x1b[1", "\x1bO"} {
		t.Run(strings.ReplaceAll(seq, "\x1b", "ESC"), func(t *testing.T) {
			kr := &keyReader{r: bufio.NewReader(strings.NewReader(seq))}
			if _, _, err := kr.next(); err == nil {
				t.Fatalf("decoding the truncated %q returned no error", seq)
			}
		})
	}
}

// TestBareEscapeAtEndOfInputKeepsTheLine is the one truncation that is not an
// error. A stream ending in a lone ESC is nothing more than an unbound key
// arriving last, and reporting it as a read failure would throw away whatever
// the user had already typed.
func TestBareEscapeAtEndOfInputKeepsTheLine(t *testing.T) {
	kr := &keyReader{r: bufio.NewReader(strings.NewReader("\x1b"))}
	k, _, err := kr.next()
	if err != nil {
		t.Fatalf("a trailing ESC reported %v, want it treated as an unbound key", err)
	}
	if k != keyIgnored {
		t.Fatalf("a trailing ESC decoded to %v, want keyIgnored", k)
	}
}

// TestAltKeyKeepsItsLetter covers the one escape shape that must *not* be
// swallowed whole. Alt-a arrives as ESC followed by 'a', and eating both would
// lose a character the user typed.
func TestAltKeyKeepsItsLetter(t *testing.T) {
	kr := &keyReader{r: bufio.NewReader(strings.NewReader("\x1ba"))}

	k, _, err := kr.next()
	if err != nil {
		t.Fatalf("decoding the ESC: %v", err)
	}
	if k != keyIgnored {
		t.Fatalf("ESC decoded to %v, want keyIgnored", k)
	}

	k, r, err := kr.next()
	if err != nil {
		t.Fatalf("decoding the letter: %v", err)
	}
	if k != keyRune || r != 'a' {
		t.Fatalf("after ESC got (%v, %q), want (keyRune, 'a') — the letter was eaten", k, r)
	}
}
