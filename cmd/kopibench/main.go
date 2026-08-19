// Command kopibench is the headless benchmark runner.
//
// It is a first-class front end, not a test utility: a benchmark cannot drive a
// REPL, which is why the engine has two surfaces from the first commit
// (docs/adr/0003-single-repo-internal-engine.md).
//
// One invocation is one arm — (model x harness configuration x provider pin),
// per ADR-0007 — over the frozen corpus, and a paired A/B is two invocations of
// one binary. `--model` and `--harness` select the arm; everything about how the
// run is executed lives behind internal/bench, because ADR-0003 decision 3
// allows a front end exactly three internal imports and this file's job is to
// parse flags, print, and choose an exit code.
//
//	kopibench run --provider mock --corpus bench/tasks --save result-a.json
//	kopibench run --provider mock --corpus bench/tasks --model minimax/minimax-m2 --save result-b.json
//	kopibench compare result-a.json result-b.json
//
// `run --save` is the half that makes the second line possible at all: two
// invocations of one binary only add up to a paired A/B (KAN-943) if the first
// invocation's [bench.RunResult] survives to be loaded back for the second.
// `compare` is the other half — it holds no knowledge of how a result was
// produced, only [bench.Compare] over two loaded ones, which is what lets it
// score a run that has not been re-executed to compare it.
//
// # Exit codes
//
// From docs/SLICE-1.md build step 14, read for a *benchmark* rather than for a
// single session:
//
//	0  the corpus ran and every task was put a question
//	1  at least one task could not be run — a session error, a panic, an oracle
//	   that could not be started. A task the model simply failed is 0: that is
//	   the measurement, not an error.
//	2  usage
//	4  the run could not be carried out at all
//
// The distinction is what lets `make bench-smoke` gate a PR. The smoke run
// replays traffic that solves nothing, so every oracle fails; if a failing
// oracle were a failing command the gate would be red by construction and would
// measure nothing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/leejianrong/kopicode/internal/bench"
	"github.com/leejianrong/kopicode/internal/build"
	"github.com/leejianrong/kopicode/internal/engine"
)

// Exit codes, from docs/SLICE-1.md build step 14.
const (
	exitOK       = 0
	exitTaskFail = 1
	exitUsage    = 2
	exitHarness  = 4
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's body with its inputs and outputs injected, so the front end's
// exit codes are assertable rather than only observable through a subprocess.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "--version" || args[0] == "version") {
		// The identity a bench result is attributed to. Printing it from the
		// same call the record uses is what keeps a report and a journal from
		// disagreeing about which binary produced a number.
		printf(stdout, "%s\n", build.Current())
		return exitOK
	}

	// `run` and `compare` are the only subcommands there are. Neither is
	// implied: `kopibench` with no arguments spending two dollars because the
	// default was to run is not a default anybody wants, and a `compare` that
	// ran without being asked would be spending nothing but would still be a
	// surprise.
	if len(args) == 0 {
		printf(stderr, "usage: kopibench run [flags]\n"+
			"       kopibench compare <result-a.json> <result-b.json>\n"+
			"       kopibench --version\n")
		return exitUsage
	}
	switch args[0] {
	case "run":
		return runRun(args[1:], stdout, stderr)
	case "compare":
		return runCompare(args[1:], stdout, stderr)
	default:
		printf(stderr, "usage: kopibench run [flags]\n"+
			"       kopibench compare <result-a.json> <result-b.json>\n"+
			"       kopibench --version\n")
		return exitUsage
	}
}

