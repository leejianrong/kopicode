package arch_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This guard exists because documentation did not stop the incident and would
// not stop it again. On 2026-08-11 a test helper ran a git subprocess that
// resolved to kopicode's own repository, left core.bare=true in the real
// .git/config so that every git command in the main checkout failed, and
// created a stash whose diff deleted every tracked file. Being "in a worktree"
// protected nothing: a worktree shares .git/config, the object store and the
// ref store with its parent.
//
// Two mechanisms produced that, not one, so there are two rules here.
//
//  1. No Dir. exec.Command("git", ...) with no Dir runs in the calling
//     process's working directory, which for a test binary is the package
//     directory, which is inside this repository.
//  2. An inherited environment, which is the one that actually bit. GIT_DIR
//     overrides the working directory entirely, so a command with a perfectly
//     correct Dir still operates on whatever GIT_DIR names.
//
// # Why this is no longer about git (KAN-837)
//
// The rule those two mechanisms taught is not git-specific. An inherited
// environment changes what *any* subprocess does, and the change is invisible
// in the command: GOFLAGS and GOOS change what `go build` produces, PYTHONPATH
// and VIRTUAL_ENV change what `python` imports, NODE_OPTIONS changes what
// `node` runs, and PATH decides which binary is executed at all. The syntax
// gate spawns all three. So the guard covers every exec.Command and
// exec.CommandContext in the tree, whatever it runs.
//
// The git half was **not** relaxed to make room for the general case, and the
// two differ in exactly one respect, which is deliberate and lives in
// [subprocessRule]: the *rule* is identical — assign Dir, assign Env — and only
// the *explanation* differs, because "your test just set core.bare on the real
// repository" and "your build picked up an ambient GOFLAGS" want different
// words. Sites where an inherited directory or environment is genuinely correct
// carry a waiver with a stated reason rather than getting a blanket exemption,
// because a reason is reviewable and a category is not.
//
// Coverage went up rather than down in one further way: the git rules only ever
// recognised a literal "git" program name, so a git command whose program was
// computed was invisible. Every call is in scope now, literal or not.
//
// # Assignment is not the same claim as a decision (KAN-845)
//
// The two rules above check that Dir and Env are *assigned*. That leaves open
// the exact thing they were written to close. `cmd.Env = os.Environ()` assigns
// Env and inherits the whole ambient environment; `cmd.Env = nil` is os/exec's
// documented spelling for "inherit the parent's environment" and reads as
// though a decision was made. internal/build/inject_test.go did the first at
// both of its sites, and it spawns `go build`, where an ambient GOFLAGS or GOOS
// changes the artefact produced while the command still reads correctly.
//
// So there is a third check, on the *value* assigned, with its own directive
// ([allowAmbientEnv]). It does not replace the assignment check: both run, and
// a subprocess that assigns nothing still fails twice.
//
// # What the value check can and cannot decide
//
// Stated plainly, because a static guard that overstates its reach is worse
// than the gap it replaced: it passes what it cannot see through while looking
// stronger. A go/ast analysis cannot follow an arbitrary expression to a value,
// so this one does not try. It decides exactly four syntactic forms —
// os.Environ(), an append over one, nil, and a local variable in the same
// function assigned one of those — and says nothing about any other shape. A
// helper that returns the ambient environment passes it.
//
// That is the funnel pattern, arrived at from the other side. The four forms
// above are the ones written *without* deciding anything; every other shape
// means somebody wrote a named function, and the name and its doc comment are
// the review surface. This repository has four such builders — repo.baseEnv,
// syntax.baseEnv, tools.childEnv and the oracleEnv/passThrough allowlist in
// internal/corpus — and each says what it strips or admits, and why.
//
// A single shared builder was considered and rejected. internal/build must
// import nothing from this module (TestBuildPackageIsALeaf in imports_test.go
// walks its test files too), so the one site that most needed fixing could not
// have used it; and the four differ on purpose — a denylist is right for a
// toolchain gate that must still find its toolchain, an allowlist is right for
// a benchmark oracle that must be reproducible. Collapsing them would need a
// passthrough option, and a passthrough option is this hole with a name.
//
// # A Cmd is not always a call (KAN-853)
//
// Everything above found subprocesses by looking for exec.Command and
// exec.CommandContext. os/exec documents a second way — "It is also possible to
// construct a Cmd directly" — and `&exec.Cmd{Path: gitPath, Args: …}` spawns
// exactly what the call spawns, inherits exactly what it inherits when Env is
// left alone, and was invisible to all three rules. Not an exotic shape: it is
// the one you reach for when you want to set SysProcAttr or Path yourself, which
// is what internal/procgroup and internal/tools are about.
//
// So a composite literal of exec.Cmd is a site like any other. A field written
// inside the literal satisfies its rule exactly as an assignment to the bound
// variable does — both are visible, and insisting on the assignment form would
// be a style rule wearing a safety rule's failure message — and the value check
// reads Env out of the literal the same way it reads it out of an assignment.
//
// It fails closed in the two places it cannot follow the literal, which is the
// same bias as everywhere else here: a positional literal, whose fields cannot
// be named, counts as setting none of them; and a literal not bound to a single
// variable — used inline, or handed straight to a helper — has no later
// assignment to look for, so only what is in the literal counts.
//
// One spelling is worth naming because it writes no exec.Cmd at all. Inside a
// container of Cmds the element type may be elided, so `[]*exec.Cmd{{Path: p}}`
// builds a Cmd out of a brace with nothing in front of it. Those inner literals
// are reachable only from the container, and they are collected from there.
//
// The analysis is flow-insensitive and deliberately crude: it asks whether the
// enclosing function assigns Dir and Env to the variable holding the Cmd at
// all, not whether it does so before every use. Ordering is visible in review;
// a missing assignment is not, and the false negative that costs a repository
// is the missing one. It also only recognises a Cmd bound to a local variable —
// a Cmd built by a helper, or passed to one, reads as unassigned and needs a
// waiver. That is the intended bias: a noisy guard beats a silent corruption,
// and a waiver with a reason on it is reviewable in a way that an invisible
// exception is not.
//
// See .claude/skills/agent-ground-rules/SKILL.md for both reproductions, and
// internal/repo for the worked example of the patterns enforced here.

// The waiver comments. Each takes a reason on the same line, and applies to a
// subprocess on that line or the line directly below it. They are deliberately
// not named for git: one vocabulary for one rule.
const (
	allowNoDir = "//kopicode:allow-nodir:"
	allowNoEnv = "//kopicode:allow-noenv:"
	// allowAmbientEnv waives the value check only. It is deliberately not
	// implied by allowNoEnv and does not imply it: "this command may inherit
	// the environment" and "this command assigns no environment at all" are
	// different claims, and KAN-837 proved the independence of the first two
	// the same way.
	allowAmbientEnv = "//kopicode:allow-ambientenv:"
)

// allDirectives is the whole vocabulary. collectWaivers reads it and
// otherDirectives reports from it, so a fourth directive cannot be added in one
// place and forgotten in the other.
var allDirectives = []string{allowNoDir, allowNoEnv, allowAmbientEnv}

// directiveStem is what every waiver in this vocabulary starts with, once the
// comment marker and any spaces are stripped. A comment that begins with it but
// is not one of the directives above is a misspelling, and a misspelled waiver
// waives nothing while looking exactly like one that does. That is how the
// rename in KAN-837 could have gone wrong quietly, so it is checked rather than
// reviewed: see TestNoUnrecognisedWaiverDirectives.
const directiveStem = "kopicode:allow-"

const (
	groundRules = ".claude/skills/agent-ground-rules/SKILL.md"
	brief       = "CLAUDE.md, \"Boundaries that must not be crossed\""
)

// guidance is what the failure message says: why the rule exists, and the
// assignment that satisfies it.
type guidance struct {
	// why explains what goes wrong without the assignment. A guard whose
	// message does not explain itself gets deleted by whoever hits it at 2am,
	// which is exactly what the message is for.
	why string
	// fix names the assignment that satisfies the rule. It takes the variable
	// holding the Cmd.
	fix string
}

// subprocessRule is one field every subprocess must assign.
//
// Both halves of the rule are the same for git and for everything else — this
// is the type that would have to grow a "git only" flag if the general case had
// been allowed to level the git case down, and it deliberately has not got one.
// What varies is the [guidance], because the consequence differs even where the
// requirement does not.
type subprocessRule struct {
	// field is the exec.Cmd field that must be assigned.
	field string
	// directive is the comment that waives this rule. Waiving one rule never
	// waives the other.
	directive string
	// general is what a reader of a non-git failure needs to know.
	general guidance
	// git is the same rule with the incident that produced it attached.
	git guidance
}

var subprocessRules = []subprocessRule{
	{
		field:     "Dir",
		directive: allowNoDir,
		general: guidance{
			why: "a subprocess with no Dir runs in the calling process's working directory. For a\n" +
				"library that is whatever the host program last chdir'd to, and for a test binary\n" +
				"it is the package directory, which is inside this repository. Which directory a\n" +
				"process starts in decides what it reads, what it writes, and — for tools that\n" +
				"search upwards, git and go among them — which project it thinks it is in.",
			fix: "assign %s.Dir the directory this command is meant to operate on",
		},
		git: guidance{
			why: "a git command with no Dir runs in the calling process's working directory. For a\n" +
				"test binary that is the package directory, which is inside this repository: on\n" +
				"2026-08-11 that left core.bare=true in the real .git/config and a stash whose diff\n" +
				"deleted every tracked file.",
			fix: "assign %s.Dir the repository this command is meant to operate on",
		},
	},
	{
		field:     "Env",
		directive: allowNoEnv,
		general: guidance{
			why: "Dir is necessary and not sufficient. A subprocess with no Env inherits this\n" +
				"process's environment, and an inherited variable changes what a program does\n" +
				"without changing anything visible in the command: GOFLAGS and GOOS change what\n" +
				"`go build` produces, PYTHONPATH and VIRTUAL_ENV change what `python` imports,\n" +
				"NODE_OPTIONS changes what `node` runs, and PATH decides which binary runs at all.",
			fix: "assign %s.Env an environment you built rather than inherited — see repo.baseEnv,\n" +
				"goEnv in internal/syntax and the fixtureEnv helper in internal/repo",
		},
		git: guidance{
			why: "Dir is necessary and not sufficient. GIT_DIR overrides the working directory\n" +
				"entirely, so an inherited environment redirects a command that looks perfectly\n" +
				"targeted. GIT_WORK_TREE, GIT_INDEX_FILE and GIT_COMMON_DIR do the same.",
			fix: "assign %s.Env an environment you built rather than inherited — see repo.baseEnv\n" +
				"and the fixtureEnv helper in internal/repo",
		},
	},
}

