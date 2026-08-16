//go:build unix

package repl_test

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/cmd/kopicode/repl"
	"github.com/leejianrong/kopicode/internal/engine"
)

// TestARealSIGINTCancelsTheTurn is the one test that uses a real signal rather
// than a channel send.
//
// Everything else about cancellation is driven through Config.Interrupts,
// which is faster and deterministic. This exists because that leaves one link
// unproven: that [repl.Interrupts] registers for the signal the terminal
// actually sends, and that registering it suppresses the default disposition
// so the process is cancelled rather than killed. A test process that gets
// killed here fails loudly, which is the right way to find that out.
//
// Unix only. Sending a signal to one's own process is not portable, and the
// SIGINT path on Windows is different enough that pretending otherwise would
// be a test asserting nothing there.
func TestARealSIGINTCancelsTheTurn(t *testing.T) {
	sig, stop := repl.Interrupts()
	t.Cleanup(stop)

	cancelled := make(chan struct{})

	var out strings.Builder
	loop, err := repl.New(repl.Config{
		In:         strings.NewReader("go\n"),
		Out:        &out,
		Interrupts: sig,
		Turn: func(ctx context.Context, _ string, s repl.Surface) (engine.Result, error) {
			s.Render(delta("working"))
			if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
				t.Errorf("sending SIGINT to this process: %v", err)
				return engine.Result{Stop: engine.StopCompleted}, nil
			}

			select {
			case <-ctx.Done():
				close(cancelled)
			case <-time.After(10 * time.Second):
				t.Error("SIGINT did not cancel the turn within 10s")
			}
			return engine.Result{Stop: engine.StopCancelled, Turns: 1}, ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("building the loop: %v", err)
	}

	if _, err := loop.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case <-cancelled:
	default:
		t.Fatal("the turn's context was never cancelled by a real SIGINT")
	}
	if !strings.Contains(out.String(), "[cancelled]") {
		t.Errorf("the interruption was not reported:\n%s", out.String())
	}
}

// TestStopIsSafeToCallTwice. It runs on every exit path a REPL has, including
// one that has already run it.
func TestStopIsSafeToCallTwice(t *testing.T) {
	_, stop := repl.Interrupts()
	stop()
	stop()
}
