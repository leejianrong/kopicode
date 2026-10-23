package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/leejianrong/kopicode/internal/engine"
)

// protectFlag is the repeatable --protect <path> (ADR-0037): a path inside the
// working tree the session may not change. The REPL and `run --print` take it;
// `serve` and `mcp` take the same list as protected_paths on a session start.
type protectFlag []string

func (p *protectFlag) String() string { return strings.Join(*p, ",") }

func (p *protectFlag) Set(v string) error {
	*p = append(*p, v)
	return nil
}

// bindProtectFlag registers --protect on fs and returns what it collects.
func bindProtectFlag(fs *flag.FlagSet) *protectFlag {
	var p protectFlag
	fs.Var(&p, "protect", "a `path` inside this directory the session may not change (repeatable): writes "+
		"under it are refused and the model is told to stop and say so. Not a sandbox: a build step that writes the tree is not seen")
	return &p
}

// protectedOptions validates the flag's values the way a session start does, so
// a bad one is a usage error before anything is opened.
func protectedOptions(p *protectFlag) ([]string, error) {
	if p == nil || len(*p) == 0 {
		return nil, nil
	}
	if err := engine.ValidateProtectedPaths(*p); err != nil {
		return nil, fmt.Errorf("--protect: %w", err)
	}
	return append([]string(nil), *p...), nil
}
