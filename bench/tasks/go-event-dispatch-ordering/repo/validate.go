package dispatch

import "errors"

// ValidateHandler checks that an order carries what every later handler
// needs. It has to run before anything else: nothing downstream re-checks
// that an order is well-formed.
type ValidateHandler struct{}

func (ValidateHandler) Name() string { return "validate" }

func (ValidateHandler) Handle(o *Order) error {
	if o.ID == "" {
		return errors.New("order has no ID")
	}
	if o.CustomerID == "" {
		return errors.New("order has no customer ID")
	}
	o.Valid = true
	return nil
}
