package dispatch

import "fmt"

// ReportHandler builds the one-line entry that goes on the day's order
// report. It has to run after enrichment: the line names the customer, and
// EnrichHandler is what fills that in.
type ReportHandler struct{}

func (ReportHandler) Name() string { return "report" }

func (ReportHandler) Handle(o *Order) error {
	o.ReportLine = fmt.Sprintf("Order %s: %s, $%.2f", o.ID, o.CustomerName, float64(o.AmountCents)/100)
	return nil
}
