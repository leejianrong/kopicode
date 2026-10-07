package permission_test

import (
	"context"
	"strings"
	"testing"

	"github.com/leejianrong/kopicode/internal/permission"
)

// TestAutoBlunders is ADR-0023's additions to the never-allow list. Each deny
// row has an allow neighbour, because a list that refuses the whole tool
// passes every deny row; and each deny reason must name an alternative, which
// is what keeps the model from retrying the same mistake in a new spelling.
func TestAutoBlunders(t *testing.T) {
	d := newDirs(t)
	p, err := permission.NewAuto(d.root, fsResolver{}, nil)
	if err != nil {
		t.Fatalf("NewAuto: %v", err)
	}
	tests := []struct {
		name, line, instead string // instead non-empty means deny, and the reason must contain it
	}{
		{"bare pip install", "pip install -r requirements.txt", "uv add"},
		{"pip3 install", "pip3 install requests", "uv add"},
		{"python -m pip install", "python3 -m pip install requests", "uv add"},
		{"pip install --user in a venv path", ".venv/bin/pip install --user x", "project environment"},
		{"pip install after a pipe", "ls | pip install x", "uv add"},
		{"venv pip", ".venv/bin/pip install -r requirements.txt", ""},
		{"venv python -m pip", "./venv/bin/python -m pip install requests", ""},
		{"activated venv", "source .venv/bin/activate && pip install requests", ""},
		{"uv add", "uv add requests", ""},
		{"uv pip", "uv pip install requests", ""},
		{"pip list", "pip list", ""},
		{"pip download is not install", "pip download x", ""},

		{"npm install -g", "npm install -g typescript", "without -g"},
		{"npm i -g", "npm i -g typescript", "without -g"},
		{"npm --global", "npm install --global typescript", "without -g"},
		{"yarn global add", "yarn global add typescript", "without -g"},
		{"pnpm add -g", "pnpm add -g typescript", "without -g"},
		{"npm install local", "npm install typescript", ""},
		{"npm run with g in a name", "npm run build -- --grep x", ""},

		{"git reset --hard", "git reset --hard HEAD~1", "git stash"},
		{"git -C reset --hard", "git -C sub reset --hard", "git stash"},
		{"git reset --soft", "git reset --soft HEAD~1", ""},
		{"git reset a path", "git reset HEAD file.go", ""},
		{"git clean -fd", "git clean -fd", "git clean -n"},
		{"git clean --force", "git clean --force", "git clean -n"},
		{"git clean dry run", "git clean -n", ""},
		{"git checkout dot", "git checkout -- .", "stash"},
		{"git checkout dot bare", "git checkout .", "stash"},
		{"git restore dot", "git restore .", "stash"},
		{"git checkout a branch", "git checkout feature", ""},
		{"git checkout one file", "git checkout -- main.go", ""},
		{"git restore staged", "git restore --staged main.go", ""},

		{"commit no-verify", "git commit --no-verify -m x", "hook"},
		{"commit -n", "git commit -n -m x", "hook"},
		{"commit -an", "git commit -an -m x", "hook"},
		{"push no-verify", "git push --no-verify origin x", "hook"},
		{"push dry run", "git push -n origin x", ""},
		{"commit with n in message", "git commit -m 'run -n later'", ""},
		{"commit amend", "git commit --amend --no-edit", ""},

		{"chmod -R 777", "chmod -R 777 .", "specific"},
		{"chmod -R a+rwx", "chmod -R a+rwx build", "specific"},
		{"chmod -R outside root", "chmod -R 755 " + d.outside, "outside the session root"},
		{"chown -R outside root", "chown -R me " + d.outside, "outside the session root"},
		{"chown -R computed", "chown -R me $DIR", "run time"},
		{"chmod +x", "chmod +x script.sh", ""},
		{"chmod -R inside root", "chmod -R 755 build", ""},
		{"chmod 777 one file", "chmod 777 file", ""},

		// Found by the v0.4.0 QA pass: other spellings of the same mistakes.
		{"npm -g before the subcommand", "npm -g install typescript", "without -g"},
		{"npm --global before the subcommand", "npm --global install typescript", "without -g"},
		{"pnpm -g add", "pnpm -g add x", "without -g"},
		{"yarn --global add", "yarn --global add x", "without -g"},
		{"npm --global=true", "npm i --global=true x", "without -g"},
		{"npm -gD cluster", "npm i -gD x", "without -g"},
		{"npm --location global", "npm --location global install x", "without -g"},
		{"npx wrapped npm -g", "npx -y npm i -g x", "without -g"},
		{"corepack yarn global", "corepack yarn global add x", "without -g"},
		{"npm test with a grep -g", "npm test -- -g pattern", ""},
		{"npm run with -g after the script", "npm run lint -g", ""},
		{"system pip by absolute path", "/usr/bin/pip install x", "uv add"},
		{"system python -m pip by path", "/usr/bin/python3 -m pip install x", "uv add"},
		{"venv by absolute path inside the root", d.root + "/.venv/bin/pip install x", ""},
		{"python -Im pip", "python -Im pip install x", "uv add"},
		{"computed venv path", "$VIRTUAL_ENV/bin/pip install x", "computed at run time"},
		{"git reset --har abbreviation", "git reset --har HEAD~1", "git stash"},
		{"git commit --no-verif abbreviation", "git commit --no-verif -m x", "hook"},
		{"git clean --forc abbreviation", "git clean --forc", "git clean -n"},
		{"git checkout ./", "git checkout ./", "stash"},
		{"git restore ./", "git restore ./", "stash"},
		{"git checkout -- ./", "git checkout -- ./", "stash"},
		{"git checkout -f other", "git checkout -f other", "stash"},
		{"git switch --discard-changes", "git switch --discard-changes main", "stash"},
		{"git checkout -- computed", "git checkout -- $(pwd)", "stash"},
		{"git switch a branch", "git switch main", ""},
		{"git commit -uno is not -n", "git commit -uno -m x", ""},
		{"git commit message of -n", "git commit -m '-n'", ""},
		{"git commit -m then --no-verify", "git commit -m msg --no-verify", "hook"},
		{"chmod -R o+w", "chmod -R o+w .", "specific"},
		{"chmod -R 666", "chmod -R 666 .", "specific"},
		{"chmod -R 1777", "chmod -R 1777 .", "specific"},
		{"chmod -R a=rwx", "chmod -R a=rwx .", "specific"},
		{"chmod -R ugo=rwx", "chmod -R ugo=rwx .", "specific"},
		{"chmod -R u+w is fine", "chmod -R u+w build", ""},
		{"chmod -R go-w is fine", "chmod -R go-w build", ""},
		{"chmod -R 644 is fine", "chmod -R 644 build", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dec, err := p.Decide(context.Background(), shellReq(d.root, tc.line))
			if err != nil {
				t.Fatalf("Decide(%q): %v", tc.line, err)
			}
			if tc.instead == "" {
				if dec.Verdict != permission.VerdictAllow {
					t.Fatalf("Decide(%q) = %s (%s), want allow", tc.line, dec.Verdict, dec.Reason)
				}
				return
			}
			if dec.Verdict != permission.VerdictDeny {
				t.Fatalf("Decide(%q) = %s, want deny", tc.line, dec.Verdict)
			}
			if !strings.Contains(dec.Reason, tc.instead) {
				t.Errorf("reason %q does not tell the model what to do instead (%q)", dec.Reason, tc.instead)
			}
		})
	}
}

