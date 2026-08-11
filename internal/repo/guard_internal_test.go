package repo

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The guards on the promise are unit-testable without a repository, and worth
// testing directly: each one exists to catch a mistake that has no other
// symptom until somebody loses their staged work.

func TestVerifyIndexIsolated(t *testing.T) {
	const want = "/repo/.kopicode/index/session"

	for _, tc := range []struct {
		name string
		env  []string
		ok   bool
	}{
		{"set to ours", []string{"PATH=/bin", "GIT_INDEX_FILE=" + want}, true},
		{"unset", []string{"PATH=/bin"}, false},
		{"empty, which git reads as the real index", []string{"GIT_INDEX_FILE="}, false},
		{"someone else's", []string{"GIT_INDEX_FILE=/repo/.git/index"}, false},
		{"relative", []string{"GIT_INDEX_FILE=.kopicode/index/session"}, false},
		// os/exec and execve both let the last duplicate win, so a guard that
		// stopped at the first match would approve an environment git will
		// read differently. This is the case that makes the check worth having
		// rather than merely worth writing.
		{"ours, then overridden later", []string{
			"GIT_INDEX_FILE=" + want, "GIT_INDEX_FILE=/repo/.git/index",
		}, false},
		{"overridden earlier, ours last", []string{
			"GIT_INDEX_FILE=/repo/.git/index", "GIT_INDEX_FILE=" + want,
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyIndexIsolated(tc.env, want)
			if tc.ok && err != nil {
				t.Fatalf("verifyIndexIsolated = %v, want nil", err)
			}
			if !tc.ok {
				if !errors.Is(err, ErrIndexNotIsolated) {
					t.Fatalf("verifyIndexIsolated = %v, want ErrIndexNotIsolated", err)
				}
				if !strings.Contains(err.Error(), "GIT_INDEX_FILE") {
					t.Errorf("the message does not name the variable: %q", err)
				}
			}
		})
	}
}

// TestBaseEnvStripsTheGitOverrides — an inherited variable from a hook, a
// rebase or an editor plugin must not reach the child.
func TestBaseEnvStripsTheGitOverrides(t *testing.T) {
	for _, name := range gitEnvOverrides {
		t.Setenv(name, "inherited")
	}
	t.Setenv("KOPICODE_KEPT", "yes")

	env := baseEnv()
	for _, name := range gitEnvOverrides {
		if v, ok := envValue(env, name); ok {
			t.Errorf("baseEnv kept %s=%q", name, v)
		}
	}
	if v, ok := envValue(env, "KOPICODE_KEPT"); !ok || v != "yes" {
		t.Errorf("baseEnv dropped an unrelated variable: %q, %v", v, ok)
	}
	// The user's git configuration governs how their files are read — line
	// endings, clean filters — so a snapshot taken without it would not
	// describe their working tree.
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM"} {
		for _, n := range gitEnvOverrides {
			if n == name {
				t.Errorf("%s is stripped, which changes how git reads the user's files", name)
			}
		}
	}
}

// TestRepoGitRefusesIndexWritingSubcommands is the structural half of the
// promise. The environment on this path has no GIT_INDEX_FILE, so a `git add`
// here would stage into the user's real index; the only reliable defence
// against a later card adding one is that it does not run.
func TestRepoGitRefusesIndexWritingSubcommands(t *testing.T) {
	r := &Repo{root: t.TempDir()}
	for _, sub := range []string{"add", "write-tree", "status", "reset", "checkout", "stash", "commit"} {
		_, err := r.git(context.Background(), sub, "--whatever")
		if !errors.Is(err, ErrIndexNotIsolated) {
			t.Errorf("r.git(%q) = %v, want ErrIndexNotIsolated", sub, err)
		}
	}
	// `status` is the least obvious entry and the one most likely to be added
	// innocently later: it refreshes and rewrites the index as a side effect of
	// reporting.
	if !indexWriting["status"] {
		t.Error("status is not on the index-writing list, but it rewrites the index while reporting")
	}
}

func TestValidateSessionID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"01J8Z5X7QK", true},
		{"a", true},
		{"a-b_c.d", true},
		{"", false},
		{"a/b", false},
		{"../escape", false},
		{".dot", false},
		{"-dash", false},
		{"a..b", false},
		{"x.lock", false},
		{"a b", false},
		{"a\nb", false},
		{strings.Repeat("x", maxSessionIDLen), true},
		{strings.Repeat("x", maxSessionIDLen+1), false},
	} {
		err := validateSessionID(tc.id)
		if tc.want && err != nil {
			t.Errorf("validateSessionID(%q) = %v, want nil", tc.id, err)
		}
		if !tc.want && !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("validateSessionID(%q) = %v, want ErrInvalidSessionID", tc.id, err)
		}
	}
}

func TestIsObjectID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{strings.Repeat("a", 40), true},
		{strings.Repeat("f", 64), true},
		{strings.Repeat("A", 40), false}, // git prints lowercase
		{strings.Repeat("a", 39), false},
		{"", false},
		{"fatal: not a tree object", false},
	} {
		if got := isObjectID(tc.id); got != tc.want {
			t.Errorf("isObjectID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestSplitLines(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"\n", 0},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a\nb", 2},
		{"a\r\nb\r\n", 2},
	} {
		if got := len(splitLines(tc.in)); got != tc.want {
			t.Errorf("splitLines(%q) has %d lines, want %d", tc.in, got, tc.want)
		}
	}
}