// forProgram picks the guidance that fits what is being run.
func (r subprocessRule) forProgram(isGit bool) guidance {
	if isGit {
		return r.git
	}
	return r.general
}

// ambientEnvRule is the value check: Env is assigned, and what it is assigned
// is visibly the environment this process was handed.
//
// It is a separate value from [subprocessRules] because it is a different kind
// of claim — those ask whether a field was assigned, this asks what it was
// assigned — and folding it in as a third entry there would have made
// "assigned" and "assigned something deliberate" indistinguishable in the one
// place the guard has to keep them apart.
var ambientEnvRule = struct {
	// field is the exec.Cmd field the failure names, so the message can say
	// cmd.Env like every other one.
	field string
	// label is what this violation is called when violations are compared.
	label     string
	directive string
	general   guidance
	git       guidance
}{
	field:     "Env",
	label:     "Env value",
	directive: allowAmbientEnv,
	general: guidance{
		why: "assigning Env is not the same as deciding what it holds. This assignment hands the\n" +
			"child the environment this process was handed, which is what the Env rule exists to\n" +
			"prevent: GOFLAGS and GOOS change what `go build` produces, PYTHONPATH and\n" +
			"VIRTUAL_ENV change what `python` imports, NODE_OPTIONS changes what `node` runs.\n" +
			"A nil Env is the same thing spelled differently — os/exec documents it as\n" +
			"inheriting the parent's environment.",
		fix: "build the environment %s runs in: start from a named builder that says what it\n" +
			"strips or admits, as repo.baseEnv, syntax.baseEnv, tools.childEnv and the\n" +
			"passThrough allowlist in internal/corpus each do",
	},
	git: guidance{
		why: "assigning Env is not the same as deciding what it holds. This assignment hands the\n" +
			"child the environment this process was handed, GIT_DIR included — and GIT_DIR\n" +
			"overrides the working directory entirely, so the command is redirected at another\n" +
			"repository however correct its Dir is. That is the mechanism that left\n" +
			"core.bare=true in the real .git/config on 2026-08-11. GIT_WORK_TREE,\n" +
			"GIT_INDEX_FILE and GIT_COMMON_DIR do the same.",
		fix: "build the environment %s runs in: repo.baseEnv strips exactly those variables, and\n" +
			"the fixtureEnv helper in internal/repo asserts they are gone before any command runs",
	},
}

// violation is one rule broken at one call site.
type violation struct {
	line int
	// field is the exec.Cmd field the failure is about, so a message can name
	// the assignment that fixes it.
	field string
	// label distinguishes the three checks. Two of them are about Dir and Env
	// being assigned at all; the third is about what Env was assigned, and a
	// test that compares violations has to be able to tell those apart.
	label string
	msg   string
}

// subprocessCall is one place an *exec.Cmd comes into existence: a call to
// exec.Command or exec.CommandContext whatever it runs, or — since KAN-853 — a
// composite literal of exec.Cmd.
//
// The two are one type because they are one rule. os/exec documents building a
// Cmd directly as an ordinary way to use the package, so `&exec.Cmd{Path: git}`
// spawns exactly what `exec.Command("git")` spawns and inherits exactly the same
// environment when Env is left alone. Before KAN-853 the guard only recognised
// the call, so the most direct way to build a Cmd was the one way past every
// check in this file.
type subprocessCall struct {
	// name is the variable the *exec.Cmd was bound to, empty when the call's
	// result or the literal is used directly or passed straight to another
	// function.
	name string
	// body is the enclosing function, nil for a package-level call.
	body *ast.BlockStmt
	line int
	// isGit is true for a literal git program name. It selects the wording of
	// the failure, never whether the rules apply.
	isGit bool
	// literal holds the keyed fields of the exec.Cmd composite literal this Cmd
	// was built from, and is nil for a Cmd built by exec.Command. A field set in
	// the literal satisfies its rule exactly as an assignment to it would: both
	// are visible, and demanding the assignment form would be a style rule
	// wearing a safety rule's failure message.
	literal map[string]ast.Expr
	// unkeyed marks a composite literal written positionally, whose fields this
	// analysis cannot name. It satisfies nothing and says so.
	unkeyed bool
	// viaLiteral distinguishes a literal from a call even when the literal set
	// no fields at all, which is the case that most needs the distinction.
	viaLiteral bool
}

// setsInLiteral reports whether the composite literal this Cmd came from wrote
// the field itself.
func (sc subprocessCall) setsInLiteral(field string) bool {
	_, ok := sc.literal[field]
	return ok
}

// subprocessCalls returns every exec.Command/exec.CommandContext call in file.
//
// file must have been parsed with parser.ParseComments; the waivers live in the
// comments. parser.ImportsOnly, which the import guard in this package uses, is
// not enough here — the call expressions and the assignments are the subject.
func subprocessCalls(fset *token.FileSet, file *ast.File) []subprocessCall {
	execPkg := resolveImport(file, "os/exec")
	if !execPkg.imported() {
		return nil
	}

	bodies := funcBodies(file)
	bound := boundCmds(execPkg, file)

	var calls []subprocessCall
	ast.Inspect(file, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		prog, isSpawn := spawnedProgram(execPkg, call)
		if !isSpawn {
			return true
		}
		calls = append(calls, subprocessCall{
			name:  bound[call],
			body:  enclosingBody(bodies, call),
			line:  fset.Position(call.Pos()).Line,
			isGit: isGitProgram(prog),
		})
		return true
	})
	return calls
}

// cmdLiterals returns every composite literal of exec.Cmd in file, whether it
// was written as a value or had its address taken.
//
// The literal's own fields are carried along rather than resolved here, because
// all three rules ask a different question of them: two ask whether the field is
// present at all, and the third asks what it holds.
func cmdLiterals(fset *token.FileSet, file *ast.File) []subprocessCall {
	execPkg := resolveImport(file, "os/exec")
	if !execPkg.imported() {
		return nil
	}

	bodies := funcBodies(file)
	bound := boundCmdLiterals(execPkg, file)

	var out []subprocessCall
	record := func(lit *ast.CompositeLit) {
		fields, keyed := literalFields(lit)
		out = append(out, subprocessCall{
			name:       bound[lit],
			body:       enclosingBody(bodies, lit),
			line:       fset.Position(lit.Pos()).Line,
			isGit:      literalRunsGit(fields),
			literal:    fields,
			unkeyed:    !keyed,
			viaLiteral: true,
		})
	}

	ast.Inspect(file, func(n ast.Node) bool {
		lit, isLit := n.(*ast.CompositeLit)
		if !isLit {
			return true
		}
		if execPkg.isCmdType(lit.Type) {
			record(lit)
			return true
		}
		// A container of Cmds lets its elements elide the type, so the literal
		// that builds the Cmd never names it. Those elements are reachable only
		// from here: their own Type is nil, and a nil type matches nothing.
		if execPkg.holdsCmdElements(lit.Type) {
			for _, elt := range lit.Elts {
				if kv, isKV := elt.(*ast.KeyValueExpr); isKV {
					elt = kv.Value
				}
				if inner, isInner := elt.(*ast.CompositeLit); isInner && inner.Type == nil {
					record(inner)
				}
			}
		}
		return true
	})
	return out
}

// literalFields returns the keyed fields of a composite literal, and whether it
// was keyed at all.
//
// A positional literal reports false with no fields. exec.Cmd has more than a
// dozen fields and go vet's composites check rejects an unkeyed literal of an
// imported type, so this is not a shape anybody writes — but "the analysis
// cannot name these fields" must resolve to "nothing is set", never to silence.
func literalFields(lit *ast.CompositeLit) (map[string]ast.Expr, bool) {
	fields := map[string]ast.Expr{}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return nil, false
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			return nil, false
		}
		fields[key.Name] = kv.Value
	}
	return fields, true
}

// literalRunsGit reports whether a Cmd literal visibly runs git, from Path or
// from the first element of Args. As with a computed program name in a call,
// this only picks the wording of the failure and never whether it fires.
func literalRunsGit(fields map[string]ast.Expr) bool {
	if path, ok := fields["Path"]; ok && isGitProgram(path) {
		return true
	}
	args, ok := fields["Args"]
	if !ok {
		return false
	}
	lit, ok := args.(*ast.CompositeLit)
	if !ok || len(lit.Elts) == 0 {
		return false
	}
	return isGitProgram(lit.Elts[0])
}

// subprocessViolations reports every subprocess in file that fails any of the
// three rules: Dir assigned, Env assigned, and the value assigned to Env not
// being one the analysis can see is inherited.
func subprocessViolations(fset *token.FileSet, file *ast.File) []violation {
	sites := append(subprocessCalls(fset, file), cmdLiterals(fset, file)...)
	if len(sites) == 0 {
		return nil
	}
	// Two traversals produced this, so the sites are in source order within each
	// and not across them. Sorting is what makes a file holding both kinds report
	// them the way a reader reads them.
	sort.SliceStable(sites, func(i, j int) bool { return sites[i].line < sites[j].line })

	waived := collectWaivers(fset, file)
	// How this file spells os.Environ. Zero when os is not imported at all, in
	// which case no Environ call can be reached and the value check simply has
	// nothing to say. It never becomes a reason to skip the assignment checks.
	osPkg := resolveImport(file, "os")

	var out []violation
	for _, sc := range sites {
		for _, rule := range subprocessRules {
			if sc.setsInLiteral(rule.field) || assignsField(fset, sc.body, sc.name, rule.field) {
				continue
			}
			reason, found := waived.lookup(rule.directive, sc.line)
			if found && reason != "" {
				continue
			}
			out = append(out, violation{
				line:  sc.line,
				field: rule.field,
				label: rule.field,
				msg:   sc.message(rule, found),
			})
		}

		// The value check speaks only where Env was assigned; where it was
		// not, the rule above has already said so and saying it twice would
		// bury the one message that names the fix.
		how, ambient := sc.ambientEnv(fset, osPkg)
		if !ambient {
			continue
		}
		reason, found := waived.lookup(ambientEnvRule.directive, sc.line)
		if found && reason != "" {
			continue
		}
		out = append(out, violation{
			line:  sc.line,
			field: ambientEnvRule.field,
			label: ambientEnvRule.label,
			msg:   sc.ambientMessage(how, found),
		})
	}
	return out
}

