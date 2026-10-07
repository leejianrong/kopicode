package engine

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
)

// This file is KAN-1025's implementation of KAN-1024's design: a genuinely
// new session's bootstrap feeds the repository's own AGENTS.md into the
// conversation, once, before the first real user turn — see
// [journal.ProjectInstructionsLoaded]'s own doc comment for what is recorded
// and why this does not touch the harness configuration hash.
//
// wrapProjectInstructions is the one framing string both this file's
// bootstrap call and resume.go's replay case use, factored out so the two
// call sites cannot drift (KAN-1024's design comment 2).

// wrapProjectInstructions frames content as a message the model can tell
// apart from the human's own words, the same direction
// [Assembler.AppendUser]'s own doc comment describes: "how the loop feeds
// back anything the wire has no better role for."
//
// This wrapper is a mechanical framing string, not an experiment variable —
// it is an engine-owned constant rather than a harness [HarnessConfig] field,
// per KAN-1024's design.
func wrapProjectInstructions(content string) string {
	return "The repository's own AGENTS.md:\n\n" + content
}

// wrapInstructions frames content by the scope its event recorded: the user's
// own file is told apart from the repository's, so the model can weigh a
// repository rule against a personal preference.
func wrapInstructions(scope, content string) string {
	if scope == journal.InstructionsScopeSkills {
		return content
	}
	if scope == journal.InstructionsScopeUser {
		return "The user's own AGENTS.md (their preferences across every project):\n\n" + content
	}
	return wrapProjectInstructions(content)
}

// loadProjectInstructions finds dir's own AGENTS.md and, if one exists,
// journals it and feeds it into e's assembler.
//
// It is called from [openSession]'s bootstrap, and only for a genuinely new
// session: never for a resumed one, whose own prior
// [journal.ProjectInstructionsLoaded] (if any) is already reconstructed by
// replayHistory from the replayed history, and never for a forked one,
// whose source session's own event of this type travels in the copied block
// (see readForkSource's own comment on why Turn 0 is not excluded for this
// one type). Calling this again on either path would journal — and feed to
// the model — a second copy of content the session already carries.
func (e *Engine) loadProjectInstructions(ctx context.Context, dir, userPath string) error {
	// The user's own file goes first, so the repository's, which is more
	// specific, is the later and so the more recent of the two.
	if userPath != "" {
		data, err := os.ReadFile(userPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return fmt.Errorf("engine: loading %s: %w", userPath, err)
		default:
			if _, err := e.append(ctx, 0, journal.ProjectInstructionsLoaded{
				Path:    userPath,
				Content: journal.InlineText(string(data)),
				Scope:   journal.InstructionsScopeUser,
			}); err != nil {
				return fmt.Errorf("engine: recording %s: %w", userPath, err)
			}
			e.asm.AppendUser(wrapInstructions(journal.InstructionsScopeUser, string(data)))
		}
	}
	af, ok, err := harness.LoadAgentsFile(dir)
	if err != nil {
		return fmt.Errorf("engine: loading %s: %w", harness.AgentsFileName, err)
	}
	if !ok {
		return nil
	}
	if _, err := e.append(ctx, 0, journal.ProjectInstructionsLoaded{
		Path:    af.Path,
		Content: journal.InlineText(af.Content),
	}); err != nil {
		return fmt.Errorf("engine: recording %s: %w", harness.AgentsFileName, err)
	}
	e.asm.AppendUser(wrapProjectInstructions(af.Content))
	return nil
}
