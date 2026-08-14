// Command kopicode is the interactive terminal coding agent.
//
// The REPL is not implemented yet. What is wired up is the model selection
// ADR-0007 settles — --model, --harness, and the precedence between the flag,
// .kopicode/config.toml and the built-in default — because an unrecognised model
// id has to be refused at startup rather than at the first provider request, and
// that refusal belongs to the front end. See docs/SLICE-1.md for the scope of
// the first slice and docs/adr/ for the decisions this build starts from.
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

// Exit codes, from docs/SLICE-1.md build step 14: 0 success, 1 task not
// completed, 2 usage error, 3 provider error, 4 harness error.
const (
	exitUsage   = 2
	exitHarness = 4
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		// The same identity the engine puts on SessionStarted, printed by the
		// same call. Two ways of asking a binary who it is would eventually
		// disagree, and the one nobody looks at would be the one in the record.
		fmt.Println(build.Current())
		return
	}

	fs := flag.NewFlagSet("kopicode", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	overrides := engine.BindSelectionFlags(fs)
	debug := fs.Bool("debug", false, "engine diagnostics on stderr")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// flag has already printed the error and the usage.
		os.Exit(exitUsage)
	}
	setupLogging(*debug)

	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kopicode: %v\n", err)
		os.Exit(exitHarness)
	}

	// Before anything else, per ADR-0007 decision 4: before a prompt is
	// assembled, before the journal is opened, before SessionStarted is
	// written. An unknown model is a configuration failure wearing a network
	// failure's clothes, and failing here means no half-session record exists
	// for a session that never started.
	selection, err := engine.ResolveSelection(dir, *overrides)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kopicode: %v\n", err)
		if engine.IsSelectionUsageError(err) {
			os.Exit(exitUsage)
		}
		os.Exit(exitHarness)
	}

	// Which rung of the precedence chain answered is diagnostics, not session
	// content: the journal records the resolved values because those identify
	// the arm (ADR-0007 §Consequences, and CLAUDE.md on slog never duplicating
	// the journal).
	slog.Debug("arm resolved", "selection", selection)

	// The REPL is slice-1 build step 13. Until it exists, exit non-zero rather
	// than pretending: a zero exit from an unimplemented binary is how a broken
	// harness passes a smoke test.
	fmt.Fprintln(os.Stderr, "kopicode: not implemented yet — see docs/SLICE-1.md")
	os.Exit(exitHarness)
}

// setupLogging puts engine diagnostics on stderr under --debug and discards
// them otherwise.
//
// Off by default, stderr rather than stdout because --print owns stdout, and
// engine-internal only: a logger that recorded session facts would be the
// parallel transcript ADR-0002 exists to prevent, arriving disguised as
// observability.
func setupLogging(debug bool) {
	level := slog.LevelDebug
	out := io.Discard
	if debug {
		out = os.Stderr
	} else {
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})))
}
