package tools_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/leejianrong/kopicode/internal/tools"
)

// sometimeLastCentury is a deadline that has comfortably passed, so the
// deadline cases need neither a clock nor a wait.
var sometimeLastCentury = time.Date(1991, time.August, 6, 0, 0, 0, 0, time.UTC)

// This file is KAN-808's acceptance criterion and the whole of it: every tool
// classifies a cancellation the same way, and none of them produces a signal
// the bench classifier reads as a harness fault.
//
// **The observable is tools.FaultOf(err), and its wire form is "cancelled".**
// That string is what the engine puts in journal.ToolResult.ErrorKind, which is
// what SLICE-1 §9's classifier derives from — so KAN-797 consumes this and does
// not invent a parallel rule. Two things must both hold, in opposite
// directions:
//
//   - not "internal". ADR-0006 §3 buckets a tool returning an internal error as
//     `harness`, and SLICE-1's acceptance criterion for the slice is zero
//     harness failures. Every Ctrl-C would count against it.
//   - not "" either. A nil error looks like a clean stop, and §9's `model` arm
//     is "everything else" — so the silent version does not merely lose the
//     cancellation, it charges it to the model. That is the same laundering
//     ADR-0006 exists to prevent, running the other way.
//
// Tools with a result to report — run_shell, write_file — return it *alongside*
// the error rather than instead of it. The result is for the caller, the fault
// is for the classifier, and neither substitutes for the other.

// cancellable is one tool, reduced to the only thing this file cares about:
// call it with a context that is already over, and hand back the error.
type cancellable struct {
	// method is the exported method name on *tools.Set, which is what
	// TestEveryToolClassifiesCancellationTheSameWay checks the table against.
	method string
	call   func(context.Context, *tools.Set) error
}

// cancellableTools is the table. A tool added to *tools.Set without a row here
// fails the completeness check below, so a sixth tool cannot quietly get this
// wrong.
var cancellableTools = []cancellable{
	{"ReadFile", func(ctx context.Context, s *tools.Set) error {
		_, err := s.ReadFile(ctx, tools.ReadRequest{Path: "main.go"})
		return err
	}},
	{"ListDir", func(ctx context.Context, s *tools.Set) error {
		_, err := s.ListDir(ctx, tools.ListRequest{Recursive: true})
		return err
	}},
	{"Grep", func(ctx context.Context, s *tools.Set) error {
		_, err := s.Grep(ctx, tools.GrepRequest{Pattern: "package"})
		return err
	}},
	{"RunShell", func(ctx context.Context, s *tools.Set) error {
		_, err := s.RunShell(ctx, tools.ShellRequest{Command: "true"})
		return err
	}},
	{"WriteFile", func(ctx context.Context, s *tools.Set) error {
		_, err := s.WriteFile(ctx, tools.WriteRequest{Path: "new.txt", Content: "x\n"})
		return err
	}},
}

// endedContext is a context that is already over, for each of the two ways that
// happens. Both are cancellations and both classify identically — the reason
// stays legible through errors.Is on the cause, which is why FaultCancelled is
// one value and not two.
var endedContexts = map[string]func(*testing.T) context.Context{
	"user cancelled": func(t *testing.T) context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		t.Cleanup(cancel)
		return ctx
	},
	"deadline exceeded": func(t *testing.T) context.Context {
		// A deadline already in the past, so nothing waits for a clock.
		ctx, cancel := context.WithDeadline(context.Background(), sometimeLastCentury)
		t.Cleanup(cancel)
		return ctx
	},
}

