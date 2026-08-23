package billing

import "fmt"

// Invoice is the itemized bill generated from a cart, built independently of
// Cart.Total: it needs the discounted price for each line, not just the sum.
type Invoice struct {
	Customer Customer
	Lines    []InvoiceLine
	Total    int
}

// InvoiceLine is one priced, discounted line on the invoice.
type InvoiceLine struct {
	Item            string
	OriginalPrice   int
	DiscountedPrice int
}

// computeLine prices one invoice line under the shop's discount rule.
func computeLine(d Discounter, item Item) InvoiceLine {
	discounted := d.Apply(item.Price)
	return InvoiceLine{
		Item:            item.Name,
		OriginalPrice:   item.Price,
		DiscountedPrice: discounted,
	}
}

// GenerateInvoice builds an itemized invoice for the cart.
func GenerateInvoice(c Cart) Invoice {
	inv := Invoice{Customer: c.Customer}
	for _, item := range c.Items {
		line := computeLine(c.Discount, item)
		inv.Lines = append(inv.Lines, line)
		inv.Total += line.DiscountedPrice
	}
	return inv
}

func (inv Invoice) String() string {
	return fmt.Sprintf("invoice for %s: total %d", inv.Customer.Name, inv.Total)
}