func TestSwitchChangesTheAnswererWithoutRestart(t *testing.T) {
	d := newDirs(t)
	auto, _ := permission.NewAuto(d.root, fsResolver{}, nil)
	ask, err := permission.NewAsk(denyingAsker{}, permission.SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	sw, err := permission.NewSwitch(ask, auto, false)
	if err != nil {
		t.Fatal(err)
	}
	req := shellReq(d.root, "go test ./...")
	dec, _ := sw.Decide(context.Background(), req)
	if dec.Verdict != permission.VerdictDeny || dec.Source != permission.SourceUser {
		t.Fatalf("asking mode = %s from %s, want the person's deny", dec.Verdict, dec.Source)
	}
	sw.SetAuto(true)
	dec, _ = sw.Decide(context.Background(), req)
	if dec.Verdict != permission.VerdictAllow || dec.Source != permission.SourceAuto {
		t.Fatalf("auto mode = %s from %s, want auto's allow", dec.Verdict, dec.Source)
	}
	sw.SetAuto(false)
	if dec, _ = sw.Decide(context.Background(), req); dec.Source != permission.SourceUser {
		t.Fatalf("back to asking: source = %s", dec.Source)
	}
	if _, err := permission.NewSwitch(nil, auto, false); err == nil {
		t.Error("a switch with no asking side must be refused")
	}
}

type denyingAsker struct{}

func (denyingAsker) Ask(context.Context, permission.Request) (permission.Reply, error) {
	return permission.Reply{Verdict: permission.VerdictDeny}, nil
}
