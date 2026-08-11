package strutil

import "testing"

func TestReverseASCII(t *testing.T) {
	if got := Reverse("hello"); got != "olleh" {
		t.Errorf("Reverse(%q) = %q, want %q", "hello", got, "olleh")
	}
}

func TestReverseUnicode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"héllo", "olléh"},
		{"日本語", "語本日"},
		{"naïve café", "éfac evïan"},
	}
	for _, c := range cases {
		if got := Reverse(c.in); got != c.want {
			t.Errorf("Reverse(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInitials(t *testing.T) {
	if got := Initials("ada lovelace"); got != "al" {
		t.Errorf("Initials = %q, want %q", got, "al")
	}
	if got := Initials("  "); got != "" {
		t.Errorf("Initials of blank = %q, want empty", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("日本語のテキスト", 3); got != "日本語…" {
		t.Errorf("Truncate = %q, want %q", got, "日本語…")
	}
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("Truncate = %q, want %q", got, "short")
	}
}
