package permission

import (
	"fmt"
	"path"
	"strings"
)

// blunders are the common, recoverable-only-by-luck mistakes a model makes in
// auto mode (ADR-0023): installing into the user's global environment, throwing
// away uncommitted work, loosening permissions across a tree, skipping a hooks
// check. Each reason says what to do instead, because a model that is only told
// no tends to find a neighbouring way to do the same thing.
//
// Like the rest of the never-allow list it is not a sandbox: it recognises the
// plain spellings and refuses a computed argument where that could be the
// dangerous one. It cannot see `python script.py` doing the same thing.
func (s *autoScan) blunders(name string, cmd shWord, args []shWord) string {
	switch {
	case name == "pip" || name == "pip3" || strings.HasPrefix(name, "pip3."):
		return s.pipInstall(name, viaVenv(cmd), args)
	case isPython(name):
		for i, a := range args {
			if !a.dynamic && a.text == "-m" && i+1 < len(args) && !args[i+1].dynamic && args[i+1].text == "pip" {
				return s.pipInstall(name, viaVenv(cmd), args[i+2:])
			}
		}
	case name == "npm" || name == "pnpm" || name == "yarn" || name == "bun":
		return globalInstall(name, args)
	case name == "git":
		return gitBlunder(args)
	case name == "chmod" || name == "chown" || name == "chgrp":
		return s.recursivePerms(name, args)
	}
	return ""
}

func isPython(name string) bool {
	return name == "python" || name == "python3" || strings.HasPrefix(name, "python3.")
}

// pipInstall refuses `pip install` outside a virtualenv and `--user` anywhere.
// A virtualenv is recognised by a path through a "bin" directory the command
// was invoked by (.venv/bin/pip, ./venv/bin/python -m pip), or an activate
// sourced earlier in the same line. A bare pip is the user's global one.
func (s *autoScan) pipInstall(name string, venvCmd bool, args []shWord) string {
	install, user := false, false
	for _, a := range args {
		if a.dynamic {
			continue
		}
		switch a.text {
		case "install":
			install = true
		case "--user":
			user = true
		}
	}
	if !install {
		return ""
	}
	if user {
		return "pip install --user writes to the user's global Python; create a project environment instead (`uv venv && uv add <pkg>`, or `python -m venv .venv` then `.venv/bin/pip install <pkg>`)"
	}
	if s.venv || venvCmd {
		return ""
	}
	return fmt.Sprintf("%s install outside a virtualenv installs into the global Python; use `uv add <pkg>` (with a pyproject.toml), or a project venv: `python -m venv .venv` then `.venv/bin/pip install <pkg>`", name)
}

// globalInstall refuses a global package install by npm, pnpm, yarn or bun.
func globalInstall(name string, args []shWord) string {
	sub := ""
	for _, a := range args {
		if a.dynamic {
			continue
		}
		t := a.text
		switch {
		case t == "-g", t == "--global", t == "--location=global":
			if sub != "" {
				return globalMsg(name)
			}
		case !strings.HasPrefix(t, "-") && sub == "":
			sub = t
			if name == "yarn" && t == "global" {
				return globalMsg(name)
			}
		}
	}
	return ""
}

func globalMsg(name string) string {
	return name + " installs globally, outside the project; add the package to the project (`" + name + " install <pkg>` without -g, or a devDependency) and run it with npx or a package script"
}

// gitBlunder refuses the git commands that discard uncommitted work, and
// --no-verify on commit and push.
func gitBlunder(args []shWord) string {
	sub, rest := "", []shWord(nil)
	for i, a := range args {
		if a.dynamic {
			continue
		}
		switch a.text {
		case "reset", "clean", "checkout", "restore", "commit", "push", "switch":
			sub, rest = a.text, args[i+1:]
		}
		if sub != "" {
			break
		}
	}
	has := func(match func(string) bool) bool {
		for _, a := range rest {
			if !a.dynamic && match(a.text) {
				return true
			}
		}
		return false
	}
	anyDynamic := func() bool {
		for _, a := range rest {
			if a.dynamic {
				return true
			}
		}
		return false
	}
	switch sub {
	case "reset":
		if has(func(t string) bool { return t == "--hard" }) || anyDynamic() {
			return "git reset --hard discards uncommitted work; use `git stash` to set it aside, or `git reset --soft` / `git restore --staged <file>` to keep it"
		}
	case "clean":
		for _, a := range rest {
			t := a.text
			if a.dynamic || t == "--force" || (strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.ContainsRune(t, 'f')) {
				return "git clean -f deletes untracked files for good; run `git clean -n` to list them and remove the ones you mean with rm"
			}
		}
	case "checkout", "restore":
		for _, a := range rest {
			t := a.text
			if a.dynamic {
				continue
			}
			if t == "." || t == ":/" || t == "*" {
				return "git " + sub + " . throws away every uncommitted change in the tree; name the one file you mean, or `git stash` first"
			}
		}
	case "commit":
		if has(func(t string) bool {
			return t == "--no-verify" || (strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.ContainsRune(t, 'n'))
		}) {
			return "git commit --no-verify skips the repository's hooks; fix what the hook reports and commit again"
		}
	case "push":
		if has(func(t string) bool { return t == "--no-verify" }) {
			return "git push --no-verify skips the repository's hooks; fix what the hook reports and push again"
		}
	}
	return ""
}

// recursivePerms refuses chmod -R 777 anywhere, and a recursive chmod, chown or
// chgrp whose target is outside the root, is the root itself's parent, or is
// computed at run time.
func (s *autoScan) recursivePerms(name string, args []shWord) string {
	recursive := false
	var operands []shWord
	endOfFlags := false
	for _, a := range args {
		t := a.text
		switch {
		case endOfFlags || a.dynamic || !strings.HasPrefix(t, "-") || t == "-":
			operands = append(operands, a)
		case t == "--":
			endOfFlags = true
		case t == "--recursive":
			recursive = true
		case strings.HasPrefix(t, "--"):
		default:
			if strings.ContainsRune(t, 'R') {
				recursive = true
			}
		}
	}
	if !recursive {
		return ""
	}
	if name == "chmod" && len(operands) > 0 {
		mode := operands[0].text
		if mode == "777" || mode == "0777" || mode == "a+rwx" || mode == "ugo+rwx" {
			return "chmod -R " + mode + " makes a whole tree world-writable; grant the specific bit to the specific file (chmod +x script.sh)"
		}
	}
	if len(operands) < 2 {
		return ""
	}
	for _, tg := range operands[1:] {
		if tg.dynamic || strings.HasPrefix(tg.text, "~") {
			return name + " -R with a target computed at run time cannot be confined to the session root"
		}
		for _, cwd := range s.cwds {
			abs, ok := s.abs(cwd, tg.text)
			if !ok {
				return name + " -R target " + tg.text + " cannot be resolved against the working directory"
			}
			if !contains(s.p.root, abs) {
				return fmt.Sprintf("%s -R target %s is outside the session root %s", name, abs, s.p.root)
			}
		}
	}
	return ""
}

// viaVenv reports whether a command was invoked by a path through a bin
// directory, the shape of a virtualenv's own pip or python.
func viaVenv(w shWord) bool {
	t := w.text
	return !w.dynamic && strings.Contains(t, "/") && path.Base(path.Dir(t)) == "bin"
}
