package permission

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// AutoPolicy is ADR-0017's `auto` consent mode: it answers every request itself,
// allowing what lies inside the session's root and refusing a fixed set of
// actions outright.
//
// It is not a sandbox, and the distinction is the whole of its honesty. It
// judges the *request the model made*: the command line is tokenized (see
// shellscan.go) and each command checked against the never-allow rules. A
// command it allows can still do anything a process running as the user can —
// `python -c`, `make`, a script already in the tree. What it removes is the
// obvious catastrophes, and the ability to smuggle one in behind an innocuous
// first word. Real containment is the caller's job (ADR-0011 decision 4).
//
// Every answer is [SourceAuto]: not [SourceUser], because nobody was asked, and
// not [SourcePolicy], because no caller-declared rule matched — the audit trail
// has to tell this mode apart from both.
//
// It never returns [VerdictAllowSession]. Standing consent is for a human being
// asked repeatedly, and a session grant would hide later commands from the
// journal; answering each request individually keeps every shell command on
// the record with its own decision.
//
// The never-allow list is fixed in code. A caller may append entries
// ([NewAuto]'s extra argument) but nothing removes or narrows a built-in one.
type AutoPolicy struct {
	root     string
	resolver Resolver
	extra    []neverEntry
}

// neverEntry is a caller-supplied never-allow entry: a command name and zero or
// more tokens that must appear after it, in order.
type neverEntry struct {
	raw    string
	tokens []string
}

// Limits on caller-supplied entries. They exist so a malformed request fails at
// startup rather than becoming a policy nobody can read.
const (
	maxNeverEntries     = 64
	maxNeverEntryBytes  = 200
	maxNeverEntryTokens = 8

	// maxScanDepth bounds how deeply `sh -c "sh -c '…'"` and nested
	// substitutions are followed. A line nested deeper is refused, not guessed.
	maxScanDepth = 8

	// maxScanSteps bounds the total work for one request, so a pathological
	// line cannot make the gate itself the denial of service.
	maxScanSteps = 20000
)

// NewAuto builds the auto policy for a session rooted at root.
//
// root is resolved through resolver, the same one the gate uses, so containment
// is judged on comparable paths. extra are caller-supplied never-allow entries
// (see [AutoPolicy]); each is "command [token ...]", matching a command whose
// name is the first token and whose arguments contain the rest in order.
func NewAuto(root string, resolver Resolver, extra []string) (*AutoPolicy, error) {
	if root == "" {
		return nil, errors.New("permission: root is required")
	}
	if resolver == nil {
		return nil, errors.New("permission: resolver is required")
	}
	abs, err := resolver.Resolve(root)
	if err != nil {
		return nil, fmt.Errorf("permission: resolving root %q: %w", root, err)
	}
	entries, err := parseNeverEntries(extra)
	if err != nil {
		return nil, err
	}
	return &AutoPolicy{root: abs, resolver: resolver, extra: entries}, nil
}

// ValidateNeverAllow reports whether extra is an acceptable list of caller
// never-allow entries, without building a policy. A front end uses it to refuse
// a malformed session.start before anything is opened.
func ValidateNeverAllow(extra []string) error {
	_, err := parseNeverEntries(extra)
	return err
}

func parseNeverEntries(extra []string) ([]neverEntry, error) {
	if len(extra) > maxNeverEntries {
		return nil, fmt.Errorf("permission: %d never-allow entries; at most %d are accepted", len(extra), maxNeverEntries)
	}
	out := make([]neverEntry, 0, len(extra))
	for _, raw := range extra {
		if len(raw) > maxNeverEntryBytes {
			return nil, fmt.Errorf("permission: a never-allow entry is %d bytes; at most %d are accepted", len(raw), maxNeverEntryBytes)
		}
		toks := strings.Fields(raw)
		if len(toks) == 0 {
			return nil, errors.New("permission: a never-allow entry is empty")
		}
		if len(toks) > maxNeverEntryTokens {
			return nil, fmt.Errorf("permission: never-allow entry %q has %d tokens; at most %d are accepted", raw, len(toks), maxNeverEntryTokens)
		}
		for _, t := range toks {
			if strings.ContainsAny(t, "\x00\n\r;&|<>()$`\\\"'") {
				return nil, fmt.Errorf("permission: never-allow entry %q contains shell syntax; an entry is a command name followed by plain argument tokens", raw)
			}
		}
		out = append(out, neverEntry{raw: strings.Join(toks, " "), tokens: toks})
	}
	return out, nil
}

