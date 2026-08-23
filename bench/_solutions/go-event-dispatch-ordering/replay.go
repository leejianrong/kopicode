package dispatch

// NewReplayPipeline builds the pipeline support tooling uses to
// regenerate an order's report line and notification text on request --
// the same four handlers as the live pipeline, wired up separately because
// replay runs on demand for one order at a time rather than as part of the
// normal batch.
func NewReplayPipeline() *Dispatcher {
	r := &Registry{}
	r.Register(ValidateHandler{}, 10)
	r.Register(EnrichHandler{}, 20)
	r.Register(ReportHandler{}, 30)
	r.Register(NotifyHandler{}, 40)
	return &Dispatcher{Registry: r}
}
