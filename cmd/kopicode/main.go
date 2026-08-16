// Command kopicode is the interactive terminal coding agent.
//
// What is wired up is the model selection ADR-0007 settles — --model,
// --harness, and the precedence between the flag, .kopicode/config.toml and
// the built-in default — because an unrecognised model id has to be refused at
// startup rather than at the first provider request, and that refusal belongs
// to the front end.
//
// There are two surfaces over one engine. The interactive one is the repl
// sub-package; the headless one is `run --print` in print.go. This file is the
// ninety lines that connect either to a session: resolve the arm, open the
// session with engine.Open, hand the surface the engine's event stream to render
// and its consent requests to answer, and map the stop onto an exit code.
// Everything about *how* a session is assembled is the engine's, which is what
// ADR-0003 decision 3 means by driving it through its exported interface — this
// file imports internal/engine and internal/build and nothing else from
// internal/.
//
// See docs/SLICE-1.md for the scope of the first slice and docs/adr/ for the
// decisions this build starts from.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/build"
	"github.com/leejianrong/kopicode/internal/engine"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// commands are the subcommands this binary offers, and the map is the usage
// message's source so the two cannot disagree.
//
// `repl` is the interactive surface (ADR-0004) and `run` the headless one
// (docs/SLICE-1.md build step 14). They are two renderings of one event stream
// and not two engines: both open a session with engine.Open, both receive
// engine.Options.Events, and neither holds a transcript of its own — see
// print.go and cmd/kopicode/repl.
var commands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"repl":    interactive,
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

// interactive resolves the arm and starts the REPL over a session.
func interactive(args []string, _, stderr io.Writer) int {
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

	return session(stdio(stderr), engine.Options{Dir: dir, Selection: selection})
}

// streams is where the REPL reads and writes, and how it tells whether it has a
// terminal.
//
// It is a parameter rather than os.Stdin and os.Stdout reached for directly, so
// that the whole front end — resolution, session, loop, exit code — is drivable
// from a test with the bytes captured. The two "is this a terminal" questions
// are answered by one value: see [stdio].
type streams struct {
	in       io.Reader
	out      io.Writer
	err      io.Writer
	terminal lineedit.Terminal
}

// stdio is the process's own streams.
//
// The terminal decides both halves, and they are different questions: whether
// input can be read a key at a time, and whether output may carry escapes.
// OSTerminal answers yes only when *both* ends are terminals, which is exactly
// the condition for each — `kopicode > transcript` with a keyboard attached
// must produce a file with no escapes in it.
func stdio(stderr io.Writer) streams {
	return streams{
		in:       os.Stdin,
		out:      os.Stdout,
		err:      stderr,
		terminal: lineedit.OSTerminal(os.Stdin, os.Stdout),
	}
}

// session opens a session, drives the REPL over it, and reports the exit code.
//
// It takes the options rather than building them so that a test can supply a
// directory and a provider endpoint; everything about *how* a session is
// assembled is the engine's, which is the point of the constructor this calls.
func session(std streams, opts engine.Options) int {
	stderr := std.err
	// The signal registration comes first and is undone last. Between these
	// two lines SIGINT no longer terminates the process — it cancels the
	// in-flight turn — and a binary that left it registered after the loop
	// finished would ignore a Ctrl-C it can no longer act on.
	interrupts, stopSignals := repl.Interrupts()
	defer stopSignals()

	// The loop and the session each need the other: the session announces its
	// own record through the loop, and the loop's turns call the session. The
	// two closures below tie the knot, and it is a knot rather than a cycle
	// because neither is invoked until Loop.Run — by which point sess is set,
	// or this function has already returned.
	var sess *engine.Session

	loop, err := repl.New(repl.Config{
		In:          std.in,
		Out:         std.out,
		Interactive: std.terminal.IsInteractive(),
		Terminal:    std.terminal,
		Interrupts:  interrupts,
		Turn: func(ctx context.Context, prompt string, _ repl.Surface) (engine.Result, error) {
			return sess.Run(ctx, prompt)
		},
		Close: func(ctx context.Context) error { return sess.Close(ctx) },
	})
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
		return exitHarness
	}

	opts.Events = loop.Render
	opts.Consent = loop.Ask

	ctx := context.Background()
	sess, err = engine.Open(ctx, opts)
	if err != nil {
		// A missing credential is exit 4 and not exit 2. Exit 2 is ADR-0007
		// decision 4's case — an id this binary does not recognise, refused
		// before anything is opened — and stretching it to cover a machine that
		// is not set up would make the code that means "your command line was
		// wrong" also mean "your environment was". Nothing was opened either
		// way; the record of that is this message.
		if errors.Is(err, engine.ErrNoAPIKey) {
			say(stderr, "kopicode: %s is not set, so there is no provider to talk to\n", engine.APIKeyEnv)
			return exitHarness
		}
		say(stderr, "kopicode: %v\n", err)
		return exitHarness
	}
	// The session id and the model are already on screen: the record's own
	// SessionStarted said them, and the surface renders it. What the record
	// cannot say is where it is, so that is the one line added here.
	loop.Notice("record: " + sess.Path())

	stop, err := loop.Run(ctx)
	if err != nil {
		say(stderr, "kopicode: %v\n", err)
	}
	return stop.ExitCode()
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
