package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/leejianrong/kopicode/internal/harness"
	"github.com/leejianrong/kopicode/internal/journal"
	"github.com/leejianrong/kopicode/internal/skills"
)

// Skill is one discovered skill (ADR-0025). The type is the skills package's,
// re-exported so a front end needs no import of its own.
type Skill = skills.Skill

// SkillRoots lists where skills are looked for, in precedence order, for a
// session started in dir: the repository's `.agents/skills` and
// `.kopicode/skills`, the user's own directory, the user config's extra paths,
// and `~/.claude/skills` and `~/.agents/skills` as read-only fallbacks.
//
// Only the repository's roots, and only when they lie inside dir, are marked as
// ones the model's file tools can read.
func SkillRoots(dir string, extra []string) []skills.Root {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	repo := repoRoot(abs)
	home, _ := os.UserHomeDir()

	within := func(p string) bool {
		rel, err := filepath.Rel(abs, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	var roots []skills.Root
	for _, rel := range []string{filepath.Join(".agents", "skills"), filepath.Join(".kopicode", "skills")} {
		p := filepath.Join(repo, rel)
		roots = append(roots, skills.Root{Dir: p, Label: "repo " + filepath.ToSlash(rel), InRepo: within(p)})
	}
	if ud := harness.UserConfigDir(); ud != "" {
		roots = append(roots, skills.Root{Dir: filepath.Join(ud, "skills"), Label: "user"})
	}
	for _, e := range extra {
		if strings.HasPrefix(e, "~/") && home != "" {
			e = filepath.Join(home, e[2:])
		}
		roots = append(roots, skills.Root{Dir: e, Label: "user path"})
	}
	if home != "" {
		roots = append(roots,
			skills.Root{Dir: filepath.Join(home, ".claude", "skills"), Label: "~/.claude/skills"},
			skills.Root{Dir: filepath.Join(home, ".agents", "skills"), Label: "~/.agents/skills"})
	}
	return roots
}

// repoRoot is the nearest ancestor of dir holding a .git entry, or dir.
func repoRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return dir
		}
	}
}

// DiscoverSkills finds the skills for a session started in dir. The warnings are
// skills found and skipped, as text for a front end to show.
func DiscoverSkills(dir string, extra []string) ([]Skill, []string) {
	found, warns := skills.Discover(SkillRoots(dir, extra))
	out := make([]string, len(warns))
	for i, w := range warns {
		out[i] = w.String()
	}
	return found, out
}

// FindSkill returns the skill called name.
func FindSkill(all []Skill, name string) (Skill, bool) { return skills.Find(all, name) }

// SkillSummary is a skill's description clipped for a listing.
func SkillSummary(s Skill) string { return skills.Short(s) }

// ValidSkillName reports whether s is a name a skill can have.
func ValidSkillName(s string) bool { return skills.ValidName(s) }

// SkillPrompt is the user turn that invokes s, with args as the task. The body
// is inlined because a skill outside the session root cannot be read by the
// model's own tools; what the model was given is therefore exactly what the
// journal's UserMessage holds.
func SkillPrompt(s Skill, args string) (string, error) {
	body, err := skills.Body(s)
	if err != nil {
		return "", fmt.Errorf("engine: reading skill %q: %w", s.Name, err)
	}
	args = strings.TrimSpace(args)
	task := "The user invoked this skill without further instructions; apply it to the work in progress, or ask what they want it applied to."
	if args != "" {
		task = "Task: " + args
	}
	return fmt.Sprintf("Follow the skill %q for this task.\n\n<skill name=%q>\n%s\n</skill>\n\n%s", s.Name, s.Name, body, task), nil
}

// loadSkillCatalogue journals and feeds the catalogue of readable skills into a
// new session, once, after the instructions. It reuses the
// ProjectInstructionsLoaded event with scope "skills", so resume and fork replay
// it from the journal like the instructions files and there is no new event
// type. Nothing is recorded when there is nothing to list.
func (e *Engine) loadSkillCatalogue(ctx context.Context, dir string, all []Skill) error {
	cat := skills.Catalogue(all, dir)
	if cat == "" {
		return nil
	}
	if _, err := e.append(ctx, 0, journal.ProjectInstructionsLoaded{
		Content: journal.InlineText(cat),
		Scope:   journal.InstructionsScopeSkills,
	}); err != nil {
		return fmt.Errorf("engine: recording the skills catalogue: %w", err)
	}
	e.asm.AppendUser(cat)
	return nil
}
