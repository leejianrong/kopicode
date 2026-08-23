package dispatch

import "fmt"

// Dispatcher runs a batch of orders through a Registry's handlers.
type Dispatcher struct {
	Registry *Registry
}

// Dispatch runs every order through every registered handler, in priority
// order, stopping that order -- and the whole batch -- at the first error.
func (d *Dispatcher) Dispatch(orders []*Order) error {
	handlers := d.Registry.Handlers()
	for _, o := range orders {
		for _, h := range handlers {
			if err := h.Handle(o); err != nil {
				return fmt.Errorf("%s: order %s: %w", h.Name(), o.ID, err)
			}
		}
	}
	return nil
}
