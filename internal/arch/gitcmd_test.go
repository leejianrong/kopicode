package arch_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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
// The analysis is flow-insensitive and deliberately crude: it asks whether the
// enclosing function assigns Dir and Env to the variable holding the Cmd at
// all, not whether it does so before every use. Ordering is visible in review;
// a missing assignment is not, and the false negative that costs a repository
// is the missing one. It also only recognises a literal "git" program name and
// a Cmd bound to a local variable — a Cmd built by a helper, or passed to one,
// reads as unassigned and needs a waiver. That is the intended bias: a noisy
// guard beats a silent corruption, and a waiver with a reason on it is
// reviewable in a way that an invisible exception is not.
//
// See .claude/skills/agent-ground-rules/SKILL.md for both reproductions, and
// internal/repo for the worked example of the patterns enforced here.

// The waiver comments. Each takes a reason on the same line, and applies to a
// git command on that line or the line directly below it.
const (
	allowNoDir = "//kopicode:allow-git-nodir:"
	allowNoEnv = "//kopicode:allow-git-noenv:"
)

const groundRules = ".claude/skills/agent-ground-rules/SKILL.md"

// gitRule is one half of the promise in CLAUDE.md: every git subprocess names
// its target directory *and* builds its own environment.
type gitRule struct {
	// field is the exec.Cmd field that must be assigned.
	field string
	// directive is the comment that waives this rule.
	directive string
	// why explains what goes wrong without it, in the failure message. A guard
	// whose message does not explain itself gets deleted by whoever hits it at
	// 2am, which is exactly what the message is for.
	why string
	// fix names the assignment that satisfies it.
	fix string
}

var gitRules = []gitRule{
	{
		field:     "Dir",
		directive: allowNoDir,
		why: "a git command with no Dir runs in the calling process's working directory. For a\n" +
			"test binary that is the package directory, which is inside this repository: on\n" +
			"2026-08-11 that left core.bare=true in the real .git/config and a stash whose diff\n" +
			"deleted every tracked file.",
		fix: "assign %s.Dir the repository this command is meant to operate on",
	},
	{
		field:     "Env",
		directive: allowNoEnv,
		why: "Dir is necessary and not sufficient. GIT_DIR overrides the working directory\n" +
			"entirely, so an inherited environment redirects a command that looks perfectly\n" +
			"targeted. GIT_WORK_TREE, GIT_INDEX_FILE and GIT_COMMON_DIR do the same.",
		fix: "assign %s.Env an environment you built rather than inherited — see repo.baseEnv\n" +
			"and the fixtureEnv helper in internal/repo",
	},
}

// gitViolation is one rule broken at one call site.
type gitViolation struct {
	line  int
	field string
	msg   string
}

// gitCall is a call to exec.Command or exec.CommandContext whose program is git.
type gitCall struct {
	// name is the variable the resulting *exec.Cmd was bound to, empty when the
	// call's result is used directly or passed straight to another function.
	name string
	// body is the enclosing function, nil for a package-level call.
	body *ast.BlockStmt
	line int
}

// gitViolations reports every git subprocess in file that fails either rule.
//
// file must have been parsed with parser.ParseComments; the waivers live in the
// comments. parser.ImportsOnly, which the import guard in this package uses, is
// not enough here — the call expressions and the assignments are the subject.
func gitViolations(fset *token.FileSet, file *ast.File) []gitViolation {
	execName, ok := importName(file, "os/exec")
	if !ok {
		return nil
	}

	bodies := funcBodies(file)
	bound := boundGitCmds(execName, file)
	waived := collectWaivers(fset, file)

	var out []gitViolation
	ast.Inspect(file, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || !isGitCommand(execName, call) {
			return true
		}
		line := fset.Position(call.Pos()).Line
		gc := gitCall{name: bound[call], body: enclosingBody(bodies, call), line: line}

		for _, rule := range gitRules {
			if assignsField(gc.body, gc.name, rule.field) {
				continue
			}
			reason, found := waived.lookup(rule.directive, line)
			if found && reason != "" {
				continue
			}
			out = append(out, gitViolation{line: line, field: rule.field, msg: gc.message(rule, found)})
		}
		return true
	})
	return out
}

// message is the failure a developer reads. It names which rule broke, why that
// rule exists, the assignment that satisfies it, and the escape hatch.
func (gc gitCall) message(rule gitRule, waivedWithoutReason bool) string {
	target := gc.name
	if target == "" {
		target = "the command"
	}
	head := fmt.Sprintf("git subprocess does not set %s", rule.field)
	if waivedWithoutReason {
		head = fmt.Sprintf("git subprocess does not set %s, and its %s waiver gives no reason",
			rule.field, rule.directive)
	}
	return fmt.Sprintf("%s\n%s\nfix:   %s\nwaive: %s <reason>  on this line or the line above\nsee:   %s",
		head, rule.why, fmt.Sprintf(rule.fix, target), rule.directive, groundRules)
}