// Decide answers req. See the type's comment for what the answer means.
func (p *AutoPolicy) Decide(_ context.Context, req Request) (Decision, error) {
	switch req.Kind {
	case KindRunShell:
		if req.Action.Dir == "" {
			return p.deny("shell command has no working directory"), nil
		}
		dir, err := p.resolver.Resolve(req.Action.Dir)
		if err != nil {
			return Decision{}, fmt.Errorf("resolving working directory %q: %w", req.Action.Dir, err)
		}
		if !contains(p.root, dir) {
			return p.deny(fmt.Sprintf("working directory %s is outside the session root %s — this will not be approved on retry", dir, p.root)), nil
		}
		if len(req.Action.Command) == 0 {
			return p.deny("shell command is empty"), nil
		}
		sc := &autoScan{p: p, cwds: []string{dir}}
		if reason := sc.argv(req.Action.Command, 0); reason != "" {
			return p.deny(reason + " — this will not be approved on retry"), nil
		}
		return Decision{Verdict: VerdictAllow, Source: SourceAuto, Reason: "auto mode: inside the session root, no never-allow rule matched"}, nil

	case KindWriteOutsideRoot:
		return p.deny("writes outside the session root are never approved in auto mode — this will not be approved on retry; keep changes inside the working tree"), nil

	case KindUnspecified:
		return p.deny("request carries no kind"), nil

	default:
		// A kind added later and not considered here is refused, not waved
		// through on the assumption that it resembles one of the above.
		return p.deny(fmt.Sprintf("no auto-mode rule for %s", req.Kind)), nil
	}
}

func (p *AutoPolicy) deny(reason string) Decision {
	return Decision{Verdict: VerdictDeny, Source: SourceAuto, Reason: "auto mode never-allow: " + reason}
}

// --- the never-allow rules ---------------------------------------------------

var (
	// privilegeEscalators run another command with more authority than the
	// session was given.
	privilegeEscalators = map[string]bool{"sudo": true, "doas": true, "su": true, "pkexec": true, "runuser": true}

	// shellInterpreters are the commands that execute a string or stdin as shell.
	shellInterpreters = map[string]bool{
		"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
		"ash": true, "csh": true, "tcsh": true, "fish": true,
	}

	downloaders = map[string]bool{"curl": true, "wget": true, "fetch": true, "aria2c": true}

	// wrappers run the command that follows them. They are scanned at every
	// word offset rather than by guessing which word is the real command,
	// because their option syntax (`nice -n 10 sudo`, `timeout 5 sudo`) is
	// exactly where a guess goes wrong.
	wrappers = map[string]bool{
		"env": true, "command": true, "exec": true, "nohup": true, "nice": true,
		"ionice": true, "time": true, "timeout": true, "stdbuf": true, "xargs": true,
		"setsid": true, "watch": true, "flock": true, "busybox": true, "find": true,
		"parallel": true, "unbuffer": true, "chroot": true, "strace": true,
	}

	// shellKeywords may precede the real command in a list.
	shellKeywords = map[string]bool{
		"!": true, "{": true, "}": true, "if": true, "then": true, "else": true,
		"elif": true, "fi": true, "do": true, "done": true, "while": true,
		"until": true, "for": true, "in": true,
	}

	// alwaysAllowedRedirectTargets are the device files a command line writes to
	// constantly and harmlessly.
	alwaysAllowedRedirectTargets = map[string]bool{
		"/dev/null": true, "/dev/stdout": true, "/dev/stderr": true,
	}
)

// autoScan is the state of analysing one request. cwds is every directory the
// command line may be running in — the starting directory plus the target of
// every cd seen — so a relative path is judged against all of them. "?" means a
// directory the scan could not determine, against which no relative path is
// provably inside the root.
//
// With enforce set the scan additionally refuses any command that does not match
// an entry of allow (the tokenized allowlist, [AllowlistPolicy]): the never-allow
// rules still run first, so a declared entry can narrow what is permitted but
// never widen it past them.
type autoScan struct {
	p     *AutoPolicy
	cwds  []string
	steps int

	allow   []allowEntry
	enforce bool

	// venv is set once the line has sourced a virtualenv's activate script, so a
	// later bare pip is installing into it (ADR-0023). Each run_shell is a fresh
	// shell, so this cannot carry across requests.
	venv bool
}

