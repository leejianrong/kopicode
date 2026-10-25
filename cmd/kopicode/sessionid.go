package main

import (
	"flag"
	"fmt"

	"github.com/leejianrong/kopicode/internal/engine"
)

// bindSessionIDFlag registers --session-id on fs (KOP-227): the id of the new
// session, so a caller that launches kopicode (cuttlefish in tmux) knows which
// journal is its own without guessing from timestamps.
func bindSessionIDFlag(fs *flag.FlagSet) *string {
	return fs.String("session-id", "", "name the new session `id` (letters, digits, . _ -), so a caller knows "+
		"which record under .kopicode/sessions is its own; refused if that session already exists. Not with --resume or --fork")
}

// sessionIDOption validates --session-id against dir, with the flags it cannot
// be combined with, and returns the id to put on engine.Options ("" when unset).
func sessionIDOption(id string, dir string, resume, fork string) (string, error) {
	if id == "" {
		return "", nil
	}
	if resume != "" || fork != "" {
		return "", fmt.Errorf("--session-id names a new session; it cannot be combined with --resume or --fork")
	}
	if err := engine.ValidateNewSessionID(dir, id); err != nil {
		return "", fmt.Errorf("--session-id: %w", err)
	}
	return id, nil
}