// runRun is `kopibench run`'s body.
func runRun(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kopibench run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	overrides := engine.BindSelectionFlags(fs)
	corpusDir := fs.String("corpus", "bench/tasks", "the frozen task corpus")
	providerKind := fs.String("provider", string(bench.ProviderLive),
		"where model traffic comes from: live (spends money) or mock (replays recorded traffic)")
	commit := fs.String("commit", "", "the commit to check every task worktree out from (default HEAD)")
	jobs := fs.Int("jobs", 0, "how many tasks may run at once (default 4, or the core count if lower)")
	keep := fs.Bool("keep-worktrees", false,
		"leave every task worktree behind for a post-mortem; the NEXT run reclaims them")
	debug := fs.Bool("debug", false, "engine diagnostics on stderr")
	save := fs.String("save", "",
		"write this run's result as JSON here, so `kopibench compare` can pair it with another run later")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	setupLogging(*debug, stderr)

	kind := bench.ProviderKind(*providerKind)
	if kind != bench.ProviderLive && kind != bench.ProviderMock {
		printf(stderr, "kopibench: unknown provider %q, want %q or %q\n",
			*providerKind, bench.ProviderLive, bench.ProviderMock)
		return exitUsage
	}

	dir, err := os.Getwd()
	if err != nil {
		printf(stderr, "kopibench: %v\n", err)
		return exitHarness
	}

	// ADR-0007 decision 4 is worth more here than it is in the REPL: deferring
	// an unknown model to the provider means the refusal arrives after N tasks
	// have already run, as a provider error inside a corpus rather than a
	// refusal before it. This call reads a file and touches nothing else, so a
	// refused invocation leaves no half-started run behind.
	selection, err := engine.ResolveSelection(dir, *overrides)
	if err != nil {
		printf(stderr, "kopibench: %v\n", err)
		if engine.IsSelectionUsageError(err) {
			return exitUsage
		}
		return exitHarness
	}
	slog.Debug("arm resolved", "selection", selection)

	root, err := filepath.Abs(*corpusDir)
	if err != nil {
		printf(stderr, "kopibench: %v\n", err)
		return exitUsage
	}

	// Ctrl-C cancels the run. Every worktree is still reclaimed: the runner
	// defers removal on a context detached from this one, which is the half of
	// cancellation that a naive implementation gets wrong and only notices
	// when the disk is full.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	id := build.Current()
	result, err := bench.RunCorpus(ctx, bench.Options{
		CorpusDir: root,
		Selection: selection,
		Build: bench.BuildIdentity{
			Version:   id.Version,
			Commit:    id.Commit,
			TreeState: string(id.TreeState),
			Source:    id.Source,
		},
		Provider:      kind,
		Commit:        *commit,
		Jobs:          *jobs,
		KeepWorktrees: *keep,
	})
	if result != nil {
		if werr := bench.WriteReport(stdout, result); werr != nil {
			printf(stderr, "kopibench: writing the report: %v\n", werr)
		}
		// Saved even when the run errored or left tasks unclassified: a
		// partial result is still the result, and a post-mortem `compare`
		// needs the same file a clean run would have produced.
		if *save != "" {
			if serr := saveResultFile(*save, result); serr != nil {
				printf(stderr, "kopibench: %v\n", serr)
				return exitHarness
			}
		}
	}
	if err != nil {
		printf(stderr, "kopibench: %v\n", err)
		if errors.Is(err, context.Canceled) {
			return exitTaskFail
		}
		return exitHarness
	}
	if result.Errored() > 0 {
		printf(stderr, "kopibench: %d task(s) could not be run; see the report\n", result.Errored())
		return exitTaskFail
	}
	return exitOK
}

// saveResultFile writes result to path as JSON via [bench.SaveResult].
//
// It is a named function rather than an inline closure so `compare`'s own
// file-opening code below reads as the same shape doing the opposite thing.
func saveResultFile(path string, result *bench.RunResult) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("saving result to %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := bench.SaveResult(f, result); err != nil {
		return fmt.Errorf("saving result to %s: %w", path, err)
	}
	return f.Close()
}

// runCompare is `kopibench compare`'s body: load two [bench.RunResult]s that
// `run --save` wrote, score them with [bench.Compare], and print the report.
//
// It holds no knowledge of how a result was produced — not the corpus, not
// the provider, not the engine — because everything it needs is already in
// the two files. That is the whole point of splitting `run --save` and
// `compare` into two invocations of one binary rather than one invocation
// that runs both arms back to back: the two runs do not have to happen on the
// same machine, in the same process, or even on the same day.
func runCompare(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("kopibench compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 2 {
		printf(stderr, "usage: kopibench compare <result-a.json> <result-b.json>\n")
		return exitUsage
	}

	pathA, pathB := fs.Arg(0), fs.Arg(1)
	runA, err := loadResultFile(pathA)
	if err != nil {
		printf(stderr, "kopibench: %v\n", err)
		return exitUsage
	}
	runB, err := loadResultFile(pathB)
	if err != nil {
		printf(stderr, "kopibench: %v\n", err)
		return exitUsage
	}

	res, err := bench.Compare(runA.Scored(), runB.Scored())
	if err != nil {
		printf(stderr, "kopibench: %v\n", err)
		return exitHarness
	}

	if werr := bench.WriteCompareReport(stdout, res, runA, runB); werr != nil {
		printf(stderr, "kopibench: writing the comparison report: %v\n", werr)
		return exitHarness
	}
	return exitOK
}

// loadResultFile reads back a [bench.RunResult] `run --save` wrote.
func loadResultFile(path string) (*bench.RunResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("loading result from %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	r, err := bench.LoadResult(f)
	if err != nil {
		return nil, fmt.Errorf("loading result from %s: %w", path, err)
	}
	return r, nil
}

// printf writes to an injected stream.
//
// The error is deliberately dropped and the helper exists so that dropping it
// is a decision made once rather than four times: this is a front end reporting
// to a terminal it does not own, and there is nowhere left to report a failure
// to report. The exit code is the part that has to be right.
func printf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// setupLogging puts engine diagnostics on stderr under --debug and discards
// them otherwise. Off by default, and never session content: the journal owns
// that.
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
