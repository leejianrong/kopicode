package billing

import "fmt"

// PrintableReceipt is the plain-text slip handed to the customer at
// checkout, rendered independently of the invoice.
type PrintableReceipt struct {
	Lines []string
	Total int
}

// formatLine renders one receipt line under the shop's discount rule,
// returning the printed line and the discounted price it reports.
func formatLine(d Discounter, item Item) (string, int) {
	discounted := d.Apply(item.Price)
	return fmt.Sprintf("%s  %d", item.Name, discounted), discounted
}

// BuildReceipt renders a cart into a printable receipt.
func BuildReceipt(c Cart) PrintableReceipt {
	r := PrintableReceipt{}
	for _, item := range c.Items {
		line, discounted := formatLine(c.Discount, item)
		r.Lines = append(r.Lines, line)
		r.Total += discounted
	}
	return r
}
