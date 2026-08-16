// Package repl is kopicode's line-oriented interactive surface: the prompt
// loop, the streamed output, the permission prompt and the Ctrl-C handling
// (docs/adr/0004-line-oriented-repl.md, SLICE-1 affordance U1).
//
// # What it is, in one sentence
//
// It writes lines to stdout and never takes the screen. No alt screen, no
// frame loop, no pane layout — the terminal keeps its own scrollback and its
// own selection, ADR-0004 decision 1, and everything below follows from that
// one commitment.
//
// # Why it lives under cmd/
//
// The same reason [github.com/leejianrong/kopicode/cmd/kopicode/lineedit]
// does: this is presentation. The engine decides *that* consent is required
// and what a decision means; how the question looks, what a tool result reads
// like and whether a spinner turns are the surface's business and nothing the
// engine should be able to see (ADR-0003 decision 4, CLAUDE.md "the engine
// decides policy; the surfaces decide presentation").
//
// # There is one session record and this is not it
//
// Nothing here is a transcript. [Loop.Render] takes an [Event] whose fields
// are copied from a journal payload — one Event kind per journal event type,
// listed on [Kind] — so what the user reads is a rendering of the record
// rather than a parallel account of it (ADR-0002 decision 2, and the
// sibei-flow ADR-0013 lesson CLAUDE.md cites).
//
// Streamed model text is the one place that needs stating carefully, because
// the tokens reach the terminal before the journal has an AssistantMessage to
// derive from. The rule this package holds instead: [Loop.Stream] is an
// *optimistic prefix* of the record. When the message lands,
// Render(Event{Kind: [KindAssistant]}) prints the part of the journal's text
// that was not streamed — and if the streamed bytes were not a prefix of it,
// the divergence is printed rather than hidden, because the record wins. The
// user therefore sees exactly what the journal holds, no more and no less,
// whether or not the provider streamed.
//
// # Escapes, and where they are allowed to come from
//
// SLICE-1 requires piped output to be valid line-oriented text with no ANSI
// escapes. That is guaranteed here by construction rather than by stripping:
// when [Config.Interactive] is false the writer has no code path that emits an
// escape byte at all, progress is a total no-op, and styling returns its
// argument unchanged. TestNoEscapesWhenPiped drives a whole session through
// the non-interactive path and asserts on the captured bytes.
//
// When there *is* a terminal, the escape vocabulary is deliberately tiny:
// carriage return, erase-to-end-of-line, and SGR colour. No cursor-up, no
// scroll region, no alt screen — a progress indicator may repaint the current
// line and must never touch a line the user has already read (ADR-0004
// decision 3). TestProgressNeverLeavesTheCurrentLine holds the vocabulary.
//
// # Ctrl-C
//
// At the prompt it is [lineedit.ErrInterrupted] and discards the line in
// progress. During a turn it is a signal: raw mode is entered and left per
// ReadLine, so while the engine works the terminal is in its ordinary line
// discipline and SIGINT arrives through signal.Notify. The loop cancels that
// turn's context — which is what reaches the shell tool's process group,
// since the engine threads the context all the way down — renders the
// cancellation, and prompts again with the session open. Cancelling a turn
// never ends the session; only EOF, /exit, or a harness error does.
package repl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
	"github.com/leejianrong/kopicode/internal/engine"
)

// Surface is what a turn may print through while it runs.
//
// It is declared here, at the consumer, and implemented by [*Loop]. A turn
// runner is handed one rather than reaching for a package-level writer,
// because that is what lets the whole loop be driven in a test with the bytes
// captured — and what stops the engine from acquiring an opinion about
// presentation by accident.
type Surface interface {
	// Render prints one event from the engine's stream — a journal event as
	// it is recorded, or a delta as it arrives. See render.go.
	Render(e engine.Event)

	// Progress paints a status on the current line only, replacing whatever
	// it painted before. It is a no-op with no terminal. Passing "" clears
	// it.
	Progress(status string)

	// Ask puts a consent request to the user and returns the answer. It
	// denies on anything that is not an affirmative, including a closed
	// input and a cancelled turn.
	Ask(ctx context.Context, req engine.ConsentRequest) (engine.ConsentAnswer, error)
}

// TurnFunc runs one exchange: the user's prompt in, an [engine.Result] out.
//
// It is a function rather than an interface because there is exactly one
// implementation in the product — a call to engine.Session.Run — and several in
// tests. ctx is the turn's context and is cancelled by Ctrl-C.
//
// s is the surface the turn may print through, and in the product it goes
// unused: the session announces its own record through engine.Options.Events,
// wired to the very same [*Loop]. A test's fake turn has no record to announce
// and prints through s instead. Both are the one surface, which is what stops
// two sources interleaving.
type TurnFunc func(ctx context.Context, prompt string, s Surface) (engine.Result, error)

