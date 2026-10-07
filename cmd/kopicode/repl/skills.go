package repl

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/leejianrong/kopicode/internal/engine"
)

// SkillsFunc finds the session's skills now, with warnings for any it skipped.
// It is called afresh each time, so a skill written during the session is
// there on the next /skills or Tab without a restart.
type SkillsFunc func() ([]engine.Skill, []string)

// builtinCommands are the slash commands that are never skills. A skill with
// one of these names is still listed, marked, and reached through the model's
// own reading of it.
var builtinCommands = []string{"/context", "/exit", "/mode", "/quit", "/skills"}

// IsBuiltinCommand reports whether name (without the slash) is a built-in
// command, so a front end can leave a skill with that name out of what it tells
// the model: it can never be invoked.
func IsBuiltinCommand(name string) bool { return isBuiltin(name) }

func isBuiltin(name string) bool {
	for _, b := range builtinCommands {
		if b == "/"+name {
			return true
		}
	}
	return false
}

// showSkills prints /skills.
func (l *Loop) showSkills() {
	if l.skills == nil {
		l.Notice("/skills is not available in this session")
		return
	}
	all, warns := l.skills()
	if len(all) == 0 {
		l.Notice("no skills found. A skill is a directory with a SKILL.md, in .agents/skills or .kopicode/skills " +
			"in the repository or in " + filepath.Join(engine.UserConfigDir(), "skills"))
	}
	for _, s := range all {
		note := ""
		if isBuiltin(s.Name) {
			note = " (cannot be invoked: the name is a built-in command)"
		}
		l.out.line("/" + s.Name + note + "  " + engine.SkillSummary(s) + "  [" + s.Label + "]")
	}
	for _, w := range warns {
		l.Notice("skipped " + w)
	}
}

// expandSkill turns "/name args" into the prompt that invokes that skill. ok is
// false when the line is not a skill invocation, in which case it goes to the
// model as typed. An error is a skill that was found and could not be read.
func (l *Loop) expandSkill(line string) (prompt string, ok bool, err error) {
	if l.skills == nil || !strings.HasPrefix(line, "/") {
		return "", false, nil
	}
	name, args, _ := strings.Cut(strings.TrimSpace(line[1:]), " ")
	if isBuiltin(name) || !engine.ValidSkillName(name) {
		return "", false, nil
	}
	all, _ := l.skills()
	sk, found := engine.FindSkill(all, name)
	if !found {
		return "", false, nil
	}
	prompt, err = engine.SkillPrompt(sk, args)
	if err != nil {
		return "", false, err
	}
	l.Notice("skill: " + sk.Name + " [" + sk.Label + "]")
	return prompt, true, nil
}

// complete answers Tab for a slash command being typed: the built-ins and the
// skills, as whole lines. Past the command name it offers nothing.
func (l *Loop) complete(line string) []string {
	if !strings.HasPrefix(line, "/") || strings.ContainsAny(line, " \t") {
		return nil
	}
	names := append([]string(nil), builtinCommands...)
	if l.skills != nil {
		all, _ := l.skills()
		for _, s := range all {
			if !isBuiltin(s.Name) {
				names = append(names, "/"+s.Name)
			}
		}
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, strings.ToLower(line)) {
			out = append(out, n)
		}
	}
	// One match gets a trailing space, ready for its argument.
	if len(out) == 1 && out[0] != "/exit" && out[0] != "/quit" && out[0] != "/context" && out[0] != "/skills" {
		out[0] += " "
	}
	return out
}
