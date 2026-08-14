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

// subprocessCall is a call to exec.Command or exec.CommandContext, whatever it
// runs.
type subprocessCall struct {
	// name is the variable the resulting *exec.Cmd was bound to, empty when the
	// call's result is used directly or passed straight to another function.
	name string
	// body is the enclosing function, nil for a package-level call.
	body *ast.BlockStmt
	line int
	// isGit is true for a literal git program name. It selects the wording of
	// the failure, never whether the rules apply.
	isGit bool
}

// subprocessCalls returns every exec.Command/exec.CommandContext call in file.
//
// file must have been parsed with parser.ParseComments; the waivers live in the
// comments. parser.ImportsOnly, which the import guard in this package uses, is
// not enough here — the call expressions and the assignments are the subject.
func subprocessCalls(fset *token.FileSet, file *ast.File) []subprocessCall {
	execName, ok := importName(file, "os/exec")
	if !ok {
		return nil
	}

	bodies := funcBodies(file)
	bound := boundCmds(execName, file)

	var calls []subprocessCall
	ast.Inspect(file, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall {
			return true
		}
		prog, isSpawn := spawnedProgram(execName, call)
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

// subprocessViolations reports every subprocess in file that fails any of the
// three rules: Dir assigned, Env assigned, and the value assigned to Env not
// being one the analysis can see is inherited.
func subprocessViolations(fset *token.FileSet, file *ast.File) []violation {
	calls := subprocessCalls(fset, file)
	if len(calls) == 0 {
		return nil
	}
	waived := collectWaivers(fset, file)
	// How this file spells os.Environ. Zero when os is not imported at all, in
	// which case no Environ call can be reached and the value check simply has
	// nothing to say. It never becomes a reason to skip the assignment checks.
	osPkg := resolveOsImport(file)

	var out []violation
	for _, sc := range calls {
		for _, rule := range subprocessRules {
			if assignsField(fset, sc.body, sc.name, rule.field) {
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
		how, ambient := ambientEnvAssignment(fset, osPkg, sc.body, sc.name)
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

// message is the failure a developer reads. It names which rule broke, why that
// rule exists, the assignment that satisfies it, and the escape hatch.
func (sc subprocessCall) message(rule subprocessRule, waivedWithoutReason bool) string {
	target := sc.name
	if target == "" {
		target = "the command"
	}
	subject := "subprocess"
	if sc.isGit {
		subject = "git subprocess"
	}

	head := fmt.Sprintf("%s does not set %s", subject, rule.field)
	if waivedWithoutReason {
		head = fmt.Sprintf("%s does not set %s, and its %s waiver gives no reason",
			subject, rule.field, rule.directive)
	}
	g := rule.forProgram(sc.isGit)
	return fmt.Sprintf("%s\n%s\nfix:   %s\nwaive: %s <reason>  on this line or the line above;\n"+
		"       a waiver with no reason waives nothing, and %s does not waive %s\nsee:   %s and %s",
		head, g.why, fmt.Sprintf(g.fix, target), rule.directive,
		rule.directive, otherDirectives(rule.directive), groundRules, brief)
}

// ambientMessage is the failure for the value check. how is what the analysis
// saw, quoted back, because "your Env is inherited" is unactionable next to
// "your Env is os.Environ()".
func (sc subprocessCall) ambientMessage(how string, waivedWithoutReason bool) string {
	target := sc.name
	if target == "" {
		target = "the command"
	}
	subject := "subprocess"
	if sc.isGit {
		subject = "git subprocess"
	}

	head := fmt.Sprintf("%s assigns Env the inherited environment: %s.Env is %s", subject, target, how)
	if waivedWithoutReason {
		head = fmt.Sprintf("%s assigns Env the inherited environment (%s.Env is %s), and its %s "+
			"waiver gives no reason", subject, target, how, ambientEnvRule.directive)
	}
	g := ambientEnvRule.general
	if sc.isGit {
		g = ambientEnvRule.git
	}
	return fmt.Sprintf("%s\n%s\nfix:   %s\nwaive: %s <reason>  on this line or the line above;\n"+
		"       a waiver with no reason waives nothing, and %s does not waive %s\n"+
		"scope: this check reads only os.Environ(), an append over one, nil, and a local\n"+
		"       variable assigned one of those. It cannot see through a helper, and it does\n"+
		"       not claim to — that is why an environment is built in a named function.\n"+
		"see:   %s and %s",
		head, g.why, fmt.Sprintf(g.fix, target), ambientEnvRule.directive,
		ambientEnvRule.directive, otherDirectives(ambientEnvRule.directive), groundRules, brief)
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
func spawnedProgram(execName string, call *ast.CallExpr) (ast.Expr, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != execName {
		return nil, false
	}

	var arg int
	switch sel.Sel.Name {
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
func boundCmds(execName string, file *ast.File) map[*ast.CallExpr]string {
	bound := map[*ast.CallExpr]string{}
	record := func(lhs, rhs []ast.Expr) {
		if len(lhs) != 1 || len(rhs) != 1 {
			return
		}
		call, ok := rhs[0].(*ast.CallExpr)
		if !ok {
			return
		}
		if _, isSpawn := spawnedProgram(execName, call); !isSpawn {
			return
		}
		if id, ok := lhs[0].(*ast.Ident); ok {
			bound[call] = id.Name
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
	return bound
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

// ambientEnvAssignment reports whether the function assigns name.Env something
// the analysis can see is this process's own environment, and how it saw it.
//
// Any such assignment counts, not the last one, matching the flow-insensitive
// bias of the rest of this file: a guard that has to be right about ordering is
// a guard that is wrong about the interesting cases, and ordering is visible in
// review where a wholesale inherit is not.
func ambientEnvAssignment(fset *token.FileSet, osPkg osImport, body *ast.BlockStmt, name string) (string, bool) {
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

// osImport is how a file spells package os, which decides what an Environ call
// looks like in its AST.
//
// The dot-import case is carried rather than dropped. It is rare and a linter
// would object to it, but "the guard silently stops seeing os.Environ if
// somebody dot-imports os" is exactly the shape of hole this card exists to
// close, and it costs one field to not have.
type osImport struct {
	// name qualifies the call (os.Environ). Empty when os is dot-imported,
	// blank-imported or absent.
	name string
	// dot is true when os is dot-imported, so Environ() is unqualified.
	dot bool
}

func resolveOsImport(file *ast.File) osImport {
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != "os" {
			continue
		}
		switch {
		case spec.Name == nil:
			return osImport{name: "os"}
		case spec.Name.Name == ".":
			return osImport{dot: true}
		case spec.Name.Name == "_":
			return osImport{}
		default:
			return osImport{name: spec.Name.Name}
		}
	}
	return osImport{}
}

// isEnvironCall reports whether call is os.Environ() as this file spells it.
func (o osImport) isEnvironCall(call *ast.CallExpr) (string, bool) {
	if o.dot {
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "Environ" {
			return "Environ() from a dot-imported os", true
		}
	}
	if o.name == "" {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Environ" {
		return "", false
	}
	if pkg, isIdent := sel.X.(*ast.Ident); isIdent && pkg.Name == o.name {
		return o.name + ".Environ()", true
	}
	return "", false
}

// ambientEnvExpr decides the four forms described at the top of this file, and
// refuses to guess at anything else.
//
// seen breaks the cycle a self-referential assignment would otherwise cause
// (`env = append(env, …)`), and bounds the walk.
func ambientEnvExpr(osPkg osImport, body *ast.BlockStmt, expr ast.Expr, seen map[string]bool) (string, bool) {
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

// importName returns the identifier path is bound to in file.
func importName(file *ast.File, path string) (string, bool) {
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != path {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				return "", false
			}
			return spec.Name.Name, true
		}
		return "exec", true
	}
	return "", false
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