// CloseFunc ends the session. The engine's Close writes SessionEnded, so this
// runs on every exit path including a harness error.
type CloseFunc func(ctx context.Context) error

// Config assembles a [Loop]. Only Turn is required.
type Config struct {
	// In is where the user types. Defaults to os.Stdin.
	In io.Reader
	// Out is where everything the user reads goes. Defaults to os.Stdout.
	Out io.Writer

	// Interactive reports whether Out is a terminal. False is the safe
	// direction and the default: it means no escape byte is written at all.
	//
	// It is separate from Terminal because the two questions are different.
	// Terminal decides whether *input* can be read a key at a time; this
	// decides whether *output* may carry escapes. `kopicode > transcript`
	// with a keyboard attached is exactly the case that needs them apart.
	Interactive bool

	// Terminal is lineedit's raw-mode seam. Defaults to
	// lineedit.NonInteractive.
	Terminal lineedit.Terminal

	// Prompt is what the user types after. Plain text, no escapes —
	// lineedit measures it in runes to place the cursor.
	Prompt string

	// Turn runs one exchange. Required.
	Turn TurnFunc

	// Close ends the session. Optional; nil means there is nothing to close.
	Close CloseFunc

	// Interrupts delivers SIGINT while a turn is running. Optional: nil
	// means Ctrl-C cannot cancel a turn, which is what a test that is not
	// about cancellation wants. [Interrupts] builds the one a REPL should
	// use.
	Interrupts <-chan os.Signal
}

// Loop is the prompt loop. It is not safe for concurrent use: one goroutine
// reads the prompt and runs turns, and the only other goroutine it starts is
// the per-turn interrupt watcher, which touches nothing this one writes.
type Loop struct {
	out *out
	ed  *lineedit.Editor

	prompt  string
	turn    TurnFunc
	closeFn CloseFunc
	sig     <-chan os.Signal

	// promptEndsLine records whether the line editor finishes the line
	// itself. Its raw-mode path always does and its plain path never does;
	// see out.afterPrompt.
	promptEndsLine bool

	// streamed is what Stream has printed for the message currently being
	// assembled, so Render can print the journal's text without repeating
	// what the user already read. Reset when the message lands.
	streamed strings.Builder
}

// ErrNoTurn is a Config with no TurnFunc. A loop that prompts and can do
// nothing with the answer is worse than one that refuses to start.
var ErrNoTurn = errors.New("repl: Config.Turn is required")

// New builds a Loop from cfg.
func New(cfg Config) (*Loop, error) {
	if cfg.Turn == nil {
		return nil, ErrNoTurn
	}

	in := cfg.In
	if in == nil {
		in = os.Stdin
	}
	w := cfg.Out
	if w == nil {
		w = os.Stdout
	}
	term := cfg.Terminal
	if term == nil {
		term = lineedit.NonInteractive()
	}
	prompt := cfg.Prompt
	if prompt == "" {
		prompt = defaultPrompt
	}

	return &Loop{
		out: newOut(w, cfg.Interactive),
		ed: lineedit.New(lineedit.Config{
			In:       in,
			Out:      w,
			Terminal: term,
			Prompt:   prompt,
		}),
		prompt:         prompt,
		turn:           cfg.Turn,
		closeFn:        cfg.Close,
		sig:            cfg.Interrupts,
		promptEndsLine: term.IsInteractive(),
	}, nil
}

// defaultPrompt is deliberately plain. lineedit measures the prompt in runes
// to place the cursor and refuses to interpret escapes in it.
const defaultPrompt = "kopicode> "

// Run reads prompts and runs turns until the input ends, the user leaves, or
// the harness breaks.
//
// The returned [engine.Stop] is why the *session* ended, which is not the same
// as why the last turn stopped: a cancelled or failed turn returns to the
// prompt with the session alive, so it ends as StopCompleted like any other
// clean exit. The caller maps the stop to a process exit code; this package
// deliberately owns no exit-code table, because there is one and it is
// documented in cmd/kopicode.
//
// The error is non-nil only when the loop itself could not continue — the
// terminal broke, or a turn reported [engine.StopHarnessError]. The session is
// closed on every path, including that one.
func (l *Loop) Run(ctx context.Context) (engine.Stop, error) {
	stop, err := l.loop(ctx)

	// Close last and unconditionally: SessionEnded is the record's other
	// bookend, and a session that broke is the one whose ending most needs
	// to be on it.
	if l.closeFn != nil {
		if cerr := l.closeFn(ctx); cerr != nil {
			l.Fail(cerr.Error())
			if err == nil {
				stop, err = engine.StopHarnessError, cerr
			}
		}
	}
	l.out.flushLine()
	return stop, err
}

