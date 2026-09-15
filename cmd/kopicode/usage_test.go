package main

import (
	"strings"
	"testing"
)

// TestCommandHelpMatchesCommands holds the two halves of the subcommand set to
// each other: [commands] decides what runs, [commandHelp] decides what the
// top-level usage shows, and the map's own doc comment has always claimed the
// two cannot disagree. Before this test that was an aspiration — the usage
// message was never built from the map — so a command added to one and not the
// other went silently undiscoverable.
func TestCommandHelpMatchesCommands(t *testing.T) {
	inHelp := make(map[string]bool, len(commandHelp))
	for _, c := range commandHelp {
		if inHelp[c.name] {
			t.Errorf("commandHelp lists %q twice", c.name)
		}
		inHelp[c.name] = true
		if _, ok := commands[c.name]; !ok {
			t.Errorf("commandHelp lists %q, which is not a real command", c.name)
		}
	}
	for name := range commands {
		if !inHelp[name] {
			t.Errorf("command %q has no commandHelp entry, so `kopicode -h` never mentions it", name)
		}
	}
}

// TestTopLevelHelpListsCommandsAndExitsZero: a help request is answered on
// stdout, lists every subcommand, and exits 0. The REPL flag set a bare
// `kopicode -h` used to reach lists no commands at all, which was the whole
// reason a newcomer could not find `run`, `serve` or `sessions`.
func TestTopLevelHelpListsCommandsAndExitsZero(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		var stdout, stderr strings.Builder
		if code := run([]string{arg}, &stdout, &stderr); code != exitSuccess {
			t.Errorf("run(%q) exit = %d, want %d (help is not an error). stderr:\n%s",
				arg, code, exitSuccess, stderr.String())
		}
		for _, want := range []string{"repl", "run", "serve", "sessions", "version"} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("run(%q) help does not mention %q:\n%s", arg, want, stdout.String())
			}
		}
		if stderr.String() != "" {
			t.Errorf("run(%q) wrote to stderr for an explicit help request:\n%s", arg, stderr.String())
		}
	}
}
