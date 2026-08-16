// Command kopicode is the interactive terminal coding agent.
//
// What is wired up is the model selection ADR-0007 settles — --model,
// --harness, and the precedence between the flag, .kopicode/config.toml and
// the built-in default — because an unrecognised model id has to be refused at
// startup rather than at the first provider request, and that refusal belongs
// to the front end.
//
// The REPL surface itself is in the repl sub-package, complete and tested, and
// is not driven from here yet: assembling a session needs a constructor
// internal/engine does not export, and ADR-0003's allowlist means a front end
// cannot build one out of internal/journal, internal/tools, internal/permission
// and the rest itself. Until that constructor exists this binary resolves the
// arm and exits 4 rather than pretending — an unimplemented binary exiting
// cleanly is how a broken harness passes a smoke test.
//
// See docs/SLICE-1.md for the scope of the first slice and docs/adr/ for the
// decisions this build starts from.
package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/leejianrong/kopicode/internal/build"
	"github.com/leejianrong/kopicode/internal/engine"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// commands are the subcommands this binary offers, and the map is the usage
// message's source so the two cannot disagree.
//
// `run` is declared here before it does anything (KAN-794, docs/SLICE-1.md
// build step 14). A subcommand that exists and refuses is a slot the next card
// fills; one that does not exist is a flag-parsing shape the next card has to
// invent, and inventing it twice is how the two front ends stop agreeing about
// how they are invoked.
var commands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"repl":    repl,
	"run":     runPrint,
	"version": version,
}

// defaultCommand is what a bare `kopicode` does.
const defaultCommand = "repl"

// run is main with its streams and its exit code in the open, so the whole
// binary is callable from a test.
func run(args []string, stdout, stderr io.Writer) int {
	name, rest := command(args)
	cmd, ok := commands[name]
	if !ok {
		say(stderr, "kopicode: unknown command %q; try one of: repl, run, version\n", name)
		return exitUsage
	}
	return cmd(rest, stdout, stderr)
}

// say writes a message to one of the process's own streams.
//
// The write error is discarded, deliberately and in one place rather than at
// six call sites. A binary whose stderr has gone away — `kopicode 2>&-`, a
// closed pipe — has nowhere to report that it could not report something, and
// turning it into an exit code would make the most ordinary shell idiom look
// like a failure of the thing being run.
func say(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

// command splits a subcommand off the front of args.
//
// A leading argument that starts with "-" is a flag, so `kopicode --model X`
// is the default command with a flag rather than a command named "--model".
// --version is spelled both ways because it is the one thing people type
// without reading anything first.
func command(args []string) (string, []string) {
	if len(args) == 0 {
		return defaultCommand, nil
	}
	switch args[0] {
	case "--version", "-version":
		return "version", args[1:]
	}
	if args[0][0] == '-' {
		return defaultCommand, args
	}
	return args[0], args[1:]
}

// version prints the identity the engine journals on SessionStarted, by the
// same call. Two ways of asking a binary who it is would eventually disagree,
// and the one nobody looks at would be the one in the record.
func version(_ []string, stdout, _ io.Writer) int {
	say(stdout, "%s\n", build.Current())
	return exitSuccess
}

// runPrint is KAN-794's slot: journal-derived JSON on stdout and the five
// documented exit codes.
func runPrint(_ []string, _, stderr io.Writer) int {
	say(stderr, "kopicode: `run --print` is not implemented yet — see docs/SLICE-1.md build step 14\n")
	return exitHarness
}

// repl resolves the arm and would start the interactive loop.
func repl(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("kopicode", flag.ContinueOnError)
	fs.SetOutput(stderr)
	overrides := engine.BindSelectionFlags(fs)
	debug := fs.Bool("debug", false, "engine diagnostics on stderr")
	if err := fs.Parse(args); err != nil {
		// flag has already printed the error and the usage.
		return exitUsage
	}
	setupLogging(*debug, stderr)

	dir, err := os.Getwd()
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		return exitHarness
	}

	// Before anything else, per ADR-0007 decision 4: before a prompt is
	// assembled, before the journal is opened, before SessionStarted is
	// written. An unknown model is a configuration failure wearing a network
	// failure's clothes, and failing here means no half-session record exists
	// for a session that never started.
	selection, err := engine.ResolveSelection(dir, *overrides)
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		if engine.IsSelectionUsageError(err) {
			return exitUsage
		}
		return exitHarness
	}

	// Which rung of the precedence chain answered is diagnostics, not session
	// content: the journal records the resolved values because those identify
	// the arm (ADR-0007 §Consequences, and CLAUDE.md on slog never duplicating
	// the journal).
	slog.Debug("arm resolved", "selection", selection)

	// The loop the repl package drives is built and tested; what is missing is
	// the session it drives. See this file's package comment.
	say(stderr, "kopicode: the REPL is not implemented yet — the engine exports no session "+
		"constructor a front end can call (see docs/SLICE-1.md build step 13)\n")
	return exitHarness
}

// setupLogging puts engine diagnostics on stderr under --debug and discards
// them otherwise.
//
// Off by default, stderr rather than stdout because --print owns stdout, and
// engine-internal only: a logger that recorded session facts would be the
// parallel transcript ADR-0002 exists to prevent, arriving disguised as
// observability.
func setupLogging(debug bool, stderr io.Writer) {
	level := slog.LevelDebug
	out := io.Discard
	if debug {
		out = stderr
	} else {
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})))
}