func (l *Loop) loop(ctx context.Context) (engine.Stop, error) {
	for {
		// The session's own context, not a turn's: a cancelled one means the
		// process is going away, which is an orderly end rather than a
		// failure, so it closes the record and reports no error.
		select {
		case <-ctx.Done():
			return engine.StopCancelled, nil
		default:
		}

		line, err := l.ed.ReadLine()
		l.out.afterPrompt(l.promptEndsLine)
		switch {
		case errors.Is(err, lineedit.ErrInterrupted):
			// The line in progress is gone and the session is not. lineedit
			// has already echoed ^C on the terminal path and written
			// nothing on the piped one.
			continue
		case errors.Is(err, io.EOF):
			return engine.StopCompleted, nil
		case err != nil:
			l.Fail(err.Error())
			return engine.StopHarnessError, fmt.Errorf("repl: reading the prompt: %w", err)
		}

		switch command(line) {
		case commandNone:
		case commandBlank:
			continue
		case commandExit:
			return engine.StopCompleted, nil
		}

		res, err := l.runTurn(ctx, line)
		if err != nil && res.Stop == engine.StopHarnessError {
			l.Fail(err.Error())
			return engine.StopHarnessError, err
		}
		l.renderOutcome(res, err)
	}
}

// runTurn runs one exchange under a context Ctrl-C can cancel.
//
// The turn's context is a child of the session's, so cancelling it reaches
// everything the engine threaded it into — the provider stream, the consent
// gate, and the shell tool's process group — and leaves the session's own
// context untouched, which is what "returns to the prompt with the session
// alive" means mechanically.
func (l *Loop) runTurn(ctx context.Context, prompt string) (engine.Result, error) {
	l.drainSignals()

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var interrupted atomic.Bool
	done := make(chan struct{})
	var wg sync.WaitGroup

	if l.sig != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-l.sig:
				interrupted.Store(true)
				cancel()
			case <-done:
			}
		}()
	}

	l.streamed.Reset()
	res, err := l.turn(turnCtx, prompt, l)

	close(done)
	wg.Wait()

	l.Progress("")
	l.out.flushLine()

	if interrupted.Load() {
		// The turn's own stop may be anything the loop managed to settle on
		// before the cancellation reached it; what the user did is not in
		// doubt, so the surface says so.
		l.Cancelled()
		return engine.Result{Stop: engine.StopCancelled, Turns: res.Turns, Tokens: res.Tokens}, nil
	}
	return res, err
}

// drainSignals discards interrupts that arrived while no turn was running.
//
// Without it a Ctrl-C typed at the prompt on a non-terminal stdin — where it
// is a signal rather than a byte lineedit can see — would sit in the channel
// and cancel the *next* turn, which is a turn the user did not interrupt.
func (l *Loop) drainSignals() {
	for {
		select {
		case <-l.sig:
		default:
			return
		}
	}
}

// renderOutcome prints why a turn stopped.
//
// Only [engine.StopHarnessError] ends the session, and it is handled by the
// caller. Everything else — a provider error that survived the client's own
// retries, the turn cap, the token budget, a cancellation — is reported and
// the prompt comes back, because an interactive session is a conversation and
// the next thing the user types may be the fix.
func (l *Loop) renderOutcome(res engine.Result, err error) {
	switch res.Stop {
	case engine.StopCompleted:
		return
	case engine.StopCancelled:
		// runTurn already rendered the interruption it caused. A cancellation
		// from elsewhere still deserves a line.
		if err != nil {
			l.Cancelled()
		}
		return
	case engine.StopUnspecified, engine.StopMaxTurns, engine.StopBudgetExhausted,
		engine.StopVerificationFailed, engine.StopProviderError, engine.StopHarnessError:
	}

	detail := ""
	if err != nil {
		detail = err.Error()
	}
	l.Stopped(res.Stop, detail)
}

// command classifies a line the user typed.
type lineCommand uint8

const (
	commandNone lineCommand = iota
	commandBlank
	commandExit
)

func command(line string) lineCommand {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return commandBlank
	case "/exit", "/quit":
		return commandExit
	default:
		return commandNone
	}
}
