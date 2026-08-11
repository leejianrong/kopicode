package corpus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Traits a task may declare. They exist so the composition constraints on the
// corpus — set by the card behind docs/SLICE-1.md build plan step 15 — are
// checked by the loader rather than remembered by whoever adds the next task.
const (
	// TraitRequiresRead marks a task that cannot be completed by writing a
	// file from scratch: the starting tree holds behaviour the model has no
	// way to guess and the oracle covers, so it must read before it edits.
	// These are the tasks that exercise anchored edit_file (ADR-0006).
	TraitRequiresRead = "requires_read"

	// TraitMultiFile marks a task whose fix spans more than one file.
	TraitMultiFile = "multi_file"
)

// Composition floors for the corpus as a whole.
const (
	minRequiresRead = 2
	minMultiFile    = 1
)

// maxTurnsCap is the turn budget ceiling. ADR-0005 §6 defines a corpus task as
// one that completes within 20 turns; a task that needs more is measuring
// something else.
const maxTurnsCap = 20

// maxTimeoutSeconds bounds an oracle. The corpus runs once per arm and an A/B
// series is many arms, so a slow suite multiplies.
const maxTimeoutSeconds = 300

// knownLanguages are the languages a task's starting tree may be written in.
// The list is short on purpose: every additional language is a toolchain that
// can be absent or a different version on the machine running the benchmark,
// and that variance lands directly in the measured delta.
var knownLanguages = map[string]bool{
	"go":     true,
	"python": true,
}

// knownRequirements are the executables an oracle may depend on. Go is here
// because the harness itself is Go, so it cannot be missing where kopibench
// runs; python3 because it ships with every CI image and developer machine
// this project targets and the tasks use only the standard library.
//
// Nothing that reaches the network belongs in this list, and neither does
// anything a task would have to install.
var knownRequirements = map[string]bool{
	"go":      true,
	"python3": true,
}

// knownTraits are the traits a task may declare.
var knownTraits = map[string]bool{
	TraitRequiresRead: true,
	TraitMultiFile:    true,
}

// shellMetacharacters are the characters that would only matter if the oracle
// were handed to a shell. Rejecting them keeps the "argv, never a shell
// string" rule from being quietly broken by a task that looks like it works
// because the runner happened to use a shell.
const shellMetacharacters = "|&;<>()$`\\\"'\n\t*?[]{}"

// validateTask checks one task manifest. dirName is the directory the manifest
// was read from, which the id must match.
func validateTask(t Task, dirName string) error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if t.SchemaVersion != SchemaVersion {
		add("schema_version is %d, want %d", t.SchemaVersion, SchemaVersion)
	}
	if t.ID == "" {
		add("id is empty")
	} else if t.ID != dirName {
		add("id %q does not match its directory %q", t.ID, dirName)
	}
	if !validID(dirName) {
		add("directory name %q is not lowercase letters, digits and dashes", dirName)
	}
	if strings.TrimSpace(t.Title) == "" {
		add("title is empty")
	}
	if strings.TrimSpace(t.Statement) == "" {
		add("statement is empty: the statement is what the engine is given, so a task without one measures nothing")
	}
	if strings.TrimSpace(t.Notes) == "" {
		add("notes are empty: a task has to say what it depends on and why it discriminates")
	}
	if !knownLanguages[t.Language] {
		add("language %q is not one of %s", t.Language, sortedKeys(knownLanguages))
	}
	if t.MaxTurns < 1 || t.MaxTurns > maxTurnsCap {
		add("max_turns is %d, want 1..%d (ADR-0005 §6)", t.MaxTurns, maxTurnsCap)
	}

	problems = append(problems, validateRequires(t)...)
	problems = append(problems, validateOracle(t)...)
	problems = append(problems, validateTraits(t)...)
	problems = append(problems, validateRepoDir(t)...)

	if len(problems) > 0 {
		return fmt.Errorf("corpus: task %s: %s", dirName, strings.Join(problems, "; "))
	}
	return nil
}

func validateRequires(t Task) []string {
	var problems []string
	if len(t.Requires) == 0 {
		return []string{"requires is empty: state what the oracle needs on PATH"}
	}
	for _, r := range t.Requires {
		if !knownRequirements[r] {
			problems = append(problems, fmt.Sprintf(
				"requires %q, which is not one of %s: an oracle that depends on anything "+
					"else is not deterministic enough for this corpus",
				r, sortedKeys(knownRequirements)))
		}
	}
	return problems
}