func (s *autoScan) tick() string {
	s.steps++
	if s.steps > maxScanSteps {
		return "the command line is too complex to analyse"
	}
	return ""
}

// argv analyses a command given as an argument vector, as run_shell supplies it
// (/bin/sh -c <line>). It returns "" when nothing matched, or the reason.
func (s *autoScan) argv(argv []string, depth int) string {
	words := make([]shWord, len(argv))
	for i, a := range argv {
		words[i] = shWord{text: a}
	}
	return s.command(shCommand{words: words}, depth)
}

// line analyses a shell command line.
func (s *autoScan) line(line string, depth int) string {
	if depth > maxScanDepth {
		return "the command line nests shells or substitutions too deeply to analyse"
	}
	if r := s.tick(); r != "" {
		return r
	}
	cmds, err := parseShell(line)
	if err != nil {
		return "the command line could not be analysed (" + err.Error() + ")"
	}
	for _, c := range cmds {
		for _, w := range c.words {
			for _, sub := range w.subs {
				if r := s.line(sub, depth+1); r != "" {
					return r
				}
			}
		}
		for _, rd := range c.redirs {
			for _, sub := range rd.target.subs {
				if r := s.line(sub, depth+1); r != "" {
					return r
				}
			}
		}
		if r := s.command(c, depth); r != "" {
			return r
		}
	}
	return s.pipelines(cmds)
}

// command applies the per-command rules to c.
func (s *autoScan) command(c shCommand, depth int) string {
	if r := s.tick(); r != "" {
		return r
	}
	if r := s.redirects(c); r != "" {
		return r
	}
	words := c.words
	for len(words) > 0 {
		w := words[0]
		if !w.dynamic && (shellKeywords[w.text] || isAssignment(w.text)) {
			words = words[1:]
			continue
		}
		break
	}
	if len(words) == 0 {
		return ""
	}
	if words[0].dynamic {
		return "the command name is computed at run time (" + words[0].text + ") and cannot be checked"
	}

	name := path.Base(words[0].text)
	if s.enforce && (name == "cd" || name == "pushd" || name == "popd") {
		if r := s.cdInsideRoot(name, words[1:]); r != "" {
			return r
		}
	}
	if r := s.neverAllowed(words, name, depth); r != "" {
		return r
	}
	if s.enforce && name != "cd" && name != "pushd" && name != "popd" {
		return s.allowed(words)
	}
	return ""
}

// neverAllowed runs the built-in and caller never-allow rules over one command,
// looking through a wrapper at every later word.
func (s *autoScan) neverAllowed(words []shWord, name string, depth int) string {
	if wrappers[name] {
		if r := s.rules(words[:1], depth, true); r != "" {
			return r
		}
		// Every later word is a candidate command. The wrapper's own rules run
		// first, then the rest as commands in their own right.
		for k := 1; k < len(words); k++ {
			if words[k].dynamic {
				// Only a dynamic word that could itself be the command matters,
				// and it could be: refuse rather than assume it is an argument.
				if !isOptionLike(words[k].text) && k == firstNonOption(words, 1) {
					return "the command run by " + name + " is computed at run time and cannot be checked"
				}
				continue
			}
			if r := s.rules(words[k:], depth, true); r != "" {
				return r
			}
		}
		return ""
	}
	return s.rules(words, depth, false)
}

// firstNonOption returns the index of the first word at or after from that
// does not start with "-" or look like a bare number (a timeout or niceness).
func firstNonOption(words []shWord, from int) int {
	for i := from; i < len(words); i++ {
		if !isOptionLike(words[i].text) {
			return i
		}
	}
	return -1
}

func isOptionLike(t string) bool {
	return strings.HasPrefix(t, "-") || allDigits(t) || isAssignment(t)
}

func isAssignment(t string) bool {
	i := strings.IndexByte(t, '=')
	if i <= 0 {
		return false
	}
	for j, r := range t[:i] {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && j > 0:
		default:
			return false
		}
	}
	return true
}

