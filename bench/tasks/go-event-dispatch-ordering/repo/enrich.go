package dispatch

import "fmt"

// customerDirectory is the shop's small, fixed customer list. A real shop
// would call out to a customer service; this pipeline's tests don't need
// that, so it's a lookup table instead.
var customerDirectory = map[string]string{
	"C-100": "Amara Okafor",
	"C-200": "Priya Nandakumar",
	"C-300": "Ben Rosales",
}

// EnrichHandler looks up the customer's display name. It has to run
// before anything that puts text in front of a human: it is the only
// handler that knows the name.
type EnrichHandler struct{}

func (EnrichHandler) Name() string { return "enrich" }

func (EnrichHandler) Handle(o *Order) error {
	name, ok := customerDirectory[o.CustomerID]
	if !ok {
		return fmt.Errorf("unknown customer %q", o.CustomerID)
	}
	o.CustomerName = name
	return nil
}
