package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Root is a repository root, and the only way a path argument becomes a file
// this package will open.
//
// [Root.Resolve] is the shared entry point every tool goes through — read_file,
// list_dir and grep today; write_file (KAN-781), run_shell (KAN-782) and
// edit_file (KAN-784) as they land. Nothing in this package opens a path the
// caller supplied, and nothing re-implements the containment check locally.
//
// There are two guards, and they are not redundant.
//
//   - Resolve *classifies*. It resolves the path to a real path on disk and
//     compares that against the real root, so it can say "this escapes, here is
//     where it went" — which is what the engine's permission gate (KAN-791)
//     needs to render a decision, and what a purely lexical check cannot say at
//     all. "foo/../bar" is only lexically inside the root if "foo" is not a
//     symlink to somewhere else, so the string is cleaned *after* the links are
//     followed, never before.
//   - os.Root *enforces*. Every open goes through a handle on the root
//     directory, which refuses to traverse out of it at the syscall level. That
//     closes the window between Resolve checking a path and the tool opening
//     it, which matters because the thing driving these tools is model output.
//
// os.Root is marginally stricter than Resolve: it rejects a symlink whose
// target is written as an absolute path even when that target lands back inside
// the root. Resolve accepts that path, so the two can disagree, and a
// disagreement surfaces as [FaultInternal] rather than being smoothed over —
// see [Set.open]. An absolute symlink inside a repository is already broken the
// moment the repository is cloned anywhere else, so the case is rare and being
// loud about it is cheaper than a second containment implementation.
type Root struct {
	real string
	dir  *os.Root
}

// OpenRoot resolves dir to a real path and opens a handle on it.
func OpenRoot(dir string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("tools: resolving repository root %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("tools: resolving repository root %q: %w", dir, err)
	}
	handle, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("tools: opening repository root %q: %w", dir, err)
	}
	return &Root{real: resolved, dir: handle}, nil
}

// Path returns the root's real absolute path.
func (r *Root) Path() string { return r.real }

// Close releases the root handle.
func (r *Root) Close() error { return r.dir.Close() }

// Path is a path argument that has been proven to lie inside a [Root].
//
// It is returned only by [Root.Resolve], so a function taking one has a
// compile-time reminder that the check happened. Tools open by Rel through the
// root handle, never by Abs.
type Path struct {
	// Given is the argument as the caller wrote it, for messages.
	Given string
	// Rel is the path relative to the root in OS form. "." is the root.
	Rel string
	// Abs is the real absolute path, symlinks resolved. It is for messages and
	// for a permission prompt to name; it is not what gets opened.
	Abs string
}

// Slash returns Rel in slash form, which is both what the io/fs tree wants and
// what tool output prints. Output must not change shape with the host's
// separator: a fixture recorded on Linux has to match on Windows, and a
// backslash in a rendered path is the first place that breaks.
func (p Path) Slash() string { return filepath.ToSlash(p.Rel) }

// Resolve turns a caller-supplied path into one proven inside the root.
//
// tool names the caller so the refusal the model reads says which tool refused;
// it is one of this package's tool-name constants.
//
// A path that lands outside is refused with an error wrapping [ErrOutsideRoot],
// carrying both the argument and where it actually went. Resolve never prompts
// and never consults policy — the engine owns that decision (SLICE-1 §M1: reads
// never ask, writes outside the root always do).
//
// The path need not exist: the tail that does not exist yet is resolved
// lexically against a parent that does, so write_file can create a file and
// still be checked. That is why the check cannot be "stat it and see".
func (r *Root) Resolve(tool, given string) (Path, error) {
	arg := given
	if arg == "" {
		arg = "."
	}

	// Deliberately not filepath.Join: Join cleans, and cleaning "link/../x"
	// down to "x" before the link is followed is exactly the lexical mistake
	// this function exists to avoid.
	candidate := arg
	if !filepath.IsAbs(arg) {
		candidate = r.real + string(filepath.Separator) + arg
	}

	resolved, err := realPath(candidate)
	if err != nil {
		return Path{}, taskErr(tool, given, err, "the path could not be resolved")
	}

	rel, err := filepath.Rel(r.real, resolved)
	if err != nil || escapes(rel) {
		detail := "the path is outside the repository root"
		if lexical, lerr := filepath.Rel(r.real, filepath.Clean(candidate)); lerr == nil && !escapes(lexical) {
			// The string stayed inside and the file did not, which is only
			// possible through a link. Say so: a model that wrote a plausible
			// relative path cannot otherwise tell why it was refused.
			detail = "a symbolic link in the path leads outside the repository root"
		}
		return Path{}, &Error{
			Tool:     tool,
			Fault:    FaultTask,
			Path:     given,
			Resolved: resolved,
			Detail:   detail + " (" + r.real + ")",
			err:      ErrOutsideRoot,
		}
	}

	return Path{Given: given, Rel: rel, Abs: resolved}, nil
}

// escapes reports whether a root-relative path leaves the root. filepath.Rel
// returns a cleaned path, so a leading ".." element is the whole test, and
// comparing elements rather than a string prefix keeps "..foo" out of it.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// realPath resolves every symlink in p, tolerating a tail that does not exist.
//
// filepath.EvalSymlinks resolves elements left to right, so a ".." after a
// resolved link pops the link's *target* — which is what the kernel does and
// what a lexical clean gets wrong. It insists the whole path exist, though, and
// write_file's whole job is a path that does not yet; so a missing tail is
// peeled off and rejoined onto its resolved parent, where filepath.Join's clean
// is safe because everything to its left is already link-free.
func realPath(p string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	trimmed := p
	for len(trimmed) > 1 && os.IsPathSeparator(trimmed[len(trimmed)-1]) {
		trimmed = trimmed[:len(trimmed)-1]
	}
	dir, base := filepath.Split(trimmed)
	if dir == "" || base == "" {
		return "", fmt.Errorf("resolving %q: %w", p, fs.ErrNotExist)
	}
	realDir, err := realPath(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(realDir, base), nil
}

// stat inspects a path through the root handle, following symlinks. Same
// enforcement and same classification as [Set.open].
func (s *Set) stat(tool string, p Path) (fs.FileInfo, error) {
	info, err := s.Root.dir.Stat(p.Rel)
	if err != nil {
		return nil, s.handleErr(tool, p, err)
	}
	return info, nil
}

// open reads a file through the root handle.
//
// Callers have already proven containment with [Root.Resolve]; this is the
// syscall-level enforcement described on [Root], and its failure modes are
// classified accordingly.
func (s *Set) open(tool string, p Path) ([]byte, error) {
	b, err := s.Root.dir.ReadFile(p.Rel)
	if err != nil {
		return nil, s.handleErr(tool, p, err)
	}
	return b, nil
}

// handleErr classifies a failure from the root handle.
func (s *Set) handleErr(tool string, p Path, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return taskErr(tool, p.Given, err, "no such file or directory")
	case errors.Is(err, fs.ErrPermission):
		return taskErr(tool, p.Given, err, "the path is not readable")
	default:
		// Resolve already proved this path inside the root, so a refusal here
		// is the absolute-symlink divergence documented on Root, or the tree
		// moved under us. Neither is the model's doing, and calling it a task
		// failure would hide a harness limitation inside a model score.
		return internalErr(tool, p.Given, err,
			"the root handle refused a path that resolved inside the root")
	}
}