// rules checks one command, words[0] being its name. viaWrapper is true when
// the command was found by scanning past a wrapper, in which case wrapper
// handling is skipped: the outer loop already visits every offset.
func (s *autoScan) rules(words []shWord, depth int, viaWrapper bool) string {
	if r := s.tick(); r != "" {
		return r
	}
	if words[0].dynamic {
		return ""
	}
	name := path.Base(words[0].text)
	args := words[1:]

	if privilegeEscalators[name] {
		return fmt.Sprintf("%s runs commands with elevated privileges", name)
	}
	for _, e := range s.p.extra {
		if matchesNever(e, name, args) {
			return fmt.Sprintf("%q is on this session's never-allow list", e.raw)
		}
	}

	if r := s.blunders(name, words[0], args); r != "" {
		return r
	}

	switch name {
	case "rm":
		return s.rm(args)
	case "git":
		return gitPush(args)
	case "cd", "pushd", "popd":
		s.chdir(name, args)
		return ""
	case "eval":
		for _, a := range args {
			if a.dynamic {
				return "eval executes a string computed at run time, which cannot be checked"
			}
		}
		parts := make([]string, len(args))
		for i, a := range args {
			parts[i] = a.text
		}
		return s.line(strings.Join(parts, " "), depth+1)
	case "source", ".":
		if len(args) > 0 && !args[0].dynamic && path.Base(args[0].text) == "activate" {
			s.venv = true
		}
		return downloadInSubstitution(args)
	}

	if shellInterpreters[name] {
		if r := downloadInSubstitution(args); r != "" {
			return r
		}
		if script, ok, dynamic := shellScript(args); ok {
			if dynamic {
				return fmt.Sprintf("%s -c runs a string computed at run time, which cannot be checked", name)
			}
			return s.line(script, depth+1)
		}
	}
	return ""
}

// shellScript extracts the string a `sh -c` invocation runs. ok is false when
// the shell is not given a -c script (it runs a file or stdin). dynamic is true
// when the script, or a flag that could be -c, is computed at run time.
func shellScript(args []shWord) (script string, ok, dynamic bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a.dynamic {
			return "", true, true
		}
		t := a.text
		switch {
		case t == "--" || (!strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "+")):
			return "", false, false // a script file, or the end of the options
		case t == "-o" || t == "+o" || t == "-O" || t == "+O":
			i++ // takes an option name
		case strings.HasPrefix(t, "--"):
		case strings.HasPrefix(t, "-") && strings.ContainsRune(t, 'c'):
			if i+1 >= len(args) {
				return "", true, true
			}
			return args[i+1].text, true, args[i+1].dynamic
		}
	}
	return "", false, false
}

// downloadInSubstitution refuses `sh <(curl …)`, `eval "$(curl …)"` and
// `source <(wget …)`: a download whose result is executed.
func downloadInSubstitution(args []shWord) string {
	for _, a := range args {
		for _, sub := range a.subs {
			if linesDownload(sub) {
				return "it executes the output of a download (curl/wget piped or substituted into a shell)"
			}
		}
	}
	return ""
}

// linesDownload reports whether line runs a downloader. A line that cannot be
// parsed counts as one: this is a refusal path, so the failure direction is yes.
func linesDownload(line string) bool {
	cmds, err := parseShell(line)
	if err != nil {
		return true
	}
	for _, c := range cmds {
		for _, n := range candidateNames(c) {
			if downloaders[n] {
				return true
			}
		}
	}
	return false
}

// candidateNames returns the names a command could be running: its own, and —
// when it is a wrapper — the base name of every later word.
func candidateNames(c shCommand) []string {
	words := c.words
	for len(words) > 0 && !words[0].dynamic && (shellKeywords[words[0].text] || isAssignment(words[0].text)) {
		words = words[1:]
	}
	if len(words) == 0 || words[0].dynamic {
		return nil
	}
	first := path.Base(words[0].text)
	names := []string{first}
	if wrappers[first] {
		for _, w := range words[1:] {
			if !w.dynamic {
				names = append(names, path.Base(w.text))
			}
		}
	}
	return names
}

// pipelines refuses a download piped into a shell: `curl … | sh`.
func (s *autoScan) pipelines(cmds []shCommand) string {
	for i := 0; i < len(cmds); {
		j := i
		for j < len(cmds) && cmds[j].pipeNext {
			j++
		}
		// cmds[i..j] is one pipeline.
		sawDownload := false
		for k := i; k <= j && k < len(cmds); k++ {
			for _, n := range candidateNames(cmds[k]) {
				if sawDownload && shellInterpreters[n] {
					return "it pipes a download into a shell"
				}
			}
			for _, n := range candidateNames(cmds[k]) {
				if downloaders[n] {
					sawDownload = true
				}
			}
		}
		i = j + 1
	}
	return ""
}

