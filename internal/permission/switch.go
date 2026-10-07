package permission

import (
	"context"
	"errors"
	"sync"
)

// Switch is a [Policy] that can change its answerer while the session runs
// (ADR-0023): the REPL starts asking a person and may be told to run in auto
// mode, or back.
//
// It holds both policies for the life of the session and consults one. The
// choice is read under a lock at each decision, so a switch takes effect on the
// next request and never half-applies to one in flight. Grants a person gave
// with "always" live in the [Gate], not here, and survive a switch in either
// direction: they were considered answers to exact requests.
type Switch struct {
	ask  Policy
	auto Policy

	mu      sync.Mutex
	useAuto bool
}

// NewSwitch builds a switch over an asking policy and an auto policy. Neither
// may be nil: a switch with one side missing would have to invent an answer.
func NewSwitch(ask, auto Policy, startAuto bool) (*Switch, error) {
	if ask == nil || auto == nil {
		return nil, errors.New("permission: a switch needs both an asking policy and an auto policy")
	}
	return &Switch{ask: ask, auto: auto, useAuto: startAuto}, nil
}

// SetAuto selects the auto policy (true) or the asking one (false).
func (s *Switch) SetAuto(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.useAuto = on
}

// Auto reports whether the auto policy is the one answering.
func (s *Switch) Auto() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.useAuto
}

// Decide forwards to whichever policy is selected now.
func (s *Switch) Decide(ctx context.Context, req Request) (Decision, error) {
	if s.Auto() {
		return s.auto.Decide(ctx, req)
	}
	return s.ask.Decide(ctx, req)
}
