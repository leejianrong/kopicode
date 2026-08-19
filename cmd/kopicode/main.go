// Command kopicode is the interactive terminal coding agent.
//
// What is wired up is the model selection ADR-0007 settles — --model,
// --harness, and the precedence between the flag, .kopicode/config.toml and
// the built-in default — because an unrecognised model id has to be refused at
// startup rather than at the first provider request, and that refusal belongs
// to the front end.
//
// There are two surfaces over one engine. The interactive one is the repl
// sub-package; the headless one is `run --print` in print.go. This file is what
// connects either to a session: resolve the arm, open the session with
// engine.Open (or, for the REPL's --fork flag, engine.Fork — see openAndDriveSession),
// hand the surface the engine's event stream to render and its consent requests
// to answer, and map the stop onto an exit code. `kopicode sessions`
// (sessions.go, KAN-941) is the third command here and needs none of that: it
// only lists what engine.ListSessions finds under .kopicode/sessions, so a human
// or a script has somewhere to find a session id for --resume or --fork.
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
	"strconv"
	"strings"

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
	"repl":     interactive,
	"run":      runPrint,
	"sessions": sessionsCmd,
	"version":  version,
}

// defaultCommand is what a bare `kopicode` does.
const defaultCommand = "repl"

// run is main with its streams and its exit code in the open, so the whole
// binary is callable from a test.
func run(args []string, stdout, stderr io.Writer) int {
	name, rest := command(args)
	cmd, ok := commands[name]
	if !ok {
		say(stderr, "kopicode: unknown command %q; try one of: repl, run, sessions, version\n", name)
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
	// --resume is deliberately minimal: the id of an existing session, and
	// nothing to help find one. `kopicode sessions` (KAN-941) is what finds
	// one; this flag stays a single string because resuming is one fact
	// about which session, not several.
	resume := fs.String("resume", "", "resume an existing session by id, instead of starting a new one")
	// --fork's argument names both the source session and the turn to
	// branch from in one string rather than two flags
	// (--fork-from/--fork-turn), because a caller supplying one and
	// forgetting the other is exactly the disagreement one flag prevents —
	// see parseForkSource. It is one flag and not a bare bool alongside
	// --resume's own session-id string, because "which session, which
	// turn" is a fact about the source and engine.Fork already refuses a
	// caller who gets it wrong; this flag's only job is to carry it.
	fork := fs.String("fork", "", "branch a new session from an existing one's history at "+
		"<session-id>:<turn>, replacing the working tree's contents with it (WARNING: destructive; "+
		"see `kopicode sessions` to find a session id)")
	if err := fs.Parse(args); err != nil {
		// flag has already printed the error and the usage.
		return exitUsage
	}
	setupLogging(*debug, stderr)

	if *resume != "" && *fork != "" {
		say(stderr, "kopicode: --resume and --fork are mutually exclusive: --resume continues a "+
			"session's own record in place, --fork starts a brand-new one seeded from another "+
			"session's history\n")
		return exitUsage
	}
	var forkSrc *engine.ForkSource
	if *fork != "" {
		src, ferr := parseForkSource(*fork)
		if ferr != nil {
			say(stderr, "kopicode: %v\n", ferr)
			return exitUsage
		}
		forkSrc = &src
	}

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

	opts := engine.Options{Dir: dir, Selection: selection}
	std := stdio(stderr)
	switch {
	case forkSrc != nil:
		// The confirmation runs after the arm resolved (an invocation that
		// would fail on an unknown model should fail before asking anyone
		// anything) and before anything is opened, locked or written —
		// engine.Fork's own read-only checks (does the source exist, does
		// the new id already have one) still run after this and before the
		// working tree is touched, so a confirmed fork that turns out to
		// name a bad source or turn still leaves the tree untouched.
		if !confirmFork(std, dir, *forkSrc) {
			return exitUsage
		}
		opts.Fork = forkSrc
		return forkSession(std, opts)
	case *resume != "":
		opts.SessionID = *resume
		opts.Resume = true
	}
	return session(std, opts)
}

// parseForkSource parses --fork's "<session-id>:<turn>" argument.
//
// One flag rather than --fork-from/--fork-turn: a session id and a turn are
// one fact about where to branch from, and two flags is the shape that lets
// a caller supply one and forget the other. The split is on the *last*
// colon rather than the first, so a caller-supplied session id containing
// one — unusual, since [engine.newSessionID]'s own ids never do, but
// [Options.SessionID] accepts any string — still splits correctly, because
// the turn itself is never anything but digits at the end.
func parseForkSource(raw string) (engine.ForkSource, error) {
	idx := strings.LastIndex(raw, ":")
	if idx <= 0 || idx == len(raw)-1 {
		return engine.ForkSource{}, fmt.Errorf(
			"--fork needs <session-id>:<turn>, for example --fork 20260819T101112Z-ab12cd34:2; got %q", raw)
	}
	id, turnStr := raw[:idx], raw[idx+1:]
	turn, err := strconv.Atoi(turnStr)
	if err != nil || turn < 0 {
		return engine.ForkSource{}, fmt.Errorf(
			"--fork's turn must be a non-negative integer; got %q in %q", turnStr, raw)
	}
	return engine.ForkSource{SessionID: id, Turn: turn}, nil
}

// forkConfirmPhrase is what confirmFork demands typed back in full. It names
// the operation rather than being a generic "yes", because the whole point
// is that it cannot be satisfied by the reflex a habitual [y/N] answer
// would supply.
const forkConfirmPhrase = "wipe and fork"

// confirmFork prints the destructive warning --fork implies and requires it
// typed back before anything on disk is touched.
//
// It is not [engine.ConsentRequest] machinery — see [repl.Confirm]'s own doc
// comment for why forking is a pre-session, human-only decision rather than
// a question the engine's permission gate could ever be asked. This
// function owns only the words; [repl.Confirm] owns how the question is put
// and answered, the same division main.go already holds session() to for
// everything else it prints.
func confirmFork(std streams, dir string, src engine.ForkSource) bool {
	lines := []string{
		"",
		"WARNING: --fork will DELETE every file directly under",
		"  " + dir,
		"except .git and .kopicode, and replace them with session " + src.SessionID + "'s",
		fmt.Sprintf("working tree as of turn %d. kopicode cannot undo this.", src.Turn),
		"",
	}
	if err := repl.Confirm(std.in, std.out, std.terminal, lines, forkConfirmPhrase); err != nil {
		say(std.err, "kopicode: fork cancelled; the working tree was not touched (%v)\n", err)
		return false
	}
	return true
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

// session opens a session with [engine.Open], drives the REPL over it, and
// reports the exit code.
//
// It takes the options rather than building them so that a test can supply a
// directory and a provider endpoint; everything about *how* a session is
// assembled is the engine's, which is the point of the constructor this calls.
func session(std streams, opts engine.Options) int {
	return openAndDriveSession(std, opts, engine.Open)
}

// forkSession opens a session with [engine.Fork] instead of [engine.Open],
// and otherwise drives it exactly as [session] does.
//
// It is a second, one-line function rather than a bool parameter on
// session, because the two constructors' signatures already agree —
// func(context.Context, engine.Options) (*engine.Session, error) — so the
// only thing that needs saying at the call site is which one, and a bool
// would still need a name explaining what it selects. main.go's --fork
// handling calls this after confirmFork, never before.
func forkSession(std streams, opts engine.Options) int {
	return openAndDriveSession(std, opts, engine.Fork)
}

// openAndDriveSession is [session] and [forkSession]'s shared body: build the loop,
// wire it to the engine's event stream and consent gate, open the session
// with whichever constructor the caller asked for, and drive it to an exit
// code.
//
// open is [engine.Open] or [engine.Fork]. Both refuse before touching
// anything when handed options meant for the other — Open on Options.Fork
// being set, Fork on it being nil — so a caller here that mismatched opts
// and open would get that refusal rather than a session started under the
// wrong intent; [session] and [forkSession] are what keep that mismatch
// from being possible to reach in the first place.
func openAndDriveSession(std streams, opts engine.Options, open func(context.Context, engine.Options) (*engine.Session, error)) int {
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
	// opts.ConsentMode is left at its zero value, engine.ConsentInteractive: a
	// human is answering through loop.Ask, so every PermissionDecided this
	// session journals is stamped permission.SourceUser (KAN-885).
	opts.Ask = loop.AnswerAsk
	// opts.AskMode is left at its zero value, engine.AskInteractive, for the
	// identical reason: a human is answering through loop.AnswerAsk, so every
	// journal.AskAnswered this session journals is stamped "user"
	// (docs/adr/0009-ask-tool-contract.md decision 4).

	ctx := context.Background()
	sess, err = open(ctx, opts)
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
		// A working tree already running a session (docs/SLICE-1.md §8) lands
		// on exit 4 by the same argument: the command line was right and the
		// machine was busy. Exit 2's definition in exit.go rules it out in as
		// many words — "nothing was opened, locked or written". The error names
		// the holder, its record and the lock file itself, so there is no
		// second sentence for this front end to add.
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
