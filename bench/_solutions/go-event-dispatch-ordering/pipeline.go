package dispatch

// NewPipeline builds the shop's live order pipeline: validate the order,
// look up the customer, then build the report line and the shipping
// notification. Priorities decide the order handlers actually run in --
// the order they are registered in below does not matter.
func NewPipeline() *Dispatcher {
	r := &Registry{}
	r.Register(ValidateHandler{}, 10)
	r.Register(EnrichHandler{}, 20)
	r.Register(ReportHandler{}, 30)
	r.Register(NotifyHandler{}, 40)
	return &Dispatcher{Registry: r}
}
