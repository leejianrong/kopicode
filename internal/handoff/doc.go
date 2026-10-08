// Package handoff builds the document a session leaves for its successor
// (ADR-0026): what a fresh context needs in order to carry on.
//
// A handoff has two halves, and the split is the design. The model writes the
// narrative (goal, what is done and what is not, decisions, open problems, the
// next step, how to verify), because only it knows what it was trying to do.
// kopicode writes the facts (files written and deleted, the last verification,
// denied calls, usage) from the journal, because a model that forgets a file
// must not lose it. The facts are derived and never asked for.
//
// This package is a leaf over internal/journal. It calls no model and touches no
// disk: the engine makes the one model call and writes the projection file, and
// everything here is a pure function of its inputs, so a handoff built from a
// replayed journal is the same bytes.
package handoff