// rm refuses removing anything outside the root, removing the root itself
// recursively, and a recursive removal whose target is computed at run time.
func (s *autoScan) rm(args []shWord) string {
	recursive, endOfFlags := false, false
	var targets []shWord
	for _, a := range args {
		t := a.text
		switch {
		case endOfFlags || a.dynamic || !strings.HasPrefix(t, "-") || t == "-":
			targets = append(targets, a)
		case t == "--":
			endOfFlags = true
		case t == "--recursive":
			recursive = true
		case t == "--no-preserve-root":
			return "rm --no-preserve-root disables rm's protection of /"
		case strings.HasPrefix(t, "--"):
		default:
			if strings.ContainsAny(t, "rR") {
				recursive = true
			}
		}
	}
	for _, tg := range targets {
		if tg.dynamic || strings.HasPrefix(tg.text, "~") {
			if recursive {
				return "rm -r with a target computed at run time cannot be confined to the session root"
			}
			continue
		}
		for _, cwd := range s.cwds {
			abs, ok := s.abs(cwd, tg.text)
			if !ok {
				return "rm target " + tg.text + " cannot be resolved against the working directory"
			}
			if !contains(s.p.root, abs) {
				return fmt.Sprintf("rm target %s is outside the session root %s", abs, s.p.root)
			}
			if recursive && abs == s.p.root {
				return "rm -r would delete the session root itself"
			}
		}
	}
	return ""
}

// gitPush refuses a forced push. A word computed at run time after "push" could
// be --force, so it is refused too.
func gitPush(args []shWord) string {
	push := -1
	for i, a := range args {
		if !a.dynamic && a.text == "push" {
			push = i
			break
		}
	}
	if push < 0 {
		return ""
	}
	for _, a := range args[push+1:] {
		t := a.text
		switch {
		case a.dynamic && (strings.HasPrefix(t, "$") || strings.HasPrefix(t, "`")):
			return "git push with an argument computed at run time could be --force and cannot be checked"
		case t == "--force", strings.HasPrefix(t, "--force-"), t == "--mirror":
			return "git push " + t + " rewrites remote history"
		case strings.HasPrefix(t, "+") && len(t) > 1:
			return "git push with a +refspec is a forced push"
		case strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.ContainsRune(t, 'f'):
			return "git push -f rewrites remote history"
		}
	}
	return ""
}

// chdir records where a cd may leave the command line.
func (s *autoScan) chdir(name string, args []shWord) {
	target := ""
	unknown := name == "popd"
	for _, a := range args {
		if !a.dynamic && (a.text == "-L" || a.text == "-P" || a.text == "--") {
			continue
		}
		if a.dynamic || a.text == "-" || strings.HasPrefix(a.text, "~") {
			unknown = true
		}
		target = a.text
		break
	}
	if target == "" {
		unknown = true // bare `cd` goes home
	}
	if unknown {
		s.cwds = append(s.cwds, "?")
		return
	}
	var next []string
	for _, cwd := range s.cwds {
		if abs, ok := s.abs(cwd, target); ok {
			next = append(next, abs)
		} else {
			next = append(next, "?")
		}
	}
	s.cwds = append(s.cwds, next...)
}

// redirects refuses a redirection that writes outside the root.
func (s *autoScan) redirects(c shCommand) string {
	for _, rd := range c.redirs {
		if !rd.writes() {
			continue
		}
		t := rd.target
		if t.dynamic {
			return "a redirection writes to a path computed at run time (" + t.text + ")"
		}
		if alwaysAllowedRedirectTargets[t.text] {
			continue
		}
		for _, cwd := range s.cwds {
			abs, ok := s.abs(cwd, t.text)
			if !ok {
				return "a redirection to " + t.text + " cannot be resolved against the working directory"
			}
			if !contains(s.p.root, abs) {
				return fmt.Sprintf("a redirection writes to %s, outside the session root %s", abs, s.p.root)
			}
		}
	}
	return ""
}

// abs resolves target against cwd. ok is false when it cannot be: cwd is "?",
// or resolution failed.
func (s *autoScan) abs(cwd, target string) (string, bool) {
	p := target
	if !filepath.IsAbs(p) {
		if cwd == "?" {
			return "", false
		}
		p = filepath.Join(cwd, p)
	}
	abs, err := s.p.resolver.Resolve(p)
	if err != nil {
		return "", false
	}
	return abs, true
}

