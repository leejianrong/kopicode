// Command kopibench is the headless benchmark runner.
//
// It is a first-class front end, not a test utility: a benchmark cannot drive a
// REPL, which is why the engine has two surfaces from the first commit
// (docs/adr/0003-single-repo-internal-engine.md).
//
// The runner itself is not implemented yet (docs/SLICE-1.md build steps 15-17).
// What is wired up is --model and --harness, which exist on both front ends
// because setting the arm per invocation is the whole point of one binary
// serving every model (ADR-0007 decisions 1 and 2) — and it is here, not in the
// REPL, that it earns its keep: a paired A/B is two invocations of one binary.
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

// Exit codes, from docs/SLICE-1.md build step 14.
const (
	exitUsage   = 2
	exitHarness = 4
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		// The identity a bench result is attributed to. Printing it from the
		// same call the record uses is what keeps a report and a journal from
		// disagreeing about which binary produced a number.
		fmt.Println(build.Current())
		return
	}

	fs := flag.NewFlagSet("kopibench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	overrides := engine.BindSelectionFlags(fs)
	debug := fs.Bool("debug", false, "engine diagnostics on stderr")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(exitUsage)
	}
	setupLogging(*debug)

	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "kopibench: %v\n", err)
		os.Exit(exitHarness)
	}

	// ADR-0007 decision 4 is worth more here than it is in the REPL: deferring
	// an unknown model to the provider means the refusal arrives after N tasks
	// have already run, as a provider error inside a corpus rather than a
	// refusal before it.
	selection, err := engine.ResolveSelection(dir, *overrides)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kopibench: %v\n", err)
		if engine.IsSelectionUsageError(err) {
			os.Exit(exitUsage)
		}
		os.Exit(exitHarness)
	}

	slog.Debug("arm resolved", "selection", selection)

	fmt.Fprintln(os.Stderr, "kopibench: not implemented yet — see docs/SLICE-1.md")
	os.Exit(exitHarness)
}

// setupLogging puts engine diagnostics on stderr under --debug and discards
// them otherwise. Off by default, and never session content: the journal owns
// that.
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
