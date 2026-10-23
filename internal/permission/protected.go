package permission

import (
	"errors"
	"fmt"
	"strings"
)

// This file is ADR-0037: a caller names paths a session may not change, and the
// gate refuses a file write under them in every consent mode. The shell is
// covered only where the tokenizer that already reads a line for ADR-0017 can
// see the write target; it is a guard against the model's own convenient edit,
// not a sandbox.

// ErrProtectedPath is joined onto the refusal of a write under a path the
// session was told not to change. Like [ErrReadOnly] it is a denial the gate
// makes itself, never a question put to a [Policy]: a policy that could answer
// "allow" would make the declaration a request.
var ErrProtectedPath = errors.New("path is protected in this session")

// protectedAdvice is the second half of every refusal under this file, and it is
// the reason for the file. The model that meets the refusal has a choice: report
// that the task cannot be done as asked, or find another way to make its check
// pass. s5 showed it picks the second (edit the test, or hide the contradiction
// in the code), so the refusal says which one is wanted.
const protectedAdvice = "it is protected in this session because the task said not to change it; " +
	"if the task cannot be done without changing it, stop and say so in your final message, and do not work around it"

// SetProtected declares the paths this gate refuses to write under, and returns
// them resolved: absolute, symlink-free, inside the root. It must be called
// before the gate is shared between goroutines, as [Gate.SetReadOnly] must.
//
// A path is relative to the root or absolute inside it. One outside the root is
// an error rather than a silent no-op: writes outside the root already need
// consent, so a protected path there would claim a guarantee nothing gives.
func (g *Gate) SetProtected(paths []string) ([]string, error) {
	abs, err := resolveProtected(g.root, g.resolver, paths)
	if err != nil {
		return nil, err
	}
	g.protected = abs
	return abs, nil
}

// resolveProtected resolves each declared path the way a tool resolves a write
// target, so `./tests/../tests/x.py`, an absolute path and a symlink into the
// tree all land on the same answer as the declaration.
func resolveProtected(root string, resolver Resolver, paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" || strings.ContainsRune(p, 0) {
			return nil, fmt.Errorf("permission: a protected path is empty or malformed: %q", p)
		}
		abs, err := resolver.Resolve(p)
		if err != nil {
			return nil, fmt.Errorf("permission: resolving protected path %q: %w", p, err)
		}
		if !contains(root, abs) {
			return nil, fmt.Errorf("permission: protected path %q resolves to %s, outside the root %s; "+
				"only a path inside the root can be protected", p, abs, root)
		}
		if abs == root {
			return nil, fmt.Errorf("permission: protected path %q is the root itself; protecting everything is read-only, "+
				"which is a different declaration (ADR-0019)", p)
		}
		out = append(out, abs)
	}
	return out, nil
}

// protectedBy reports the protected path that p lies under (or is), by whole
// components, so "tests" does not protect "tests_old".
func protectedBy(protected []string, p string) (string, bool) {
	for _, pr := range protected {
		if contains(pr, p) {
			return pr, true
		}
	}
	return "", false
}

// ProtectShell hands a policy the resolved protected paths so its shell scan can
// refuse a line that writes under one. A policy that does not scan shell lines
// (the asking policy: a person sees every line) has nothing to give them to.
func ProtectShell(p Policy, protected []string) {
	switch v := p.(type) {
	case *AutoPolicy:
		v.protected = protected
	case *AllowlistPolicy:
		v.scan.protected = protected
	case *Switch:
		ProtectShell(v.ask, protected)
		ProtectShell(v.auto, protected)
	}
}

// protectedWriters are the commands whose file arguments the scan reads as write
// targets. It is a short list on purpose: each entry is something whose target
// the tokenizer can name without running anything, and the rest (an interpreter
// given a script, a build step, `git checkout -- <path>`) are not seen.
var protectedWriters = map[string]bool{
	"sed": true, "tee": true, "mv": true, "cp": true, "rm": true, "truncate": true, "touch": true,
}

// protectedWrites refuses a command that writes under a protected path. It
// returns "" when the command is not one of the writers, writes nowhere
// protected, or the session protects nothing.
//
// A target computed at run time, or one that cannot be resolved against the
// directories the line may be running in, is refused: it could be the protected
// path, and a guard that waves through what it cannot read is open exactly when
// it matters (ADR-0017's rule, applied here).
func (s *autoScan) protectedWrites(name string, args []shWord) string {
	if len(s.p.protected) == 0 || !protectedWriters[name] {
		return ""
	}
	var targets []shWord
	var cpDir *shWord // cp's -t DIRECTORY, which is then the only thing it writes into
	endOfFlags := false
	inPlace := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		t := a.text
		switch {
		case endOfFlags || a.dynamic || !strings.HasPrefix(t, "-") || t == "-":
			targets = append(targets, a)
		case t == "--":
			endOfFlags = true
		case name == "sed" && (strings.HasPrefix(t, "--in-place") ||
			(!strings.HasPrefix(t, "--") && strings.ContainsRune(t, 'i'))):
			inPlace = true
		case (name == "cp" || name == "mv") && (t == "-t" || t == "--target-directory") && i+1 < len(args):
			dir := args[i+1]
			cpDir = &dir
			i++
		case (name == "cp" || name == "mv") && strings.HasPrefix(t, "--target-directory="):
			cpDir = &shWord{text: strings.TrimPrefix(t, "--target-directory=")}
		}
	}
	if name == "sed" && !inPlace {
		return "" // without -i, sed writes to stdout
	}
	switch {
	case name == "cp" && cpDir != nil:
		targets = []shWord{*cpDir} // every other argument is read, not written
	case name == "cp" && len(targets) > 1:
		targets = targets[len(targets)-1:] // cp reads every argument but the last
	case name == "mv" && cpDir != nil:
		targets = append(targets, *cpDir) // mv also removes its sources, so they stay targets
	}
	for _, tg := range targets {
		if tg.dynamic || strings.HasPrefix(tg.text, "~") {
			return fmt.Sprintf("%s has a target computed at run time (%s), which cannot be checked against this session's protected paths", name, tg.text)
		}
		for _, cwd := range s.cwds {
			abs, ok := s.abs(cwd, tg.text)
			if !ok {
				return fmt.Sprintf("%s target %s cannot be resolved against the working directory, so it cannot be checked against this session's protected paths", name, tg.text)
			}
			if pr, hit := protectedBy(s.p.protected, abs); hit {
				return fmt.Sprintf("%s writes %s, under %s: %s", name, tg.text, pr, protectedAdvice)
			}
			// Removing or moving a directory that holds a protected path changes it too.
			if name == "rm" || name == "mv" {
				for _, pr := range s.p.protected {
					if contains(abs, pr) {
						return fmt.Sprintf("%s %s would remove or move %s: %s", name, tg.text, pr, protectedAdvice)
					}
				}
			}
		}
	}
	return ""
}

// protectedRedirect refuses a redirection whose resolved target is protected.
func (s *autoScan) protectedRedirect(target, abs string) string {
	if pr, hit := protectedBy(s.p.protected, abs); hit {
		return fmt.Sprintf("a redirection writes to %s, under %s: %s", target, pr, protectedAdvice)
	}
	return ""
}