// target is what the fix line calls this Cmd: the variable it was bound to, or
// a stand-in when there is none to name.
func (sc subprocessCall) target() string {
	if sc.name != "" {
		return sc.name
	}
	return "the command"
}

// subject names what broke the rule, in the words that tell a reader where to
// look. A literal has to say so: "subprocess does not set Dir" sends someone
// hunting for an exec.Command that is not there.
func (sc subprocessCall) subject() string {
	switch {
	case sc.viaLiteral && sc.isGit:
		return "git exec.Cmd literal"
	case sc.viaLiteral:
		return "exec.Cmd literal"
	case sc.isGit:
		return "git subprocess"
	default:
		return "subprocess"
	}
}

// literalNote is the paragraph a composite literal gets and a call does not: how
// to satisfy the rule from inside the literal, and — where the analysis could
// not follow the literal — what it could not follow and why that is a failure
// rather than a pass.
func (sc subprocessCall) literalNote() string {
	if !sc.viaLiteral {
		return ""
	}
	note := "\nnote:  a Cmd built as a struct literal spawns what exec.Command spawns and inherits\n" +
		"       what it inherits, so it is held to the same rules. Set the field inside the\n" +
		"       literal — exec.Cmd{Dir: …, Env: …} — or on the variable it is bound to."
	switch {
	case sc.unkeyed:
		note += "\n       This literal is positional, so its fields cannot be named here and none of\n" +
			"       them counts. Write the field names."
	case sc.name == "":
		note += "\n       This literal is not bound to a single variable, so there is no later\n" +
			"       assignment for this guard to follow: the field has to be in the literal."
	}
	return note
}

// message is the failure a developer reads. It names which rule broke, why that
// rule exists, the assignment that satisfies it, and the escape hatch.
func (sc subprocessCall) message(rule subprocessRule, waivedWithoutReason bool) string {
	head := fmt.Sprintf("%s does not set %s", sc.subject(), rule.field)
	if waivedWithoutReason {
		head = fmt.Sprintf("%s does not set %s, and its %s waiver gives no reason",
			sc.subject(), rule.field, rule.directive)
	}
	g := rule.forProgram(sc.isGit)
	return fmt.Sprintf("%s\n%s\nfix:   %s\nwaive: %s <reason>  on this line or the line above;\n"+
		"       a waiver with no reason waives nothing, and %s does not waive %s%s\nsee:   %s and %s",
		head, g.why, fmt.Sprintf(g.fix, sc.target()), rule.directive,
		rule.directive, otherDirectives(rule.directive), sc.literalNote(), groundRules, brief)
}

// ambientMessage is the failure for the value check. how is what the analysis
// saw, quoted back, because "your Env is inherited" is unactionable next to
// "your Env is os.Environ()".
func (sc subprocessCall) ambientMessage(how string, waivedWithoutReason bool) string {
	target := sc.target()
	head := fmt.Sprintf("%s assigns Env the inherited environment: %s.Env is %s", sc.subject(), target, how)
	if waivedWithoutReason {
		head = fmt.Sprintf("%s assigns Env the inherited environment (%s.Env is %s), and its %s "+
			"waiver gives no reason", sc.subject(), target, how, ambientEnvRule.directive)
	}
	g := ambientEnvRule.general
	if sc.isGit {
		g = ambientEnvRule.git
	}
	return fmt.Sprintf("%s\n%s\nfix:   %s\nwaive: %s <reason>  on this line or the line above;\n"+
		"       a waiver with no reason waives nothing, and %s does not waive %s\n"+
		"scope: this check reads only os.Environ(), an append over one, nil, and a local\n"+
		"       variable assigned one of those. It cannot see through a helper, and it does\n"+
		"       not claim to — that is why an environment is built in a named function.%s\n"+
		"see:   %s and %s",
		head, g.why, fmt.Sprintf(g.fix, target), ambientEnvRule.directive,
		ambientEnvRule.directive, otherDirectives(ambientEnvRule.directive), sc.literalNote(), groundRules, brief)
}

// otherDirectives names the waivers this one does not imply.
func otherDirectives(directive string) string {
	var others []string
	for _, d := range allDirectives {
		if d != directive {
			others = append(others, d)
		}
	}
	return strings.Join(others, " or ")
}

// spawnedProgram reports whether call is exec.Command/exec.CommandContext, and
// returns the expression naming the program.
//
// The program expression is returned rather than required to be a literal:
// every spawn is in scope whether or not the analysis can read what it runs.
// internal/syntax and internal/corpus both compute the program name, and before
// KAN-837 both were invisible here.
func spawnedProgram(execPkg pkgQualifier, call *ast.CallExpr) (ast.Expr, bool) {
	fn, ok := execPkg.reference(call.Fun)
	if !ok {
		return nil, false
	}

	var arg int
	switch fn {
	case "Command":
		arg = 0
	case "CommandContext":
		arg = 1
	default:
		return nil, false
	}
	if len(call.Args) <= arg {
		return nil, false
	}
	return call.Args[arg], true
}

