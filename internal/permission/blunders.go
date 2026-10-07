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
		return s.pipInstall(name, s.viaVenv(cmd), args)
	case isPython(name):
		for i, a := range args {
			if a.dynamic || !isMFlag(a.text) || i+1 >= len(args) {
				continue
			}
			if m := args[i+1]; !m.dynamic && m.text == "pip" {
				return s.pipInstall(name, s.viaVenv(cmd), args[i+2:])
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

// globalInstall refuses a global package install by npm, pnpm, yarn or bun. The
// flag may come before or after the subcommand (`npm -g install x`), and only
// a subcommand that installs counts, so `npm test -- -g pattern` is not one.
func globalInstall(name string, args []shWord) string {
	installing := map[string]bool{
		"install": true, "i": true, "add": true, "update": true, "upgrade": true, "up": true,
		"uninstall": true, "remove": true, "rm": true, "link": true, "ln": true, "global": true,
	}
	global, sub := false, ""
	skipNext := false
	for _, a := range args {
		if a.dynamic {
			continue
		}
		t := a.text
		if skipNext {
			skipNext = false
			if t == "global" {
				global = true
			}
			continue
		}
		switch {
		case t == "--":
			// What follows belongs to the script being run, not to the package manager.
			return decideGlobal(name, global, sub, installing)
		case t == "--location":
			skipNext = true
		case t == "--global" || strings.HasPrefix(t, "--global=") || t == "--location=global":
			global = true
		case strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--"):
			if strings.ContainsRune(t, 'g') {
				global = true
			}
		case !strings.HasPrefix(t, "-") && sub == "":
			sub = t
		}
	}
	return decideGlobal(name, global, sub, installing)
}

func decideGlobal(name string, global bool, sub string, installing map[string]bool) string {
	if (global && installing[sub]) || (name == "yarn" && sub == "global") {
		return globalMsg(name)
	}
	return ""
}

func globalMsg(name string) string {
	return name + " installs globally, outside the project; add the package to the project (`" + name + " install <pkg>` without -g, or a devDependency) and run it with npx or a package script"
}

// gitBlunder refuses the git commands that discard uncommitted work, and
// --no-verify on commit and push. Long options are matched as git matches them,
// by any prefix of three characters or more.
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
	// Words that are another option's value, not an option or a path.
	var words []shWord
	for i := 0; i < len(rest); i++ {
		t := rest[i].text
		if !rest[i].dynamic && sub == "commit" && (t == "-m" || t == "-F" || t == "-C" || t == "-c" || t == "--message" || t == "--file") {
			i++
			continue
		}
		words = append(words, rest[i])
	}
	has := func(match func(string) bool) bool {
		for _, a := range words {
			if !a.dynamic && match(a.text) {
				return true
			}
		}
		return false
	}
	long := func(full string) bool { return has(func(t string) bool { return isLongOpt(t, full) }) }
	anyDynamic := func() bool {
		for _, a := range words {
			if a.dynamic {
				return true
			}
		}
		return false
	}
	shortCluster := func(set string, must rune) func(string) bool {
		return func(t string) bool {
			if len(t) < 2 || t[0] != '-' || t[1] == '-' || !strings.ContainsRune(t, must) {
				return false
			}
			for _, r := range t[1:] {
				if !strings.ContainsRune(set, r) {
					return false
				}
			}
			return true
		}
	}
	switch sub {
	case "reset":
		if long("--hard") || anyDynamic() {
			return "git reset --hard discards uncommitted work; use `git stash` to set it aside, or `git reset --soft` / `git restore --staged <file>` to keep it"
		}
	case "clean":
		if long("--force") || has(shortCluster("fdxXinqe", 'f')) || anyDynamic() {
			return "git clean -f deletes untracked files for good; run `git clean -n` to list them and remove the ones you mean with rm"
		}
	case "checkout", "restore", "switch":
		wholeTree := has(func(t string) bool { return t == "." || t == "./" || t == ":/" || t == "*" })
		forced := (sub == "checkout" && (has(shortCluster("fqb", 'f')) || long("--force"))) ||
			(sub == "switch" && (long("--discard-changes") || has(shortCluster("fqc", 'f')) || long("--force")))
		dashed := false
		for _, a := range words {
			if !a.dynamic && a.text == "--" {
				dashed = true
			} else if dashed && a.dynamic {
				wholeTree = true
			}
		}
		if wholeTree && sub != "switch" {
			return "git " + sub + " . throws away every uncommitted change in the tree; name the one file you mean, or `git stash` first"
		}
		if forced {
			return "git " + sub + " with --force/--discard-changes throws away uncommitted changes; commit or `git stash` them first"
		}
	case "commit":
		if long("--no-verify") || has(shortCluster("aivqsenp", 'n')) {
			return "git commit --no-verify skips the repository's hooks; fix what the hook reports and commit again"
		}
	case "push":
		if long("--no-verify") {
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
		if mode := operands[0].text; worldWritable(mode) {
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
// directory that is the session's own: a relative path (.venv/bin/pip) or an
// absolute one inside the root. /usr/bin/pip is the system's, and a computed
// path ($VIRTUAL_ENV/bin/pip) cannot be shown to be anything.
func (s *autoScan) viaVenv(w shWord) bool {
	t := w.text
	if w.dynamic || !strings.Contains(t, "/") || path.Base(path.Dir(t)) != "bin" {
		return false
	}
	if strings.HasPrefix(t, "/") {
		return contains(s.p.root, path.Clean(t))
	}
	return !strings.HasPrefix(t, "..")
}

// isMFlag reports whether a python option word ends in -m, alone or in a
// cluster such as -Im, so `python -Im pip install` is seen.
func isMFlag(t string) bool {
	return len(t) >= 2 && t[0] == '-' && t[1] != '-' && strings.HasSuffix(t, "m")
}

// isLongOpt reports whether t is full or an abbreviation of the long option
// (git accepts any unambiguous prefix, so `--har` is `--hard`). Three
// characters is the shortest that is taken, so `--h` still refuses: the answer
// to ambiguity here is no.
func isLongOpt(t, full string) bool {
	return strings.HasPrefix(t, "--") && len(t) >= 3 && strings.HasPrefix(full, t)
}

// worldWritable reports whether a chmod mode grants write to everyone.
func worldWritable(mode string) bool {
	if mode == "" {
		return false
	}
	allDigits := true
	for _, r := range mode {
		if r < '0' || r > '7' {
			allDigits = false
		}
	}
	if allDigits {
		if len(mode) < 3 || len(mode) > 4 {
			return false
		}
		switch mode[len(mode)-1] {
		case '2', '3', '6', '7':
			return true
		}
		return false
	}
	for _, clause := range strings.Split(mode, ",") {
		who, perms, op := clause, "", byte(0)
		if i := strings.IndexAny(clause, "+-="); i >= 0 {
			who, op, perms = clause[:i], clause[i], clause[i+1:]
		}
		if op == '-' || !strings.Contains(perms, "w") && !strings.Contains(perms, "a") {
			continue
		}
		if who == "" || strings.ContainsAny(who, "oa") || who == "ugo" {
			if strings.Contains(perms, "w") {
				return true
			}
		}
	}
	return false
}