func validateOracle(t Task) []string {
	var problems []string
	o := t.Oracle

	if len(o.Argv) == 0 {
		problems = append(problems, "oracle.argv is empty")
	} else {
		if !contains(t.Requires, o.Argv[0]) {
			problems = append(problems, fmt.Sprintf(
				"oracle runs %q but requires does not list it", o.Argv[0]))
		}
		for _, arg := range o.Argv {
			if arg == "" {
				problems = append(problems, "oracle.argv holds an empty argument")
				continue
			}
			if strings.ContainsAny(arg, shellMetacharacters) {
				problems = append(problems, fmt.Sprintf(
					"oracle argument %q holds a shell metacharacter: the oracle is an argv, "+
						"never a shell string", arg))
			}
		}
	}

	if o.TimeoutSeconds < 1 || o.TimeoutSeconds > maxTimeoutSeconds {
		problems = append(problems, fmt.Sprintf(
			"oracle.timeout_seconds is %d, want 1..%d", o.TimeoutSeconds, maxTimeoutSeconds))
	}

	for k, v := range o.Env {
		if k == "" {
			problems = append(problems, "oracle.env has an empty variable name")
		}
		if strings.ContainsAny(k, "= ") {
			problems = append(problems, fmt.Sprintf("oracle.env name %q holds '=' or a space", k))
		}
		if strings.Contains(v, "\x00") {
			problems = append(problems, fmt.Sprintf("oracle.env value for %q holds a NUL", k))
		}
	}

	return problems
}

func validateTraits(t Task) []string {
	var problems []string
	seen := map[string]bool{}
	for _, tr := range t.Traits {
		if !knownTraits[tr] {
			problems = append(problems, fmt.Sprintf(
				"trait %q is not one of %s", tr, sortedKeys(knownTraits)))
		}
		if seen[tr] {
			problems = append(problems, fmt.Sprintf("trait %q is declared twice", tr))
		}
		seen[tr] = true
	}
	return problems
}

// validateRepoDir checks the starting tree exists and holds something. An
// empty repo/ is the corpus equivalent of a test that asserts nothing.
func validateRepoDir(t Task) []string {
	entries, err := os.ReadDir(t.RepoDir())
	if err != nil {
		return []string{fmt.Sprintf("reading %s/: %v", RepoDirName, err)}
	}
	if len(entries) == 0 {
		return []string{fmt.Sprintf("%s/ is empty: there is no starting tree to work on", RepoDirName)}
	}
	return nil
}

// validateCorpus checks the properties that are about the corpus as a whole
// rather than any one task.
func validateCorpus(c *Corpus) error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if c.SchemaVersion != SchemaVersion {
		add("schema_version is %d, want %d", c.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(c.Version) == "" {
		add("corpus_version is empty: a result has to be able to say which corpus produced it (ADR-0005)")
	}
	if !strings.HasPrefix(c.Digest, digestPrefix) {
		add("digest %q does not start with %q", c.Digest, digestPrefix)
	}
	if strings.TrimSpace(c.Description) == "" {
		add("description is empty")
	}

	if len(c.Tasks) < MinTasks {
		add("%d tasks, want at least %d: a run over fewer tasks than the corpus is "+
			"supposed to hold is a walk that went wrong, not a smaller experiment",
			len(c.Tasks), MinTasks)
	}

	reads, multi := 0, 0
	for _, t := range c.Tasks {
		if t.HasTrait(TraitRequiresRead) {
			reads++
		}
		if t.HasTrait(TraitMultiFile) {
			multi++
		}
	}
	if reads < minRequiresRead {
		add("%d tasks marked %s, want at least %d: without them nothing in the corpus "+
			"forces a read before an edit, and anchored edit_file goes unexercised",
			reads, TraitRequiresRead, minRequiresRead)
	}
	if multi < minMultiFile {
		add("%d tasks marked %s, want at least %d", multi, TraitMultiFile, minMultiFile)
	}

	if len(problems) > 0 {
		return errors.New("corpus: " + strings.Join(problems, "; "))
	}
	return nil
}

// validID reports whether name is lowercase letters, digits and dashes. Task
// ids end up in report tables, file paths and result records on three
// operating systems, so they stay boring.
func validID(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasSuffix(name, "-")
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// SolutionDir returns the reference-solution directory for a task, given the
// corpus root. Solutions deliberately live outside the corpus tree: the agent
// is pointed at a task's repo/ directory, and the fix must not be sitting
// where a run that stays inside its own root could read it.
//
// They are an overlay, not a patch — each file replaces or adds the file at
// the same path under repo/ — so applying one needs no patch tool and behaves
// the same on every platform.
func SolutionDir(corpusRoot, taskID string) string {
	return filepath.Join(filepath.Dir(corpusRoot), "_solutions", taskID)
}
