package engine

import (
	"context"
	"fmt"

	"github.com/leejianrong/kopicode/internal/journal"
)

// This file is the cancellation's half of the record.
//
// Cancelling a turn already worked before KAN-857: the context reaches the
// provider stream, the consent gate, the tools and the shell's process group,
// and the REPL comes back to a live prompt. What was missing was that the
// journal said nothing about it. The record stopped, and "the user pressed
// Ctrl-C", "the process was killed" and "the disk filled up mid-write" were the
// same bytes — an absence, which ADR-0002 decision 2 does not allow to stand in
// for a record.

// The vocabulary of journal.TurnCancelled.Phase. This package is its source of
// truth; internal/journal documents the values as a wire contract and imports
// nothing to get them.
//
// The phases name what was *in flight*, not where the loop was standing when it
// looked. The loop checks its context at the top of every turn and every
// attempt, so a cancellation that interrupted a shell command would otherwise
// be recorded as "between_steps" one step later — technically true about the
// loop and useless about the session. [Engine.noteCancelled] therefore keeps
// the first observation and discards the rest.
const (
	// phaseProviderStream is a reply that was still arriving. This is the
	// mid-stream case: a ProviderRequest with no ProviderResponse after it.
	phaseProviderStream = "provider_stream"

	// phaseToolCall is a tool that was running. For run_shell this is also the
	// record that a process group was killed; the ToolResult beside it carries
	// internal/tools' "cancelled" fault.
	phaseToolCall = "tool_call"

	// phaseVerification is the project's own verification command (SLICE-1 §5).
	phaseVerification = "verification"

	// phaseBetweenSteps is nothing in flight: the loop reached a checkpoint with
	// the context already cancelled. A turn refused before it started reads this
	// way, and so does a cancellation that landed in the gap between two
	// provider calls.
	phaseBetweenSteps = "between_steps"
)

// noteCancelled records what was in flight when the cancellation was first
// seen. Later notices are dropped: see the vocabulary's comment above.
func (e *Engine) noteCancelled(phase string) {
	if e.cancelPhase == "" {
		e.cancelPhase = phase
	}
}

// noteProviderStop classifies a failed provider call and, when it turns out to
// be a cancellation, records that the reply was still in flight.
//
// It exists so that [providerStop] stays a pure function of the error — it is
// also read by tests — while the one caller that needs the side effect gets it
// without repeating the comparison at two call sites.
func (e *Engine) noteProviderStop(err error) Stop {
	stop := providerStop(err)
	if stop == StopCancelled {
		e.noteCancelled(phaseProviderStream)
	}
	return stop
}

// journalCancellation writes the TurnCancelled event for turn.
//
// ctx is the cancelled context and is passed anyway: journal.FileJournal
// deliberately ignores a cancelled context on Append, which is the whole reason
// the event explaining why a turn stopped can be written at all. Handing it a
// fresh context here would hide a real journal failure behind a difference
// nobody chose.
//
// cause is the context error. It may be nil in principle — a stop settled as
// cancelled with no error behind it — and the record says so honestly rather
// than inventing a reason.
func (e *Engine) journalCancellation(ctx context.Context, turn int, cause error) error {
	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	phase := e.cancelPhase
	if phase == "" {
		// Reachable only if a stop settles as cancelled without any of the
		// observation points having fired. "between_steps" is the honest reading
		// — nothing was in flight — and it is written rather than left empty,
		// because an empty phase would be a field nobody filled in and a reader
		// could not tell it from a build that predates the field.
		phase = phaseBetweenSteps
	}
	_, err := e.append(ctx, turn, journal.TurnCancelled{
		Phase:  phase,
		Detail: journal.InlineText(detail),
	})
	if err != nil {
		return fmt.Errorf("engine: recording the cancellation of turn %d: %w", turn, err)
	}
	return nil
}
