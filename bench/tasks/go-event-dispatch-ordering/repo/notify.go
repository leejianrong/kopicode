package dispatch

import "fmt"

// NotifyHandler builds the shipping notification queued for the customer.
// Like ReportHandler, it has to run after enrichment: the message greets
// the customer by name.
type NotifyHandler struct{}

func (NotifyHandler) Name() string { return "notify" }

func (NotifyHandler) Handle(o *Order) error {
	o.Notification = fmt.Sprintf("Hi %s, your order %s has shipped!", o.CustomerName, o.ID)
	return nil
}
