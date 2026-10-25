package main

import (
	"strings"
	"testing"
)

func TestSessionIDOption(t *testing.T) {
	dir := t.TempDir()
	if got, err := sessionIDOption("", dir, "", ""); got != "" || err != nil {
		t.Errorf("unset = %q, %v; want nothing", got, err)
	}
	if got, err := sessionIDOption("cuttlefish-1", dir, "", ""); got != "cuttlefish-1" || err != nil {
		t.Errorf("good id = %q, %v", got, err)
	}
	for _, c := range []struct{ id, resume, fork, want string }{
		{"x", "other", "", "--resume"},
		{"x", "", "src:1", "--fork"},
		{"../x", "", "", "not usable"},
	} {
		_, err := sessionIDOption(c.id, dir, c.resume, c.fork)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("sessionIDOption(%+v) = %v, want a refusal mentioning %q", c, err, c.want)
		}
	}
}
