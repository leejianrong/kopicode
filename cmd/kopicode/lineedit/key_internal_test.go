package lineedit

import (
	"bufio"
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

// declaredKeys parses key.go and returns the names of every constant declared
// with type key. Parsing the source is the point: a hand-maintained list of
// key names would go stale in exactly the same way the binding does.
func declaredKeys(t *testing.T) []string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "key.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing key.go: %v", err)
	}

	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
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
		if !ok || ident.Name != "key" {
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
		t.Fatal("found no key constants in key.go; the guard is broken, not the keys")
	}
	return names
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
