package wrap

import (
	"reflect"
	"testing"
)

func TestWrapShortText(t *testing.T) {
	got := Wrap("one two", 20)
	want := []string{"one two"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Wrap = %q, want %q", got, want)
	}
}

func TestWrapNeverExceedsWidth(t *testing.T) {
	// "epsilon" is the longest word at seven characters, and a word longer
	// than the width legitimately gets a line of its own, so the sweep starts
	// where every word still fits.
	text := "alpha beta gamma delta epsilon zeta eta theta iota kappa"
	for width := 7; width <= 20; width++ {
		lines := Wrap(text, width)
		if n := Longest(lines); n > width {
			t.Errorf("width %d: longest line is %d characters: %q", width, n, lines)
		}
	}
}

func TestWrapExactFit(t *testing.T) {
	// "alpha beta" is exactly ten characters, so it fits a width of ten and
	// not a width of nine.
	if got, want := Wrap("alpha beta", 10), []string{"alpha beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Wrap(width 10) = %q, want %q", got, want)
	}
	if got, want := Wrap("alpha beta", 9), []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Wrap(width 9) = %q, want %q", got, want)
	}
}

func TestWrapLongWord(t *testing.T) {
	got := Wrap("a extraordinarily b", 5)
	want := []string{"a", "extraordinarily", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Wrap = %q, want %q", got, want)
	}
}

func TestIndent(t *testing.T) {
	got := Indent([]string{"a", "b"}, "  ")
	want := []string{"  a", "  b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Indent = %q, want %q", got, want)
	}
}
