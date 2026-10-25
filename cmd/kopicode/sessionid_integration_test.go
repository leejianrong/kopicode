//go:build integration

package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// KOP-227 on the real binary: a bad --session-id is a usage error that opens,
// locks and writes nothing, on the REPL and on run --print.
func TestSessionIDFlagOnTheRealBinaryRefusesABadID(t *testing.T) {
	bin := build(t, frontEnds[0])
	for name, args := range map[string][]string{
		"repl":  {"--session-id", "../escape"},
		"run":   {"run", "--print", "--session-id", "../escape", "do nothing"},
		"mixed": {"--session-id", "ok", "--resume", "other"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := repoDir(t)
			res := run(t, bin, dir, sessionEnv(), args...)
			if res.code != exitUsage {
				t.Fatalf("exit = %d, want %d. stderr:\n%s", res.code, exitUsage, res.stderr)
			}
			if !strings.Contains(res.stderr, "--session-id") {
				t.Errorf("the refusal does not name the flag:\n%s", res.stderr)
			}
			if _, err := os.Stat(filepath.Join(dir, ".kopicode")); !os.IsNotExist(err) {
				t.Errorf("a refused --session-id created .kopicode")
			}
		})
	}
}