// isGitProgram reports whether prog is a literal git program name. A computed
// one is still a subprocess and still bound by both rules; it only reads its
// failure in the general wording.
func isGitProgram(prog ast.Expr) bool {
	lit, ok := prog.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	base := name[strings.LastIndexAny(name, `/\`)+1:]
	return base == "git" || base == "git.exe"
}

// boundCmds maps each spawn to the local variable its result was assigned to. A
// call that is not bound to a single identifier — used inline, or handed
// straight to a helper — has no entry, so nothing can be found assigning its
// fields and it needs a waiver.
func boundCmds(execPkg pkgQualifier, file *ast.File) map[*ast.CallExpr]string {
	bound := map[*ast.CallExpr]string{}
	walkSingleBindings(file, func(name string, rhs ast.Expr) {
		call, ok := rhs.(*ast.CallExpr)
		if !ok {
			return
		}
		if _, isSpawn := spawnedProgram(execPkg, call); isSpawn {
			bound[call] = name
		}
	})
	return bound
}

// boundCmdLiterals maps each exec.Cmd composite literal to the local variable it
// was assigned to, whether it was written as a value or taken the address of.
//
// A literal with no entry — used inline, handed straight to a helper, or bound
// as one of several values in a multiple assignment — has nothing an assignment
// can be looked up against, so the only fields it can satisfy the rules with are
// the ones written inside the literal itself. That is the fail-closed direction
// and it is the same one boundCmds takes.
func boundCmdLiterals(execPkg pkgQualifier, file *ast.File) map[*ast.CompositeLit]string {
	bound := map[*ast.CompositeLit]string{}
	walkSingleBindings(file, func(name string, rhs ast.Expr) {
		if lit, ok := cmdCompositeLit(execPkg, rhs); ok {
			bound[lit] = name
		}
	})
	return bound
}

// walkSingleBindings calls visit for every `x = <expr>`, `x := <expr>` and
// `var x = <expr>` in file that binds exactly one name to exactly one
// expression. Anything wider than that is not followed, by design.
func walkSingleBindings(file *ast.File, visit func(name string, rhs ast.Expr)) {
	record := func(lhs, rhs []ast.Expr) {
		if len(lhs) != 1 || len(rhs) != 1 {
			return
		}
		if id, ok := lhs[0].(*ast.Ident); ok {
			visit(id.Name, rhs[0])
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			record(s.Lhs, s.Rhs)
		case *ast.ValueSpec:
			names := make([]ast.Expr, 0, len(s.Names))
			for _, id := range s.Names {
				names = append(names, id)
			}
			record(names, s.Values)
		}
		return true
	})
}

// cmdCompositeLit unwraps expr to an exec.Cmd composite literal, seeing through
// the address-of that `&exec.Cmd{…}` puts in front of it.
func cmdCompositeLit(execPkg pkgQualifier, expr ast.Expr) (*ast.CompositeLit, bool) {
	if unary, ok := expr.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		expr = unary.X
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok || !execPkg.isCmdType(lit.Type) {
		return nil, false
	}
	return lit, true
}

// fieldAssign is one `<something>.<field> = <rhs>`.
type fieldAssign struct {
	// name is the identifier the field was selected from, empty when the
	// selection was not from a plain identifier (`a.b.Env = …`). An empty name
	// matches no Cmd, so such an assignment fails closed exactly as an
	// unfollowable Cmd does — while still being visible to the positive
	// control, which is the point of recording it rather than dropping it.
	name string
	rhs  ast.Expr
	line int
}

// fieldAssignments is the one traversal all three rules rest on: it finds every
// assignment to the named field anywhere under n.
//
// Both the assignment checks and the value check go through it, and so does
// TestFieldAssignmentAnalysisSeesEveryAssignment, which reads the same source a
// second way and requires this to have found what it found. That is the check
// on the check: if this traversal stops descending, the rules go quiet and the
// control goes loud.
func fieldAssignments(fset *token.FileSet, n ast.Node, field string) []fieldAssign {
	if n == nil {
		return nil
	}
	var out []fieldAssign
	ast.Inspect(n, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != field {
				continue
			}
			fa := fieldAssign{line: fset.Position(sel.Sel.Pos()).Line}
			if id, isIdent := sel.X.(*ast.Ident); isIdent {
				fa.name = id.Name
			}
			if len(assign.Lhs) == len(assign.Rhs) {
				fa.rhs = assign.Rhs[i]
			}
			out = append(out, fa)
		}
		return true
	})
	return out
}

// assignsField reports whether the function assigns name.field anywhere.
func assignsField(fset *token.FileSet, body *ast.BlockStmt, name, field string) bool {
	if body == nil || name == "" {
		return false
	}
	for _, fa := range fieldAssignments(fset, body, field) {
		if fa.name == name {
			return true
		}
	}
	return false
}

// ambientEnv reports whether this Cmd's Env is one the analysis can see is this
// process's own environment, wherever it was written: in the composite literal
// that built the Cmd, or in an assignment to the variable holding it.
//
// The literal is read first because it is the one a Cmd built as a literal
// usually uses, and because `&exec.Cmd{Env: os.Environ()}` is precisely the
// shape KAN-853 stopped being invisible — it satisfies both assignment rules
// from inside the literal while inheriting everything.
func (sc subprocessCall) ambientEnv(fset *token.FileSet, osPkg pkgQualifier) (string, bool) {
	if env, ok := sc.literal[ambientEnvRule.field]; ok {
		if how, ambient := ambientEnvExpr(osPkg, sc.body, env, map[string]bool{}); ambient {
			return how, true
		}
	}
	return ambientEnvAssignment(fset, osPkg, sc.body, sc.name)
}

// ambientEnvAssignment reports whether the function assigns name.Env something
// the analysis can see is this process's own environment, and how it saw it.
//
// Any such assignment counts, not the last one, matching the flow-insensitive
// bias of the rest of this file: a guard that has to be right about ordering is
// a guard that is wrong about the interesting cases, and ordering is visible in
// review where a wholesale inherit is not.
func ambientEnvAssignment(fset *token.FileSet, osPkg pkgQualifier, body *ast.BlockStmt, name string) (string, bool) {
	if body == nil || name == "" {
		return "", false
	}
	for _, fa := range fieldAssignments(fset, body, ambientEnvRule.field) {
		if fa.name != name || fa.rhs == nil {
			continue
		}
		if how, ok := ambientEnvExpr(osPkg, body, fa.rhs, map[string]bool{}); ok {
			return how, true
		}
	}
	return "", false
}

// pkgQualifier is how one file spells one imported package, which decides what
// a reference into that package looks like in its AST. os and os/exec both need
// it: the value check has to recognise os.Environ, and the construction check
// (KAN-853) has to recognise exec.Cmd.
//
// The dot-import case is carried rather than dropped. It is rare and a linter
// would object to it, but "the guard silently stops seeing os.Environ if
// somebody dot-imports os" is exactly the shape of hole these cards exist to
// close, and it costs one field to not have.
type pkgQualifier struct {
	// name qualifies the reference (os.Environ, exec.Cmd). Empty when the
	// package is dot-imported, blank-imported or absent.
	name string
	// dot is true when the package is dot-imported, so Environ() and Cmd{} are
	// unqualified.
	dot bool
}

// imported reports whether the package is reachable in this file at all.
func (q pkgQualifier) imported() bool { return q.name != "" || q.dot }

// resolveImport returns how file spells the import of path. The last import
// path that matches wins, which cannot happen in compiling Go.
func resolveImport(file *ast.File, path string) pkgQualifier {
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != path {
			continue
		}
		switch {
		case spec.Name == nil:
			return pkgQualifier{name: defaultPkgName(path)}
		case spec.Name.Name == ".":
			return pkgQualifier{dot: true}
		case spec.Name.Name == "_":
			return pkgQualifier{}
		default:
			return pkgQualifier{name: spec.Name.Name}
		}
	}
	return pkgQualifier{}
}

// defaultPkgName is the identifier an unaliased import binds. It is the last
// path element for every package this guard resolves, which is checked rather
// than assumed by the callers passing only "os" and "os/exec".
func defaultPkgName(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// reference reports whether expr names something in this package, and which
// name it is. It is the one place the dot-import and alias spellings are
// decided, so every check that reaches into a package — os.Environ, exec.Command,
// exec.Cmd — reads the same rules rather than each re-deriving them.
func (q pkgQualifier) reference(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		if q.dot {
			return e.Name, true
		}
	case *ast.SelectorExpr:
		if q.name == "" {
			return "", false
		}
		if pkg, isIdent := e.X.(*ast.Ident); isIdent && pkg.Name == q.name {
			return e.Sel.Name, true
		}
	}
	return "", false
}

// isEnvironCall reports whether call is os.Environ() as this file spells it.
func (q pkgQualifier) isEnvironCall(call *ast.CallExpr) (string, bool) {
	name, ok := q.reference(call.Fun)
	if !ok || name != "Environ" {
		return "", false
	}
	if q.dot {
		return "Environ() from a dot-imported os", true
	}
	return q.name + ".Environ()", true
}

// isCmdType reports whether expr is the type exec.Cmd as this file spells it.
func (q pkgQualifier) isCmdType(expr ast.Expr) bool {
	name, ok := q.reference(expr)
	return ok && name == "Cmd"
}

// holdsCmdElements reports whether a composite literal of type t has exec.Cmd
// elements, so the literals inside it are Cmds even though Go lets their type be
// elided: `[]*exec.Cmd{{Path: p}}` builds a Cmd and never writes exec.Cmd in
// front of the brace that does it.
func (q pkgQualifier) holdsCmdElements(t ast.Expr) bool {
	switch e := t.(type) {
	case *ast.ArrayType:
		return q.isCmdTypeOrPointer(e.Elt)
	case *ast.MapType:
		return q.isCmdTypeOrPointer(e.Value)
	}
	return false
}

// isCmdTypeOrPointer accepts exec.Cmd and *exec.Cmd alike. Which one a container
// holds changes nothing about the rules its elements are bound by.
func (q pkgQualifier) isCmdTypeOrPointer(t ast.Expr) bool {
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	return q.isCmdType(t)
}

// ambientEnvExpr decides the four forms described at the top of this file, and
// refuses to guess at anything else.
//
// seen breaks the cycle a self-referential assignment would otherwise cause
// (`env = append(env, …)`), and bounds the walk.
func ambientEnvExpr(osPkg pkgQualifier, body *ast.BlockStmt, expr ast.Expr, seen map[string]bool) (string, bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		// The untyped nil. os/exec: "If Env is nil, the new process uses the
		// current process's environment." It assigns Env, so the assignment
		// rule is satisfied, and it inherits everything, so the rule the
		// assignment rule stands for is not.
		if e.Name == "nil" {
			return "nil, which os/exec documents as inheriting this process's environment", true
		}
		if seen[e.Name] {
			return "", false
		}
		seen[e.Name] = true
		for _, rhs := range assignmentsTo(body, e.Name) {
			if how, ok := ambientEnvExpr(osPkg, body, rhs, seen); ok {
				return fmt.Sprintf("%s, which this function assigns %s", e.Name, how), true
			}
		}
		return "", false

	case *ast.CallExpr:
		if how, ok := osPkg.isEnvironCall(e); ok {
			return how, true
		}
		// append(<ambient>, …) is still the whole ambient environment: adding
		// entries changes what a few variables say and leaves every other one
		// exactly where it was.
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "append" && len(e.Args) > 0 {
			if how, ambient := ambientEnvExpr(osPkg, body, e.Args[0], seen); ambient {
				return fmt.Sprintf("append over %s", how), true
			}
		}
		return "", false

	default:
		return "", false
	}
}

// assignmentsTo returns every expression assigned to the local variable name
// anywhere in body, by := , by = or by a var declaration.
func assignmentsTo(body *ast.BlockStmt, name string) []ast.Expr {
	// A package-level Cmd has no enclosing function, and ast.Inspect over a typed
	// nil panics rather than doing nothing.
	if body == nil {
		return nil
	}
	var out []ast.Expr
	record := func(lhs, rhs []ast.Expr) {
		if len(lhs) != len(rhs) {
			return
		}
		for i, l := range lhs {
			if id, ok := l.(*ast.Ident); ok && id.Name == name {
				out = append(out, rhs[i])
			}
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			record(s.Lhs, s.Rhs)
		case *ast.ValueSpec:
			names := make([]ast.Expr, 0, len(s.Names))
			for _, id := range s.Names {
				names = append(names, id)
			}
			record(names, s.Values)
		}
		return true
	})
	return out
}

func funcBodies(file *ast.File) []*ast.BlockStmt {
	var bodies []*ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			bodies = append(bodies, fn.Body)
		}
	}
	return bodies
}

func enclosingBody(bodies []*ast.BlockStmt, node ast.Node) *ast.BlockStmt {
	for _, body := range bodies {
		if body.Pos() <= node.Pos() && node.End() <= body.End() {
			return body
		}
	}
	return nil
}

type waiverKey struct {
	directive string
	line      int
}

// waiverSet is the file's waiver comments, plus which lines are comments at
// all, so a waiver can be found through a doc block sitting above the call.
type waiverSet struct {
	reasons  map[waiverKey]string
	comments map[int]bool
	// unrecognised holds comments that start like a waiver and are not one,
	// keyed by line. A misspelling waives nothing while reading as a waiver.
	unrecognised map[int]string
}

// lookup finds a waiver for the subprocess on line: on that line itself, or
// anywhere in the unbroken run of comment lines directly above it. The run
// matters because the two rules stack — waiving both puts one directive two
// lines above the call — and a blank line ends it, so a waiver cannot drift
// away from the call it was written for.
func (w waiverSet) lookup(directive string, line int) (string, bool) {
	for l := line; ; l-- {
		if reason, ok := w.reasons[waiverKey{directive: directive, line: l}]; ok {
			return reason, true
		}
		if !w.comments[l-1] {
			return "", false
		}
	}
}

// collectWaivers reads the waiver comments and the reason each one gives. The
// reason is only demanded where a waiver is actually applied, so documenting
// the syntax in a comment does not fail the guard.
func collectWaivers(fset *token.FileSet, file *ast.File) waiverSet {
	out := waiverSet{
		reasons:      map[waiverKey]string{},
		comments:     map[int]bool{},
		unrecognised: map[int]string{},
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			pos := fset.Position(comment.Slash)
			for l := pos.Line; l <= fset.Position(comment.End()).Line; l++ {
				out.comments[l] = true
			}

			text := strings.TrimSpace(comment.Text)
			matched := false
			for _, directive := range allDirectives {
				if reason, found := strings.CutPrefix(text, directive); found {
					out.reasons[waiverKey{directive: directive, line: pos.Line}] = strings.TrimSpace(reason)
					matched = true
				}
			}
			if !matched && looksLikeWaiver(text) {
				out.unrecognised[pos.Line] = text
			}
		}
	}
	return out
}

// looksLikeWaiver reports whether text is trying to be a waiver. The comment
// marker and any spacing are stripped first, so `// kopicode:allow-nodir:` —
// which gofmt will happily produce from a hand-typed comment and which waives
// nothing — is caught rather than ignored.
func looksLikeWaiver(text string) bool {
	stripped := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(text), "/*"))
	return strings.HasPrefix(stripped, directiveStem)
}

// goFile is one parsed file from the tree walk, with the bytes it was parsed
// from so a second, independent reading of the same source is possible.
type goFile struct {
	rel  string
	src  []byte
	fset *token.FileSet
	file *ast.File
}

// walkGoFiles parses every first-party Go file in the repository and hands it to
// visit. A parse error fails the walk rather than being skipped: a file the
// guard cannot read is a file the guard is not checking.
func walkGoFiles(t *testing.T, visit func(goFile)) {
	t.Helper()
	root := repoRoot(t)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		src, readErr := os.ReadFile(path) //nolint:gosec // path comes from a walk of this repository
		if readErr != nil {
			return readErr
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, src, parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		visit(goFile{rel: rel, src: src, fset: fset, file: file})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// skipDir keeps the walk out of directories that hold no first-party Go code.
func skipDir(name string) bool {
	return name == "vendor" || name == "testdata" || strings.HasPrefix(name, ".")
}

// TestSubprocessesSetDirAndEnv is the guard. Every subprocess in this
// repository, in test code as much as in product code, names the directory it
// runs in and builds its own environment.
func TestSubprocessesSetDirAndEnv(t *testing.T) {
	walkGoFiles(t, func(g goFile) {
		for _, v := range subprocessViolations(g.fset, g.file) {
			t.Errorf("%s:%d: %s", g.rel, v.line, v.msg)
		}
	})
}

// TestNoUnrecognisedWaiverDirectives catches the waiver that waives nothing.
//
// KAN-837 renamed the directives, and the failure mode of a rename is a comment
// left behind under the old spelling: it still reads as a waiver to a human, it
// no longer is one, and if the site happens to comply for another reason nobody
// finds out. A typo has the same shape.
func TestNoUnrecognisedWaiverDirectives(t *testing.T) {
	walkGoFiles(t, func(g goFile) {
		for line, text := range collectWaivers(g.fset, g.file).unrecognised {
			t.Errorf("%s:%d: %q is not a waiver this guard recognises, so it waives nothing "+
				"while reading as though it does.\nthe vocabulary is exactly %s, each with a "+
				"reason after the colon.\nsee:   %s and %s",
				g.rel, line, text, strings.Join(allDirectives, ", "), groundRules, brief)
		}
	})
}

// TestSubprocessGuardSeesEveryCallSite is the positive control.
//
// The classic way a go/parser guard dies is that its traversal quietly stops
// finding call sites: every assertion still runs, nothing is left to assert on,
// and the suite is green. So the sites are counted a second time by a reading
// that shares nothing with the first — go/scanner over the same bytes, matching
// the token sequence `exec . Command (` — and every site that reading finds
// must also be one the AST analysis found.
//
// The scanner is the right second opinion precisely because it is not a
// traversal: it cannot skip a subtree, and it classifies comments and string
// literals as single tokens, so this file's own fixtures do not count
// themselves.
func TestSubprocessGuardSeesEveryCallSite(t *testing.T) {
	seen := 0
	walkGoFiles(t, func(g goFile) {
		found := map[int]bool{}
		for _, sc := range subprocessCalls(g.fset, g.file) {
			found[sc.line] = true
		}
		for _, line := range scannedSpawnLines(g.rel, g.src) {
			seen++
			if !found[line] {
				t.Errorf("%s:%d: go/scanner sees an exec.Command here and the guard's AST "+
					"analysis does not.\nEither the traversal in subprocessCalls has stopped "+
					"finding call sites — in which case this guard is asserting nothing — or "+
					"this call is written in a shape it cannot follow, which it must not "+
					"silently allow.\nsee:   %s", g.rel, line, groundRules)
			}
		}
	})

	if seen == 0 {
		t.Fatal("the token scan found no exec.Command call site anywhere in the repository, " +
			"so this positive control proved nothing. Either the walk is broken or this " +
			"repository no longer spawns subprocesses, and only one of those is plausible.")
	}
}

// scannedSpawnLines reports the line of every `exec.Command(` and
// `exec.CommandContext(` in src, read as a token stream.
//
// It deliberately keys on the literal package identifier: an aliased import of
// os/exec would be missed here and caught by the AST side, which is the safe
// direction for a control whose job is to prove the AST side still sees things.
func scannedSpawnLines(name string, src []byte) []int {
	var fset token.FileSet
	f := fset.AddFile(name, -1, len(src))

	var s scanner.Scanner
	s.Init(f, src, nil, 0)

	// A four-token window: exec, '.', Command, '('.
	type tok struct {
		tok token.Token
		lit string
		pos token.Pos
	}
	var window [4]tok

	var lines []int
	for {
		pos, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		window[0], window[1], window[2], window[3] = window[1], window[2], window[3], tok{tok: t, lit: lit, pos: pos}

		if window[3].tok != token.LPAREN ||
			window[0].tok != token.IDENT || window[0].lit != "exec" ||
			window[1].tok != token.PERIOD ||
			window[2].tok != token.IDENT {
			continue
		}
		if window[2].lit != "Command" && window[2].lit != "CommandContext" {
			continue
		}
		lines = append(lines, fset.Position(window[0].pos).Line)
	}
	return lines
}

// TestFieldAssignmentAnalysisSeesEveryAssignment is the positive control for
// the other traversal — the one that decides whether Dir and Env were assigned
// and, since KAN-845, what Env was assigned.
//
// The control above proves the guard still finds subprocesses. It says nothing
// about [fieldAssignments], which is where all three rules actually conclude:
// if that walk stopped descending, every subprocess would read as unassigned
// and the suite would go *red*, which is the safe direction — but the value
// check fails the other way. An Env assignment the walk cannot see is an Env
// value never inspected, and that is a silent green.
//
// So it is checked in both directions rather than counted. go/scanner reads the
// same bytes as a token stream, matching `. <field> =`, and the two sets of
// lines must be equal: a line the scanner sees and the AST does not means the
// traversal has a hole, and a line the AST reports and the scanner does not
// means it is matching something that is not there. Neither is allowed to be
// discovered later.
//
// What this cannot cover, said plainly so nobody reads it as more than it is:
// it compares *lines*, so a [fieldAssignments] that found every assignment and
// dropped every right-hand side would pass it while the value check went
// silent. That half is covered by a different mechanism —
// TestSubprocessGuardCatchesViolations drives the analysis over fixtures whose
// answers are written down, and dropping the right-hand side turns twelve of
// its cases red. Both breakages were performed and both were watched. Between
// them: this one proves the walk still reaches the real source, and that one
// proves the walk still reads what it reaches.
func TestFieldAssignmentAnalysisSeesEveryAssignment(t *testing.T) {
	fields := []string{"Dir", "Env"}
	seen := map[string]int{}

	walkGoFiles(t, func(g goFile) {
		for _, field := range fields {
			byAST := map[int]bool{}
			for _, fa := range fieldAssignments(g.fset, g.file, field) {
				byAST[fa.line] = true
			}
			byScanner := map[int]bool{}
			for _, line := range scannedFieldAssignLines(g.rel, g.src, field) {
				byScanner[line] = true
				seen[field]++
			}

			for line := range byScanner {
				if !byAST[line] {
					t.Errorf("%s:%d: go/scanner sees a .%s assignment here and fieldAssignments "+
						"does not.\nEvery subprocess rule concludes through that walk, so a hole in "+
						"it means an Env value that is never inspected — a green suite over the "+
						"exact gap KAN-845 closed.\nsee:   %s", g.rel, line, field, groundRules)
				}
			}
			for line := range byAST {
				if !byScanner[line] {
					t.Errorf("%s:%d: fieldAssignments reports a .%s assignment here and the token "+
						"scan finds none.\nEither the scan has grown blind to an assignment "+
						"operator it should recognise, or the AST side is matching something that "+
						"is not an assignment at all; the two readings must agree.\nsee:   %s",
						g.rel, line, field, groundRules)
				}
			}
		}
	})

	for _, field := range fields {
		if seen[field] == 0 {
			t.Fatalf("the token scan found no .%s assignment anywhere in the repository, so this "+
				"positive control proved nothing about %s. Either the walk is broken or no "+
				"subprocess in this repository sets %s, and the guard above says otherwise.",
				field, field, field)
		}
	}
}

// scannedFieldAssignLines reports the line of every `.<field> =` in src, read as
// a token stream, keyed on the field name's own position so it lines up with
// what fieldAssignments records.
//
// ADD_ASSIGN is accepted alongside ASSIGN because `cmd.Dir += …` is legal Go and
// the AST side counts it. A compound operator that is legal here and not listed
// would fail the control rather than pass it, which is the right direction: the
// two readings disagreeing is the signal.
func scannedFieldAssignLines(name string, src []byte, field string) []int {
	var fset token.FileSet
	f := fset.AddFile(name, -1, len(src))

	var s scanner.Scanner
	s.Init(f, src, nil, 0)

	type tok struct {
		tok token.Token
		lit string
		pos token.Pos
	}
	var window [3]tok

	var lines []int
	for {
		pos, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		window[0], window[1], window[2] = window[1], window[2], tok{tok: t, lit: lit, pos: pos}

		if window[0].tok != token.PERIOD ||
			window[1].tok != token.IDENT || window[1].lit != field {
			continue
		}
		if window[2].tok != token.ASSIGN && window[2].tok != token.ADD_ASSIGN {
			continue
		}
		lines = append(lines, fset.Position(window[1].pos).Line)
	}
	return lines
}

// TestSubprocessGuardCatchesViolations is the guard on the guard. A static
// check that has never been watched fail may be asserting nothing, and the tree
// being green is equally consistent with the analysis matching nothing at all.
func TestSubprocessGuardCatchesViolations(t *testing.T) {
	const preamble = "package p\n\nimport (\n\t\"context\"\n\t\"os\"\n\t\"os/exec\"\n)\n\n" +
		"func f(ctx context.Context, dir string, env []string, prog string) {\n"

	tests := []struct {
		name string
		body string
		// src replaces preamble+body when a case needs a different import
		// block — an aliased or dot-imported os, which the preamble cannot
		// express.
		src  string
		want []string // the violation labels, in the order they are reported
	}{
		{
			name: "git with no Dir and no Env",
			body: "\tcmd := exec.Command(\"git\", \"status\")\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "git with Dir but no Env is the one that bit",
			body: "\tcmd := exec.Command(\"git\", \"init\")\n\tcmd.Dir = dir\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "git with Env but no Dir",
			body: "\tcmd := exec.Command(\"git\", \"init\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "git with both set is clean",
			body: "\tcmd := exec.Command(\"git\", \"init\")\n\tcmd.Dir = dir\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "an absolute program path is still git",
			body: "\tcmd := exec.Command(\"/usr/bin/git\", \"init\")\n\tcmd.Dir = dir\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "a non-git command with no Dir and no Env",
			body: "\tcmd := exec.Command(\"go\", \"build\", \"./...\")\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "a non-git command missing only Dir",
			body: "\tcmd := exec.Command(\"node\", \"--check\", \"a.js\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "a non-git command missing only Env",
			body: "\tcmd := exec.Command(\"python3\", \"-m\", \"py_compile\")\n\tcmd.Dir = dir\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "a non-git command with both set is clean",
			body: "\tcmd := exec.Command(\"gofmt\", \"-l\", \".\")\n\tcmd.Dir = dir\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "a computed program name is a subprocess like any other",
			body: "\tcmd := exec.Command(prog, \"-l\")\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "CommandContext skips the context argument",
			body: "\tcmd := exec.CommandContext(ctx, \"gofmt\", \"-l\")\n\tcmd.Dir = dir\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "an unbound Cmd cannot be checked and so fails closed",
			body: "\t_ = exec.Command(\"taskkill\", \"/PID\", \"7\").Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "a Cmd handed to a helper escapes and fails closed",
			body: "\tcmd := exec.Command(\"go\", \"vet\")\n\tconfigure(cmd)\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "waivers with reasons are honoured on a non-git command",
			body: "\t//kopicode:allow-nodir: kills a pid, so no directory is the right one\n" +
				"\t//kopicode:allow-noenv: needs the ambient PATH to find the binary at all\n" +
				"\tcmd := exec.Command(\"taskkill\", \"/PID\", \"7\")\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "waiving nodir on a non-git command does not waive noenv",
			body: "\t//kopicode:allow-nodir: kills a pid, so no directory is the right one\n" +
				"\tcmd := exec.Command(\"taskkill\", \"/PID\", \"7\")\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "waiving noenv on a non-git command does not waive nodir",
			body: "\t//kopicode:allow-noenv: needs the ambient PATH to find the binary at all\n" +
				"\tcmd := exec.Command(\"taskkill\", \"/PID\", \"7\")\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "a waiver without a reason does not waive, on a non-git command",
			body: "\t//kopicode:allow-nodir:\n\tcmd := exec.Command(\"ps\", \"-p\", \"7\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "a waiver without a reason does not waive, on git either",
			body: "\t//kopicode:allow-git-noenv:\n\tcmd := exec.Command(\"git\", \"status\")\n\tcmd.Dir = dir\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "the retired git-specific spelling waives nothing",
			body: "\t//kopicode:allow-git-nodir: the ambient repository is the subject\n" +
				"\tcmd := exec.Command(\"git\", \"status\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "a waiver does not reach a call two lines below it",
			body: "\t//kopicode:allow-nodir: too far away to apply\n\n\tcmd := exec.Command(\"git\", \"status\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "a call that is not a subprocess is not this guard's business",
			body: "\t_ = exec.LookPath(\"gofmt\")\n",
			want: nil,
		},

		// --- the value check (KAN-845) ------------------------------------
		//
		// The first of these is the gap the card names: it satisfied the
		// assignment rule while inheriting everything, and internal/build did
		// exactly this at both of its sites.
		{
			name: "os.Environ satisfies the assignment rule and must not satisfy the value rule",
			body: "\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n\tcmd.Env = os.Environ()\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "os.Environ on a git command is the mechanism that carried GIT_DIR",
			body: "\tcmd := exec.Command(\"git\", \"init\")\n\tcmd.Dir = dir\n\tcmd.Env = os.Environ()\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "appending to os.Environ is still os.Environ",
			body: "\tcmd := exec.Command(\"go\", \"test\")\n\tcmd.Dir = dir\n\tcmd.Env = append(os.Environ(), \"GOFLAGS=\")\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "a nil Env inherits, and reads as though it decided not to",
			body: "\tcmd := exec.Command(\"go\", \"vet\")\n\tcmd.Dir = dir\n\tcmd.Env = nil\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "a local variable holding os.Environ is followed",
			body: "\tinherited := os.Environ()\n\tcmd := exec.Command(\"node\", \"--check\")\n\tcmd.Dir = dir\n" +
				"\tcmd.Env = inherited\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "a local variable declared with var and holding os.Environ is followed",
			body: "\tvar inherited = os.Environ()\n\tcmd := exec.Command(\"node\", \"--check\")\n\tcmd.Dir = dir\n" +
				"\tcmd.Env = inherited\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "a self-referential append terminates rather than recursing",
			body: "\te := os.Environ()\n\te = append(e, \"X=1\")\n\tcmd := exec.Command(\"go\", \"list\")\n" +
				"\tcmd.Dir = dir\n\tcmd.Env = e\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "an environment from a helper is not decided, which is the funnel",
			body: "\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n\tcmd.Env = baseEnv()\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "an environment built from a literal is not inherited",
			body: "\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n" +
				"\tcmd.Env = []string{\"PATH=\" + os.Getenv(\"PATH\")}\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "a local variable built by filtering os.Environ is not the ambient environment",
			body: "\tout := make([]string, 0, len(os.Environ()))\n\tfor _, kv := range os.Environ() {\n" +
				"\t\tout = append(out, kv)\n\t}\n\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n" +
				"\tcmd.Env = out\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "the value check does not replace the assignment checks",
			body: "\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Env = os.Environ()\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env value"},
		},
		{
			name: "the ambientenv waiver with a reason is honoured",
			body: "\t//kopicode:allow-ambientenv: this test is about the ambient toolchain itself\n" +
				"\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n\tcmd.Env = os.Environ()\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "an ambientenv waiver without a reason waives nothing",
			body: "\t//kopicode:allow-ambientenv:\n" +
				"\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n\tcmd.Env = os.Environ()\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "noenv does not waive ambientenv",
			body: "\t//kopicode:allow-noenv: a reason that is about the other rule entirely\n" +
				"\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n\tcmd.Env = os.Environ()\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "ambientenv does not waive noenv",
			body: "\t//kopicode:allow-ambientenv: a reason about a value that is never assigned\n" +
				"\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "ambientenv does not waive nodir",
			body: "\t//kopicode:allow-ambientenv: a reason about the value, not the directory\n" +
				"\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "an aliased os import is still os.Environ",
			body: "",
			// The preamble does not alias os, so this case supplies its own file.
			src: "package p\n\nimport (\n\tstdos \"os\"\n\t\"os/exec\"\n)\n\nfunc f(dir string) {\n" +
				"\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n\tcmd.Env = stdos.Environ()\n" +
				"\t_ = cmd.Run()\n}\n",
			want: []string{"Env value"},
		},
		{
			name: "a dot-imported os makes Environ unqualified, and it is still Environ",
			body: "",
			src: "package p\n\nimport (\n\t. \"os\"\n\t\"os/exec\"\n)\n\nfunc f(dir string) {\n" +
				"\tcmd := exec.Command(\"go\", \"build\")\n\tcmd.Dir = dir\n\tcmd.Env = Environ()\n" +
				"\t_ = cmd.Run()\n}\n",
			want: []string{"Env value"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.src
			if src == "" {
				src = preamble + tc.body + "}\n"
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}

			var got []string
			for _, v := range subprocessViolations(fset, file) {
				got = append(got, v.label)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("reported %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCmdLiteralGuardCatchesViolations is the guard on the half of the guard
// KAN-853 added. A composite literal of exec.Cmd is the most direct way to build
// a subprocess and, until this card, the one way past every check in this file:
// a &exec.Cmd{} with no Dir and an inherited Env passed all three rules because
// none of them was looking.
//
// The table is the call table's shape deliberately, case for case where a case
// exists on both sides, because the claim being made is that the two
// constructions are held to the same rules and not to similar ones.
func TestCmdLiteralGuardCatchesViolations(t *testing.T) {
	const preamble = "package p\n\nimport (\n\t\"os\"\n\t\"os/exec\"\n)\n\n" +
		"func f(dir string, env []string, prog string) {\n"

	tests := []struct {
		name string
		body string
		// src replaces preamble+body when a case needs a different import
		// block, which is how the aliased and dot-imported spellings are
		// reached.
		src  string
		want []string
	}{
		{
			name: "a bare Cmd literal sets neither, and is the hole this card closed",
			body: "\tcmd := &exec.Cmd{Path: prog}\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "fields written in the literal satisfy both rules",
			body: "\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: env}\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "Dir in the literal and Env nowhere is the shape that bit, rebuilt",
			body: "\tcmd := &exec.Cmd{Path: prog, Dir: dir}\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "Env in the literal and Dir nowhere",
			body: "\tcmd := &exec.Cmd{Path: prog, Env: env}\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "a value literal is a Cmd exactly as a pointer one is",
			body: "\tcmd := exec.Cmd{Path: prog}\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "fields assigned after the literal count",
			body: "\tcmd := &exec.Cmd{Path: prog}\n\tcmd.Dir = dir\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "the literal and the assignments may split the work between them",
			body: "\tcmd := &exec.Cmd{Path: prog, Dir: dir}\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "an inline literal has no variable to follow and fails closed",
			body: "\t_ = (&exec.Cmd{Path: prog}).Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "an inline literal that sets both is complete where it stands",
			body: "\t_ = (&exec.Cmd{Path: prog, Dir: dir, Env: env}).Run()\n",
			want: nil,
		},
		{
			name: "a literal handed straight to a helper fails closed",
			body: "\tconfigure(&exec.Cmd{Path: prog})\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "a literal bound as one of several values is not followed",
			body: "\tcmd, err := &exec.Cmd{Path: prog}, error(nil)\n\tcmd.Dir = dir\n\tcmd.Env = env\n\t_ = err\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "a positional literal names no fields, so it satisfies none",
			body: "\tcmd := &exec.Cmd{prog}\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "a literal of another exec type is not this guard's business",
			body: "\t_ = exec.Error{Name: prog}\n",
			want: nil,
		},

		// --- the elided type, which writes no exec.Cmd in front of its brace ---
		{
			name: "a Cmd inside a slice of Cmds elides its type and is still a Cmd",
			body: "\tcmds := []*exec.Cmd{{Path: prog}}\n\t_ = cmds\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "an elided Cmd that sets both fields is clean",
			body: "\tcmds := []*exec.Cmd{{Path: prog, Dir: dir, Env: env}}\n\t_ = cmds\n",
			want: nil,
		},
		{
			name: "a slice of values rather than pointers elides the same way",
			body: "\tcmds := []exec.Cmd{{Path: prog}}\n\t_ = cmds\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "a map of Cmds elides the type behind a key",
			body: "\tcmds := map[string]*exec.Cmd{\"build\": {Path: prog}}\n\t_ = cmds\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "an elided Cmd is read for its Env value too",
			body: "\tcmds := []*exec.Cmd{{Path: prog, Dir: dir, Env: os.Environ()}}\n\t_ = cmds\n",
			want: []string{"Env value"},
		},
		{
			name: "an empty container of Cmds builds none",
			body: "\tcmds := []*exec.Cmd{}\n\t_ = cmds\n",
			want: nil,
		},
		{
			name: "a container of something else does not lend its elements a Cmd type",
			body: "\tspecs := []struct{ Path string }{{Path: prog}}\n\t_ = specs\n",
			want: nil,
		},
		{
			name: "a signature naming *exec.Cmd builds nothing",
			src: "package p\n\nimport \"os/exec\"\n\nfunc newCmd(prog string) *exec.Cmd {\n" +
				"\treturn nil\n}\n\nfunc take(cmd *exec.Cmd) {}\n",
			want: nil,
		},
		{
			name: "a var declaration of a Cmd type is not a composite literal",
			body: "\tvar cmd exec.Cmd\n\t_ = cmd\n",
			want: nil,
		},

		// --- the value check, reached through the literal --------------------
		{
			name: "os.Environ in the literal satisfies the assignment rule and must not satisfy the value rule",
			body: "\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: os.Environ()}\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "appending to os.Environ in the literal is still os.Environ",
			body: "\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: append(os.Environ(), \"GOFLAGS=\")}\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "an explicit nil Env in the literal reads as a decision and is not one",
			body: "\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: nil}\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "a local variable holding os.Environ is followed into the literal",
			body: "\tinherited := os.Environ()\n\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: inherited}\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "an environment from a named builder is not decided here, which is the funnel",
			body: "\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: baseEnv()}\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "an inline literal is read for its Env value too",
			body: "\t_ = (&exec.Cmd{Path: prog, Dir: dir, Env: os.Environ()}).Run()\n",
			want: []string{"Env value"},
		},
		{
			name: "the value check does not replace the assignment checks on a literal either",
			body: "\tcmd := &exec.Cmd{Path: prog, Env: os.Environ()}\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env value"},
		},

		// --- the waiver vocabulary, unchanged ---------------------------------
		{
			name: "waivers with reasons are honoured on a literal",
			body: "\t//kopicode:allow-nodir: kills a pid, so no directory is the right one\n" +
				"\t//kopicode:allow-noenv: needs the ambient PATH to find the binary at all\n" +
				"\tcmd := &exec.Cmd{Path: prog}\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "waiving nodir on a literal does not waive noenv",
			body: "\t//kopicode:allow-nodir: kills a pid, so no directory is the right one\n" +
				"\tcmd := &exec.Cmd{Path: prog}\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "a waiver without a reason does not waive a literal either",
			body: "\t//kopicode:allow-nodir:\n\tcmd := &exec.Cmd{Path: prog, Env: env}\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "the ambientenv waiver reaches an Env written in the literal",
			body: "\t//kopicode:allow-ambientenv: this test is about the ambient toolchain itself\n" +
				"\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: os.Environ()}\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "noenv does not waive ambientenv on a literal",
			body: "\t//kopicode:allow-noenv: a reason that is about the other rule entirely\n" +
				"\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: os.Environ()}\n\t_ = cmd.Run()\n",
			want: []string{"Env value"},
		},

		// --- how the file spells os/exec ---------------------------------------
		{
			name: "an aliased os/exec import still spells the literal Cmd",
			src: "package p\n\nimport (\n\t\"os\"\n\tosexec \"os/exec\"\n)\n\nfunc f(dir string, prog string) {\n" +
				"\tcmd := &osexec.Cmd{Path: prog, Dir: dir, Env: os.Environ()}\n\t_ = cmd.Run()\n}\n",
			want: []string{"Env value"},
		},
		{
			name: "a dot-imported os/exec makes the literal unqualified, and it is still a Cmd",
			src: "package p\n\nimport (\n\t. \"os/exec\"\n)\n\nfunc f(prog string) {\n" +
				"\tcmd := &Cmd{Path: prog}\n\t_ = cmd.Run()\n}\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "a blank-imported os/exec cannot name a Cmd, so there is nothing to find",
			src: "package p\n\nimport (\n\t_ \"os/exec\"\n)\n\ntype Cmd struct{ Path string }\n\n" +
				"func f(prog string) {\n\tcmd := &Cmd{Path: prog}\n\t_ = cmd\n}\n",
			want: nil,
		},

		// --- calls and literals in one file ------------------------------------
		//
		// Two traversals produce these, so without an ordering step the calls
		// would all report before any literal however the source reads. The
		// labels here differ on purpose: swap the sort out and this case reports
		// them the other way round.
		{
			name: "a literal and a call report in source order",
			body: "\tlit := &exec.Cmd{Path: prog, Dir: dir, Env: os.Environ()}\n" +
				"\tcall := exec.Command(prog)\n\tcall.Env = env\n\t_ = lit.Run()\n\t_ = call.Run()\n",
			want: []string{"Env value", "Dir"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.src
			if src == "" {
				src = preamble + tc.body + "}\n"
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}

			var got []string
			for _, v := range subprocessViolations(fset, file) {
				got = append(got, v.label)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("reported %v, want %v", got, tc.want)
			}
		})
	}
}

// cmdLiteralControlSrc is the source [TestCmdLiteralAnalysisSeesEveryLiteral]
// reads two ways. It lives outside the test because the point is that both
// readings get the same bytes.
//
// Every literal in it is compliant. The control is about whether the analysis
// *sees* a literal, and a control whose fixture also has to be a violation
// conflates two failures into one red.
const cmdLiteralControlSrc = `package p

import (
	"os"
	"os/exec"
)

func f(dir string, env []string, prog string) {
	a := &exec.Cmd{Path: prog, Dir: dir, Env: env}
	b := exec.Cmd{Path: prog, Dir: dir, Env: env}
	_ = (&exec.Cmd{Path: prog, Dir: dir, Env: env}).Run()
	c := &exec.Cmd{Path: prog}
	c.Dir = dir
	c.Env = env
	_ = os.Getpid()
	_, _, _ = a, b, c
}
`

// TestCmdLiteralAnalysisSeesEveryLiteral is the positive control for the walk
// KAN-853 added, and it is built differently from the other two on purpose.
//
// The controls above can demand a non-zero count from the repository, because a
// repository with no exec.Command in it would be the surprising thing. This one
// cannot: there is no exec.Cmd literal in the tree today and the hoped-for
// future is that there never is one. A control that asserts nothing whenever the
// tree is clean is a control that goes silent exactly when the guard does, so
// this one carries its own source with literals in it, and the count that must
// be non-zero is over that.
//
// The repository is then walked as well, holding the same equality: a literal
// go/scanner finds and the AST walk does not is a hole, whether or not one
// exists today.
func TestCmdLiteralAnalysisSeesEveryLiteral(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "control.go", cmdLiteralControlSrc, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing the control source: %v", err)
	}

	byAST := map[int]bool{}
	for _, sc := range cmdLiterals(fset, file) {
		byAST[sc.line] = true
	}
	byScanner := map[int]bool{}
	for _, line := range scannedCmdLiteralLines("control.go", []byte(cmdLiteralControlSrc)) {
		byScanner[line] = true
	}

	if len(byScanner) == 0 {
		t.Fatal("the token scan found no exec.Cmd literal in this test's own control source, " +
			"so the reading it is meant to check the AST walk against proved nothing")
	}
	for line := range byScanner {
		if !byAST[line] {
			t.Errorf("control.go:%d: go/scanner sees an exec.Cmd literal here and cmdLiterals "+
				"does not. A Cmd this walk cannot see is a Cmd held to none of the three rules, "+
				"which is the state KAN-853 found the guard in.", line)
		}
	}
	for line := range byAST {
		if !byScanner[line] {
			t.Errorf("control.go:%d: cmdLiterals reports an exec.Cmd literal here and the token "+
				"scan finds none; the two readings must agree.", line)
		}
	}

	walkGoFiles(t, func(g goFile) {
		found := map[int]bool{}
		for _, sc := range cmdLiterals(g.fset, g.file) {
			found[sc.line] = true
		}
		for _, line := range scannedCmdLiteralLines(g.rel, g.src) {
			if !found[line] {
				t.Errorf("%s:%d: go/scanner sees an exec.Cmd literal here and the guard's AST "+
					"analysis does not.\nEither the traversal in cmdLiterals has stopped finding "+
					"them, or this literal is written in a shape it cannot follow, which it must "+
					"not silently allow.\nsee:   %s", g.rel, line, groundRules)
			}
		}
	})
}

// typeContextTokens are the tokens after which `exec.Cmd {` is a type followed
// by an unrelated brace rather than a composite literal.
//
// This list is not a guess. `func newShellCmd(command string) *exec.Cmd {` in
// internal/tools is the shape that made it necessary: a return type and the
// opening brace of the function body, three tokens apart from a literal and
// identical to one in a four-token window. `)` covers the same signature written
// without the pointer, `]` covers `[]exec.Cmd{…}` — a slice literal, whose type
// is not exec.Cmd — and an identifier covers a declaration such as
// `var cmd exec.Cmd`.
var typeContextTokens = map[token.Token]bool{
	token.MUL:    true,
	token.RPAREN: true,
	token.RBRACK: true,
	token.IDENT:  true,
	token.CHAN:   true,
	token.ARROW:  true,
}

// scannedCmdLiteralLines reports the line of every `exec.Cmd{` in src that is a
// composite literal, read as a token stream.
//
// As with the call scan, it keys on the literal package identifier, so an
// aliased or dot-imported os/exec is missed here and caught by the AST side.
// That is the safe direction for a control whose job is to prove the AST side
// still sees things — and so is the elided form inside `[]*exec.Cmd{{…}}`, which
// puts no `exec.Cmd` in front of its brace at all and which the AST side
// resolves from the enclosing literal's element type.
func scannedCmdLiteralLines(name string, src []byte) []int {
	var fset token.FileSet
	f := fset.AddFile(name, -1, len(src))

	var s scanner.Scanner
	s.Init(f, src, nil, 0)

	// A five-token window: whatever came before, then exec, '.', Cmd, '{'.
	type tok struct {
		tok token.Token
		lit string
		pos token.Pos
	}
	var window [5]tok

	var lines []int
	for {
		pos, t, lit := s.Scan()
		if t == token.EOF {
			break
		}
		window[0], window[1], window[2], window[3], window[4] =
			window[1], window[2], window[3], window[4], tok{tok: t, lit: lit, pos: pos}

		if window[4].tok != token.LBRACE ||
			window[1].tok != token.IDENT || window[1].lit != "exec" ||
			window[2].tok != token.PERIOD ||
			window[3].tok != token.IDENT || window[3].lit != "Cmd" {
			continue
		}
		if typeContextTokens[window[0].tok] {
			continue
		}
		lines = append(lines, fset.Position(window[1].pos).Line)
	}
	return lines
}

// TestCmdLiteralMessageExplainsItself holds the literal's failure to the same
// standard as the call's, plus the two things only it has to say: that the Cmd
// in question is a struct literal, and — where the analysis could not follow the
// literal — what it could not follow. A reader who is told "subprocess does not
// set Dir" and has no exec.Command on that line learns nothing.
func TestCmdLiteralMessageExplainsItself(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "a git literal still names the incident",
			body: "\tcmd := &exec.Cmd{Args: []string{\"git\", \"status\"}}\n\t_ = cmd.Run()\n",
			want: []string{"git exec.Cmd literal", "core.bare=true", "GIT_DIR", "note:",
				groundRules, brief, "waive:"},
		},
		{
			name: "a non-git literal names what an inherited environment does",
			body: "\tcmd := &exec.Cmd{Path: \"/usr/bin/go\"}\n\t_ = cmd.Run()\n",
			want: []string{"exec.Cmd literal", "GOFLAGS", "PATH", "note:", groundRules, brief},
		},
		{
			name: "an unbound literal says there is nothing to follow",
			body: "\t_ = (&exec.Cmd{Path: prog}).Run()\n",
			want: []string{"exec.Cmd literal", "not bound to a single variable"},
		},
		{
			name: "a positional literal says why none of its fields counts",
			body: "\tcmd := &exec.Cmd{prog}\n\t_ = cmd.Run()\n",
			want: []string{"exec.Cmd literal", "positional", "Write the field names"},
		},
		{
			name: "the value check on a literal keeps its scope paragraph and gains the note",
			body: "\tcmd := &exec.Cmd{Path: prog, Dir: dir, Env: os.Environ()}\n\t_ = cmd.Run()\n",
			want: []string{"exec.Cmd literal", "os.Environ()", "scope:", "note:",
				allowAmbientEnv, allowNoDir, allowNoEnv},
		},
	}

	const preamble = "package p\n\nimport (\n\t\"os\"\n\t\"os/exec\"\n)\n\n" +
		"func f(dir string, prog string) {\n"

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", preamble+tc.body+"}\n", parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}

			violations := subprocessViolations(fset, file)
			if len(violations) == 0 {
				t.Fatal("the fixture produced no violation, so there is no message to read")
			}
			var joined string
			for _, v := range violations {
				joined += v.msg + "\n"
			}
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("the failures do not mention %q:\n%s", want, joined)
				}
			}
		})
	}
}