func TestCancellationIsNeverAHarnessFault(t *testing.T) {
	for name, mkctx := range endedContexts {
		for _, tool := range cancellableTools {
			t.Run(name+"/"+tool.method, func(t *testing.T) {
				f := newFixture(t, map[string]string{"main.go": sample})
				s := f.set(t)

				err := tool.call(mkctx(t), s)
				if err == nil {
					t.Fatalf("%s on an ended context returned a nil error; "+
						"SLICE-1 §9 reads a clean stop as `model`, so a silent "+
						"cancellation is charged to the model", tool.method)
				}

				got := tools.FaultOf(err)
				if got == tools.FaultInternal {
					t.Errorf("%s: fault = %q; ADR-0006 §3 buckets that as `harness`, "+
						"and a cancellation is nobody's failure", tool.method, got)
				}
				if got != tools.FaultCancelled {
					t.Errorf("%s: fault = %q, want %q", tool.method, got, tools.FaultCancelled)
				}
				if wire := got.String(); wire != "cancelled" {
					t.Errorf("%s: ErrorKind would be %q, want %q", tool.method, wire, "cancelled")
				}
			})
		}
	}
}

// TestCancellationKeepsItsCause is the reason FaultCancelled does not split
// into two values. The classifier treats a deadline and a Ctrl-C alike — both
// mean the trial did not finish — and a report that wants to tell them apart
// reads the cause instead of a second bucket.
func TestCancellationKeepsItsCause(t *testing.T) {
	causes := map[string]error{
		"user cancelled":    context.Canceled,
		"deadline exceeded": context.DeadlineExceeded,
	}
	for name, want := range causes {
		for _, tool := range cancellableTools {
			t.Run(name+"/"+tool.method, func(t *testing.T) {
				f := newFixture(t, map[string]string{"main.go": sample})
				s := f.set(t)

				err := tool.call(endedContexts[name](t), s)
				if !errors.Is(err, want) {
					t.Errorf("%s: error %v does not wrap %v", tool.method, err, want)
				}
			})
		}
	}
}

// TestEveryToolClassifiesCancellationTheSameWay is the guard the card asks for:
// a sixth tool that gets cancellation wrong must fail the suite, and a table
// nobody remembers to extend cannot do that on its own.
//
// Every exported method on *tools.Set taking a context is a tool by definition —
// a context parameter is how this package says "this can be cancelled" — so the
// set of methods is derived rather than listed, and adding one without a row in
// cancellableTools is what fails.
func TestEveryToolClassifiesCancellationTheSameWay(t *testing.T) {
	tabled := make(map[string]bool, len(cancellableTools))
	for _, tool := range cancellableTools {
		if tabled[tool.method] {
			t.Errorf("%s appears twice in cancellableTools", tool.method)
		}
		tabled[tool.method] = true
	}

	set := reflect.TypeOf(&tools.Set{})
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()

	found := 0
	for i := range set.NumMethod() {
		m := set.Method(i)
		// In(0) is the receiver, so the context is In(1).
		if m.Type.NumIn() < 2 || m.Type.In(1) != ctxType {
			continue
		}
		found++
		if !tabled[m.Name] {
			t.Errorf("(*tools.Set).%s takes a context but has no row in cancellableTools; "+
				"add one, so that KAN-808's rule — a cancellation is never a harness "+
				"fault — is asserted for it too", m.Name)
		}
		delete(tabled, m.Name)
	}
	for name := range tabled {
		t.Errorf("cancellableTools has a row for %q, which is not a context-taking "+
			"method on *tools.Set; the reflection guard is asserting nothing for it", name)
	}
	if found == 0 {
		t.Fatal("found no context-taking methods on *tools.Set; the guard is broken, not the tools")
	}
}

// TestFaultCancelledIsTheWireForm pins the string the engine copies into
// journal.ToolResult.ErrorKind. That field's own doc lists "", "task" and
// "internal"; KAN-808 adds this fourth value and KAN-797 is the consumer.
func TestFaultCancelledIsTheWireForm(t *testing.T) {
	if got := tools.FaultCancelled.String(); got != "cancelled" {
		t.Errorf("FaultCancelled.String() = %q, want %q", got, "cancelled")
	}
	for _, other := range []tools.Fault{tools.FaultNone, tools.FaultTask, tools.FaultInternal} {
		if other.String() == tools.FaultCancelled.String() {
			t.Errorf("%d and FaultCancelled share the wire form %q", other, other.String())
		}
	}
}
