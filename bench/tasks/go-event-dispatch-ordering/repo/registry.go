package dispatch

import "sort"

// entry pairs a handler with the priority it was registered under.
type entry struct {
	handler  Handler
	priority int
}

// Registry holds every handler wired into a pipeline, in whatever order
// they were registered, and hands them back in the order the dispatcher
// should actually run them.
type Registry struct {
	entries []entry
}

// Register adds a handler at the given priority. Lower priorities run
// first. Handlers can be registered in any order -- Handlers() is what is
// responsible for putting them in priority order before anything runs.
func (r *Registry) Register(h Handler, priority int) {
	r.entries = append(r.entries, entry{handler: h, priority: priority})
}

// Handlers returns every registered handler in the order the dispatcher
// should run them: the lowest priority first. Ties keep registration
// order, so two handlers registered at the same priority run in the order
// they were added.
func (r *Registry) Handlers() []Handler {
	sorted := make([]entry, len(r.entries))
	copy(sorted, r.entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].priority < sorted[j].priority
	})

	handlers := make([]Handler, len(sorted))
	for i, e := range sorted {
		handlers[i] = e.handler
	}
	return handlers
}
