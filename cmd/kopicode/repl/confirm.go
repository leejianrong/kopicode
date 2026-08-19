package repl

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/leejianrong/kopicode/cmd/kopicode/lineedit"
)

// This file is the confirmation mechanism KAN-941's --fork needs, and it is
// deliberately not a reuse of Ask/consent.go's machinery, even though the
// visual style below borrows from it.
//
// [Loop.Ask] answers an [engine.ConsentRequest] — a question the model's
// tool call raised, mid-session, that internal/permission decided needs a
// human — and its answer is journaled as a PermissionDecided event. A
// --fork flag is not that kind of question: it is a decision the human
// already made on the command line, before any session exists to journal
// anything into, and the thing it is about to do — wipe every file directly
// under the destination directory except .git and .kopicode
// (engine.clearWorkingTreeForFork) — is not a tool call at all. Routing it
// through Ask would either invent a ConsentRequest for something the engine
// never asked about, or make this package's one human-facing "are you sure"
// mechanism secretly mean two different things depending on when it runs.
// So this is its own small function: styled the same way — plain lines, an
// explicit typed reply, no [y/N] a bare Enter can satisfy — because the goal
// is the same one Ask already solved for a different case (a habitual
// reflex answer must not be mistaken for an informed one), just needed
// before there is a Loop, a session, or anything to render a rendering of.

// ErrConfirmDenied reports that the human either declined — typed anything
// other than the exact word this call demanded — or could not be asked at
// all (closed input, an interrupted read, a broken terminal). The two are
// deliberately not distinguished by a separate error, because both answers
// are the same one: an unaskable question is not a yes, the same rule [Ask]
// holds itself to for the case ErrNoConsent's own doc comment argues.
var ErrConfirmDenied = errors.New("repl: not confirmed")

// Confirm prints lines and then asks the user to type mustType back exactly,
// returning nil only when they did.
//
// It takes a reader, a writer and a [lineedit.Terminal] rather than a *Loop,
// because a *Loop is a running session's surface and this question is asked
// before any session exists — main.go's --fork handling calls it between
// resolving the arm and opening one. The three parameters are exactly what
// [lineedit.Config] needs and exactly what main.go's own stdio() already
// builds, so a caller that already has a streams value has everything this
// needs with nothing to translate.
//
// There is no [y]es/[N]o shorthand. A destructive, irreversible operation
// needs to be immune to the reflex a single keystroke invites, so the
// prompt asks for mustType typed back in full; anything else, including
// a bare Enter, is [ErrConfirmDenied].
func Confirm(in io.Reader, out io.Writer, term lineedit.Terminal, lines []string, mustType string) error {
	for _, ln := range lines {
		if _, err := fmt.Fprintln(out, ln); err != nil {
			return fmt.Errorf("%w: %w", ErrConfirmDenied, err)
		}
	}

	ed := lineedit.New(lineedit.Config{
		In:       in,
		Out:      out,
		Terminal: term,
		Prompt:   fmt.Sprintf("type %q to continue, anything else cancels: ", mustType),
	})
	reply, err := ed.ReadLine()
	if err != nil {
		// EOF, an interrupted read, or a broken terminal are all "could not
		// be asked" — never answered as a yes by default.
		return fmt.Errorf("%w: %w", ErrConfirmDenied, err)
	}
	if strings.TrimSpace(reply) != mustType {
		return ErrConfirmDenied
	}
	return nil
}
