package repl

import (
	"os"
	"os/signal"
)

// Interrupts is the channel a [Loop] watches for Ctrl-C, and the function that
// stops watching it.
//
// It is a constructor rather than something New does for itself, for two
// reasons. Registering a signal handler is process-global state, and a
// library that grabbed it on construction would make two Loops in one process
// — which a test does routinely — fight over SIGINT. And [Config.Interrupts]
// is an ordinary channel, so a test drives cancellation by sending on one
// rather than by signalling itself.
//
// Call stop before the process exits. Until it is called, SIGINT no longer
// terminates the process: signal.Notify suppresses the default disposition,
// which is exactly the point — Ctrl-C must cancel the turn rather than kill
// the session — and leaving it registered after the REPL has finished would
// make the binary ignore a Ctrl-C it can no longer act on.
//
// The buffer is one, deliberately. signal.Notify drops a signal when the
// buffer is full rather than blocking, and dropping a second Ctrl-C while the
// first is still unhandled is the correct behaviour: it is the same request,
// and queueing it would cancel the *next* turn too.
//
// # What this does not cover
//
// A Ctrl-C typed at the prompt on a real terminal never reaches here. Raw mode
// turns off ISIG, so it arrives as byte 0x03 and lineedit returns
// [lineedit.ErrInterrupted]; that is the path this channel deliberately does
// not duplicate, because a second keyboard reader racing the first is the
// design ADR-0004 refuses.
func Interrupts() (ch <-chan os.Signal, stop func()) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	return c, func() { signal.Stop(c) }
}