// TestDefaultPkgNameMatchesTheImportsThisGuardResolves checks the assumption
// [defaultPkgName] is built on, rather than leaving it in a doc comment. It is
// the last path element for os and os/exec, and it is not for every package —
// so this holds it to the two paths the guard actually passes it, and would go
// red if a third arrived whose package name is not its last element.
func TestDefaultPkgNameMatchesTheImportsThisGuardResolves(t *testing.T) {
	for path, want := range map[string]string{"os": "os", "os/exec": "exec"} {
		if got := defaultPkgName(path); got != want {
			t.Errorf("defaultPkgName(%q) = %q, want %q", path, got, want)
		}
		src := "package p\n\nimport \"" + path + "\"\n"
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parsing fixture: %v", err)
		}
		q := resolveImport(file, path)
		if !q.imported() || q.dot || q.name != want {
			t.Errorf("resolveImport(%q) = %+v, want the unaliased name %q", path, q, want)
		}
	}

	for _, tc := range []struct {
		name string
		spec string
		want pkgQualifier
	}{
		{name: "an alias", spec: "osexec \"os/exec\"", want: pkgQualifier{name: "osexec"}},
		{name: "a dot import", spec: ". \"os/exec\"", want: pkgQualifier{dot: true}},
		{name: "a blank import", spec: "_ \"os/exec\"", want: pkgQualifier{}},
		{name: "no import at all", spec: "\"strings\"", want: pkgQualifier{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", "package p\n\nimport "+tc.spec+"\n", parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}
			if got := resolveImport(file, "os/exec"); got != tc.want {
				t.Errorf("resolveImport = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestUnrecognisedWaiverDetection covers the spellings that must not pass for
// waivers, including the ones this rename retired.
func TestUnrecognisedWaiverDetection(t *testing.T) {
	tests := []struct {
		name    string
		comment string
		want    bool
	}{
		{name: "the retired nodir spelling", comment: "//kopicode:allow-git-nodir: a reason", want: true},
		{name: "the retired noenv spelling", comment: "//kopicode:allow-git-noenv: a reason", want: true},
		{name: "a typo", comment: "//kopicode:allow-no-env: a reason", want: true},
		{name: "a space after the marker", comment: "// kopicode:allow-nodir: a reason", want: true},
		{name: "a typo in the value directive", comment: "//kopicode:allow-ambient-env: a reason", want: true},
		{name: "the value directive without its suffix", comment: "//kopicode:allow-ambient: a reason", want: true},
		{name: "the current nodir spelling", comment: "//kopicode:allow-nodir: a reason", want: false},
		{name: "the current noenv spelling", comment: "//kopicode:allow-noenv: a reason", want: false},
		{name: "the current ambientenv spelling", comment: "//kopicode:allow-ambientenv: a reason", want: false},
		{name: "an ordinary comment", comment: "// nothing to see", want: false},
		{name: "another tool's directive", comment: "//nolint:gosec // argv is built here", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			src := "package p\n\nfunc f() {\n\t" + tc.comment + "\n\t_ = 1\n}\n"
			file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}
			_, got := collectWaivers(fset, file).unrecognised[4]
			if got != tc.want {
				t.Errorf("unrecognised = %v, want %v for %q", got, tc.want, tc.comment)
			}
		})
	}
}

// TestSubprocessGuardMessageExplainsItself keeps the failure worth reading. The
// message is the whole deliverable of a static guard: it has to survive being
// met by someone who has never read this file. It also has to keep saying the
// git-specific thing for a git command, which is the half KAN-837 was told not
// to level down.
func TestSubprocessGuardMessageExplainsItself(t *testing.T) {
	tests := []struct {
		name string
		prog string
		want []string
	}{
		{
			name: "git names the incident",
			prog: `"git"`,
			want: []string{groundRules, brief, "waive:", "core.bare=true", "GIT_DIR"},
		},
		{
			name: "a non-git command names what an inherited environment does",
			prog: `"go"`,
			want: []string{groundRules, brief, "waive:", "GOFLAGS", "PATH"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			src := "package p\n\nimport \"os/exec\"\n\nfunc f() {\n\tcmd := exec.Command(" +
				tc.prog + ")\n\t_ = cmd.Run()\n}\n"
			file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}

			violations := subprocessViolations(fset, file)
			if len(violations) != 2 {
				t.Fatalf("got %d violations, want 2", len(violations))
			}

			joined := violations[0].msg + "\n" + violations[1].msg
			for _, v := range violations {
				if !strings.Contains(v.msg, "cmd."+v.field) {
					t.Errorf("the %s failure does not name the assignment that fixes it:\n%s", v.field, v.msg)
				}
			}
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("the failures do not mention %q:\n%s", want, joined)
				}
			}
		})
	}
}

