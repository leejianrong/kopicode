package engine

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
)

// This file is KAN-1025's implementation of KAN-1024's design, extended by
// ADR-0032: a genuinely new session's bootstrap feeds the repository's own
// AGENTS.md files into the conversation, once, before the first real user turn
// — see [journal.ProjectInstructionsLoaded]'s own doc comment for what is
// recorded and why this does not touch the harness configuration hash.
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
	if scope == journal.InstructionsScopeSkills || scope == journal.InstructionsScopeHandoff ||
		scope == journal.InstructionsScopeEnvironment {
		return content
	}
	if scope == journal.InstructionsScopeUser {
		return "The user's own AGENTS.md (their preferences across every project):\n\n" + content
	}
	return wrapProjectInstructions(content)
}

// InstructionFile is one AGENTS.md a session's bootstrap found (ADR-0032), for
// the listing a front end shows. Loaded is false for a file the byte cap
// excluded: it is on the record, the model was not shown it.
type InstructionFile struct {
	Path   string
	User   bool
	Bytes  int
	Loaded bool
}

// Instructions lists the AGENTS.md files found at this session's start, the
// user's own first and the nearest repository file last. It is empty for a
// resumed or forked session, which reads them from the journal instead.
func (s *Session) Instructions() []InstructionFile {
	return append([]InstructionFile(nil), s.engine.instructions...)
}

// loadProjectInstructions finds the user's AGENTS.md and every repository
// AGENTS.md from the root down to dir, and journals and feeds each into e's
// assembler, one event per file, furthest first and the nearest last
// (ADR-0032).
//
// The total is held to [harness.AgentsCapBytes]: when it is over, the furthest
// files are dropped whole and still journalled, with Dropped set and no content.
//
// It is called from [openSession]'s bootstrap, and only for a genuinely new
// session: never for a resumed one, whose own prior
// [journal.ProjectInstructionsLoaded] events are already reconstructed by
// replayHistory from the replayed history, and never for a forked one, whose
// source session's own events of this type travel in the copied block (see
// readForkSource's own comment on why Turn 0 is not excluded for this one
// type). Calling this again on either path would journal — and feed to the
// model — a second copy of content the session already carries.
func (e *Engine) loadProjectInstructions(ctx context.Context, dir, userPath string) error {
	type entry struct {
		path    string
		scope   string
		content string
	}
	var entries []entry
	// The user's own file goes first, so the repository's, which is more
	// specific, is the later and so the more recent.
	if userPath != "" {
		data, err := os.ReadFile(userPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return fmt.Errorf("engine: loading %s: %w", userPath, err)
		default:
			entries = append(entries, entry{userPath, journal.InstructionsScopeUser, string(data)})
		}
	}
	chain, err := harness.LoadAgentsChain(dir)
	if err != nil {
		return fmt.Errorf("engine: loading %s: %w", harness.AgentsFileName, err)
	}
	for _, af := range chain {
		entries = append(entries, entry{af.Path, "", af.Content})
	}
	sizes := make([]int, len(entries))
	for i, en := range entries {
		sizes[i] = len(en.content)
	}
	keep := harness.FitWithinCap(sizes, harness.AgentsCapBytes)
	for i, en := range entries {
		payload := journal.ProjectInstructionsLoaded{Path: en.path, Scope: en.scope}
		if keep[i] {
			payload.Content = journal.InlineText(en.content)
		} else {
			payload.Dropped = true
			payload.Bytes = int64(len(en.content))
		}
		if _, err := e.append(ctx, 0, payload); err != nil {
			return fmt.Errorf("engine: recording %s: %w", en.path, err)
		}
		if keep[i] {
			e.asm.AppendUser(wrapInstructions(en.scope, en.content))
		}
		e.instructions = append(e.instructions, InstructionFile{
			Path: en.path, User: en.scope == journal.InstructionsScopeUser, Bytes: len(en.content), Loaded: keep[i],
		})
	}
	return nil
}