// isGitCommand reports whether call is exec.Command/exec.CommandContext with a
// literal git program name. A program name computed at runtime is invisible to
// this analysis and is why the rules also live in review.
func isGitCommand(execName string, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != execName {
		return false
	}

	var arg int
	switch sel.Sel.Name {
	case "Command":
		arg = 0
	case "CommandContext":
		arg = 1
	default:
		return false
	}
	if len(call.Args) <= arg {
		return false
	}
	lit, ok := call.Args[arg].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	prog, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	base := prog[strings.LastIndexAny(prog, `/\`)+1:]
	return base == "git" || base == "git.exe"
}

// boundGitCmds maps each git call to the local variable its result was assigned
// to. A call that is not bound to a single identifier — used inline, or handed
// straight to a helper — has no entry, so nothing can be found assigning its
// fields and it needs a waiver.
func boundGitCmds(execName string, file *ast.File) map[*ast.CallExpr]string {
	bound := map[*ast.CallExpr]string{}
	record := func(lhs, rhs []ast.Expr) {
		if len(lhs) != 1 || len(rhs) != 1 {
			return
		}
		call, ok := rhs[0].(*ast.CallExpr)
		if !ok || !isGitCommand(execName, call) {
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

// assignsField reports whether the function assigns name.field anywhere.
func assignsField(body *ast.BlockStmt, name, field string) bool {
	if body == nil || name == "" {
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != field {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return true
	})
	return found
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
}

// lookup finds a waiver for the git command on line: on that line itself, or
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
	out := waiverSet{reasons: map[waiverKey]string{}, comments: map[int]bool{}}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			pos := fset.Position(comment.Slash)
			for l := pos.Line; l <= fset.Position(comment.End()).Line; l++ {
				out.comments[l] = true
			}
			text := strings.TrimSpace(comment.Text)
			for _, rule := range gitRules {
				if reason, found := strings.CutPrefix(text, rule.directive); found {
					out.reasons[waiverKey{directive: rule.directive, line: pos.Line}] = strings.TrimSpace(reason)
				}
			}
		}
	}
	return out
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

// TestGitSubprocessesSetDirAndEnv is the guard. Every git subprocess in this
// repository, in test code as much as in product code, names its target
// directory and builds its own environment.
func TestGitSubprocessesSetDirAndEnv(t *testing.T) {
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

		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return parseErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		for _, v := range gitViolations(fset, file) {
			t.Errorf("%s:%d: %s", rel, v.line, v.msg)
		}
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

// TestGitSubprocessGuardCatchesViolations is the guard on the guard. A static
// check that has never been watched fail may be asserting nothing, and the
// tree being green is equally consistent with the analysis matching nothing at
// all.
func TestGitSubprocessGuardCatchesViolations(t *testing.T) {
	const preamble = "package p\n\nimport (\n\t\"context\"\n\t\"os/exec\"\n)\n\nfunc f(ctx context.Context, dir string, env []string) {\n"

	tests := []struct {
		name string
		body string
		want []string // the fields reported missing
	}{
		{
			name: "no Dir and no Env",
			body: "\tcmd := exec.Command(\"git\", \"status\")\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "Dir without Env is the one that bit",
			body: "\tcmd := exec.Command(\"git\", \"init\")\n\tcmd.Dir = dir\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "Env without Dir",
			body: "\tcmd := exec.Command(\"git\", \"init\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "both set is clean",
			body: "\tcmd := exec.Command(\"git\", \"init\")\n\tcmd.Dir = dir\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "CommandContext skips the context argument",
			body: "\tcmd := exec.CommandContext(ctx, \"git\", \"init\")\n\tcmd.Dir = dir\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "an absolute program path is still git",
			body: "\tcmd := exec.Command(\"/usr/bin/git\", \"init\")\n\tcmd.Dir = dir\n\t_ = cmd.Run()\n",
			want: []string{"Env"},
		},
		{
			name: "an unbound Cmd cannot be checked and so fails closed",
			body: "\t_ = exec.Command(\"git\", \"init\").Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "a Cmd handed to a helper escapes and fails closed",
			body: "\tcmd := exec.Command(\"git\", \"init\")\n\tconfigure(cmd)\n\t_ = cmd.Run()\n",
			want: []string{"Dir", "Env"},
		},
		{
			name: "waivers with reasons are honoured",
			body: "\t//kopicode:allow-git-nodir: targets the ambient repository on purpose\n" +
				"\t//kopicode:allow-git-noenv: reads the caller's configuration on purpose\n" +
				"\tcmd := exec.Command(\"git\", \"status\")\n\t_ = cmd.Run()\n",
			want: nil,
		},
		{
			name: "a waiver without a reason does not waive",
			body: "\t//kopicode:allow-git-nodir:\n\tcmd := exec.Command(\"git\", \"status\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "a waiver does not reach a call two lines below it",
			body: "\t//kopicode:allow-git-nodir: too far away to apply\n\n\tcmd := exec.Command(\"git\", \"status\")\n\tcmd.Env = env\n\t_ = cmd.Run()\n",
			want: []string{"Dir"},
		},
		{
			name: "a non-git command is not this guard's business",
			body: "\tcmd := exec.Command(\"gofmt\", \"-l\", \".\")\n\t_ = cmd.Run()\n",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p.go", preamble+tc.body+"}\n", parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}

			var got []string
			for _, v := range gitViolations(fset, file) {
				got = append(got, v.field)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("reported %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGitSubprocessGuardMessageExplainsItself keeps the failure worth reading.
// The message is the whole deliverable of a static guard: it has to survive
// being met by someone who has never read this file.
func TestGitSubprocessGuardMessageExplainsItself(t *testing.T) {
	fset := token.NewFileSet()
	src := "package p\n\nimport \"os/exec\"\n\nfunc f() {\n\tcmd := exec.Command(\"git\", \"status\")\n\t_ = cmd.Run()\n}\n"
	file, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	violations := gitViolations(fset, file)
	if len(violations) != 2 {
		t.Fatalf("got %d violations, want 2", len(violations))
	}
	for _, v := range violations {
		for _, want := range []string{groundRules, "cmd." + v.field, "waive:"} {
			if !strings.Contains(v.msg, want) {
				t.Errorf("the %s failure does not mention %q:\n%s", v.field, want, v.msg)
			}
		}
	}
}
