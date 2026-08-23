// Package dispatch runs a customer order through a small pipeline of
// handlers: one validates it, one looks up the customer's display name, and
// two more turn the result into text a human actually reads -- the day's
// order report and the shipping notification queued for the customer.
package dispatch

// Order is one customer order as it moves through the pipeline. Handlers
// read and write it in place.
type Order struct {
	ID          string
	CustomerID  string
	AmountCents int

	// Valid is set once the order has passed validation.
	Valid bool
	// CustomerName is filled in from the customer directory. Nothing
	// downstream should have to look it up itself.
	CustomerName string

	// ReportLine and Notification are what this pipeline exists to
	// produce: the line that goes on the day's order report, and the
	// message queued to tell the customer their order has shipped.
	ReportLine   string
	Notification string
}

// Handler is one step in the pipeline. A handler does not know its own
// place in the running order -- that is assigned when it is registered --
// only what it needs to have already happened to the Order by the time it
// runs.
type Handler interface {
	// Name identifies the handler in error messages.
	Name() string
	// Handle processes the order, mutating it in place.
	Handle(o *Order) error
}