// TestAmbientEnvMessageExplainsItselfAndItsLimits holds the value check's
// failure to the same standard, plus one the others do not have to meet.
//
// It must quote back what the analysis saw, because "your Env is inherited" is
// unactionable next to "your Env is os.Environ()"; it must name the directive
// that waives it and the two it does not; and it must state its own scope. That
// last one is the part worth a test: a reader who hits this at 2am and takes it
// for a proof will conclude that everything the guard is quiet about is
// deliberate, which is exactly the belief this check cannot support.
func TestAmbientEnvMessageExplainsItselfAndItsLimits(t *testing.T) {
	tests := []struct {
		name string
		prog string
		want []string
	}{
		{
			name: "git names the variable that redirects it",
			prog: `"git"`,
			want: []string{"os.Environ()", "GIT_DIR", "core.bare=true", "repo.baseEnv",
				allowAmbientEnv, allowNoDir, allowNoEnv, "scope:", groundRules, brief},
		},
		{
			name: "a non-git command names what changes under it",
			prog: `"go"`,
			want: []string{"os.Environ()", "GOFLAGS", "NODE_OPTIONS", "syntax.baseEnv",
				allowAmbientEnv, allowNoDir, allowNoEnv, "scope:", groundRules, brief},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			src := "package p\n\nimport (\n\t\"os\"\n\t\"os/exec\"\n)\n\nfunc f(dir string) {\n" +
				"\tcmd := exec.Command(" + tc.prog + ")\n\tcmd.Dir = dir\n\tcmd.Env = os.Environ()\n" +
				"\t_ = cmd.Run()\n}\n"
			file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}

			violations := subprocessViolations(fset, file)
			if len(violations) != 1 || violations[0].label != "Env value" {
				t.Fatalf("got %d violations %v, want exactly the Env value one",
					len(violations), violations)
			}

			msg := violations[0].msg
			if !strings.Contains(msg, "cmd.Env") {
				t.Errorf("the failure does not name the assignment it is about:\n%s", msg)
			}
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("the failure does not mention %q:\n%s", want, msg)
				}
			}
		})
	}
}
