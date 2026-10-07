// Package skills finds reusable task instructions on disk (ADR-0025).
//
// A skill is a directory holding a SKILL.md with a `name` and a `description`
// in YAML frontmatter, the convention Claude Code, Codex, Cursor and Gemini CLI
// share (ADR-0014). This package only discovers and reads them. It never
// executes anything in a skill and never writes.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// FileName is the file a skill directory must hold.
const FileName = "SKILL.md"

// maxFileBytes bounds one SKILL.md. A skill is prose a model reads; one larger
// than this is a data file in the wrong place, and is refused with a warning
// rather than inlined into a prompt.
const maxFileBytes = 256 << 10

// maxDescription bounds the description shown in a listing or catalogue. The
// full text stays in the file.
const maxDescription = 300

// Root is one directory skills are looked for in.
type Root struct {
	// Dir is the directory holding one subdirectory per skill.
	Dir string
	// Label says where it came from, for /skills ("repo", "user", ...).
	Label string
	// InRepo is true when the model can read files here with its own tools,
	// because the directory is inside the session's root.
	InRepo bool
}

// Skill is one discovered skill.
type Skill struct {
	Name        string
	Description string
	// Dir is the skill's directory; Path is its SKILL.md.
	Dir  string
	Path string
	// Label and InRepo come from the [Root] that supplied it.
	Label  string
	InRepo bool
}

// Warning is something found and skipped. A skill that fails to parse is
// reported, never silently dropped, because "my skill isn't showing up" is
// otherwise undiagnosable.
type Warning struct {
	Path    string
	Message string
}

func (w Warning) String() string { return w.Path + ": " + w.Message }

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidName reports whether s can be a skill name, and so a slash command.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Discover reads every root in order. When two roots hold a skill of the same
// name the earlier root wins, so a repository's skill shadows a personal one.
// The result is sorted by name. A missing root is not a warning: most are
// absent.
func Discover(roots []Root) ([]Skill, []Warning) {
	var (
		out   []Skill
		warns []Warning
		seen  = map[string]bool{}
	)
	for _, r := range roots {
		entries, err := os.ReadDir(r.Dir)
		if err != nil {
			if !os.IsNotExist(err) {
				warns = append(warns, Warning{r.Dir, err.Error()})
			}
			continue
		}
		for _, e := range entries {
			dir := filepath.Join(r.Dir, e.Name())
			// A symlinked skill directory is common (a shared checkout), so
			// follow it; Stat rather than the entry's own type.
			if info, err := os.Stat(dir); err != nil || !info.IsDir() {
				continue
			}
			path := filepath.Join(dir, FileName)
			sk, warn := read(path, e.Name())
			if warn != "" {
				if _, statErr := os.Stat(path); statErr == nil {
					warns = append(warns, Warning{path, warn})
				}
				continue
			}
			if seen[sk.Name] {
				continue
			}
			seen[sk.Name] = true
			sk.Dir = dir
			sk.Path = path
			sk.Label = r.Label
			sk.InRepo = r.InRepo
			out = append(out, sk)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, warns
}

func read(path, dirName string) (Skill, string) {
	data, err := readCapped(path)
	if err != nil {
		return Skill{}, err.Error()
	}
	front, _, err := split(string(data))
	if err != nil {
		return Skill{}, err.Error()
	}
	name := front["name"]
	if name == "" {
		name = dirName
	}
	if !ValidName(name) {
		return Skill{}, fmt.Sprintf("the name %q is not usable as a command; use lowercase letters, digits, - and _", name)
	}
	desc := strings.Join(strings.Fields(front["description"]), " ")
	if desc == "" {
		return Skill{}, "no description in the frontmatter; a skill is found by its description"
	}
	return Skill{Name: name, Description: desc}, ""
}

func readCapped(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxFileBytes {
		return nil, fmt.Errorf("%d bytes is larger than the %d a skill may be", info.Size(), maxFileBytes)
	}
	return os.ReadFile(path)
}

// Body reads a skill's instructions: the file after its frontmatter.
func Body(s Skill) (string, error) {
	data, err := readCapped(s.Path)
	if err != nil {
		return "", err
	}
	_, body, err := split(string(data))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(body), nil
}

// Find returns the skill called name.
func Find(all []Skill, name string) (Skill, bool) {
	for _, s := range all {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// Short is the description clipped for a listing.
func Short(s Skill) string {
	d := s.Description
	if r := []rune(d); len(r) > maxDescription {
		return string(r[:maxDescription]) + "…"
	}
	return d
}

// split separates YAML frontmatter from the body. It reads the subset skills
// use: `key: value` lines, quoted or bare, with a value that may continue on
// indented lines or use the folded (>) and literal (|) block forms. Anything
// more elaborate is ignored rather than guessed at. A file with no frontmatter
// has an empty map.
func split(content string) (map[string]string, string, error) {
	content = strings.TrimPrefix(content, "\xef\xbb\xbf")
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	front := map[string]string{}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return front, content, nil
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, "", fmt.Errorf("the frontmatter opens with --- and never closes")
	}
	var key string
	for _, l := range lines[1:end] {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			if key != "" {
				front[key] += " " + strings.TrimSpace(l)
			}
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			key = ""
			continue
		}
		key = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if v == ">" || v == "|" || v == ">-" || v == "|-" {
			v = ""
		}
		front[key] = unquote(v)
	}
	for k, v := range front {
		front[k] = strings.TrimSpace(v)
	}
	return front, strings.Join(lines[end+1:], "\n"), nil
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// Catalogue renders the skills a model can read for itself as one message. A
// skill outside the session's root is left out: the model's file tools cannot
// open it, so listing it would invite a failed read. Those are reached by the
// user with /<name>, which inlines the body.
//
// It is capped, and says when it is: more than maxCatalogue skills are listed by
// name only past the cap. Returns "" when there is nothing to say.
func Catalogue(all []Skill, root string) string {
	const maxCatalogue = 100
	var b strings.Builder
	n := 0
	for _, s := range all {
		if !s.InRepo {
			continue
		}
		if n == maxCatalogue {
			b.WriteString("- …and more; run list_dir on the skills directories to see them\n")
			break
		}
		rel, err := filepath.Rel(root, s.Path)
		if err != nil {
			rel = s.Path
		}
		fmt.Fprintf(&b, "- %s: %s (%s)\n", s.Name, Short(s), filepath.ToSlash(rel))
		n++
	}
	if n == 0 {
		return ""
	}
	return "Skills in this repository. Each is a packaged set of instructions; read the file with " +
		"read_file when its description fits your task, before improvising an approach:\n" + b.String()
}