// matchesNever reports whether a command (name, args) matches a caller entry.
// Remaining entry tokens must appear among the arguments in order; a word
// computed at run time matches any token, since it could be that token.
func matchesNever(e neverEntry, name string, args []shWord) bool {
	if path.Base(e.tokens[0]) != name {
		return false
	}
	rest := e.tokens[1:]
	i := 0
	for _, a := range args {
		if i == len(rest) {
			break
		}
		if a.dynamic || a.text == rest[i] {
			i++
		}
	}
	return i == len(rest)
}

// --- the tokenized allowlist (ADR-0018) ---------------------------------------

// allowEntry is one declared command prefix: a command name and the leading
// arguments that must follow it.
type allowEntry []string

// allowed reports "" when words match some allow entry, or the reason they do
// not. Matching is on tokens, never characters: an entry matches a command whose
// first words equal it, so ["uv","run","pytest"] permits `uv run pytest -v` and
// not `uv run pytestx`. The entry's own words must be written statically in the
// command; a word computed at run time cannot stand in for one.
func (s *autoScan) allowed(words []shWord) string {
	for _, e := range s.allow {
		if e.matches(words) {
			return ""
		}
	}
	return fmt.Sprintf("%s is not on the declared allowlist", renderWords(words))
}

func (e allowEntry) matches(words []shWord) bool {
	if len(words) < len(e) {
		return false
	}
	for i, tok := range e {
		w := words[i]
		if w.dynamic {
			return false
		}
		if i == 0 && !strings.Contains(tok, "/") {
			if path.Base(w.text) != tok {
				return false
			}
			continue
		}
		if w.text != tok {
			return false
		}
	}
	return true
}

func renderWords(words []shWord) string {
	parts := make([]string, 0, len(words))
	for _, w := range words {
		parts = append(parts, w.text)
	}
	return strings.Join(parts, " ")
}

// cdInsideRoot refuses a cd the allowlist cannot show stays inside the declared
// root. cd is otherwise implicit — a line that changes directory and then runs an
// allowed command is the ordinary shape of a model's command — but a cd that
// leaves the root would make every relative path after it mean something else.
func (s *autoScan) cdInsideRoot(name string, args []shWord) string {
	if name == "popd" {
		return "popd returns to a directory the scan cannot determine"
	}
	target := ""
	for _, a := range args {
		if !a.dynamic && (a.text == "-L" || a.text == "-P" || a.text == "--") {
			continue
		}
		if a.dynamic || a.text == "-" || strings.HasPrefix(a.text, "~") {
			return "cd to a directory computed at run time cannot be confined to the declared root"
		}
		target = a.text
		break
	}
	if target == "" {
		return "a bare cd goes to the home directory, outside the declared root"
	}
	for _, cwd := range s.cwds {
		abs, ok := s.abs(cwd, target)
		if !ok {
			return "cd " + target + " cannot be resolved against the working directory"
		}
		if !contains(s.p.root, abs) {
			return fmt.Sprintf("cd %s leaves the declared root %s", abs, s.p.root)
		}
	}
	return ""
}

// parseAllowCommands validates declared allow_commands entries. An entry may not
// name a command that runs another command — a wrapper, a shell, eval, a
// privilege escalator — because matching stops at the entry's own words and the
// nested command would go unchecked; the commands themselves are what to list.
func parseAllowCommands(entries [][]string) ([]allowEntry, error) {
	out := make([]allowEntry, 0, len(entries))
	for i, e := range entries {
		if len(e) == 0 {
			return nil, fmt.Errorf("permission: allow_commands[%d] is empty; an entry needs at least a command name", i)
		}
		for _, tok := range e {
			if tok == "" || strings.ContainsAny(tok, "\x00\n\r") {
				return nil, fmt.Errorf("permission: allow_commands[%d] has an empty or malformed word", i)
			}
		}
		name := path.Base(e[0])
		switch {
		case wrappers[name], shellInterpreters[name], privilegeEscalators[name],
			name == "eval", name == "source", name == ".", name == "exec":
			return nil, fmt.Errorf("permission: allow_commands[%d] names %q, which runs other commands; "+
				"a command it starts would not be matched against the list, so list those commands instead", i, name)
		case name == "cd", name == "pushd", name == "popd":
			return nil, fmt.Errorf("permission: allow_commands[%d] names %q; cd inside the declared root is "+
				"always permitted and needs no entry", i, name)
		}
		out = append(out, append(allowEntry(nil), e...))
	}
	return out, nil
}
