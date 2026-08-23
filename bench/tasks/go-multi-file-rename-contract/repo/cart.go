package billing

// Customer is who is buying. Tier determines the discount every line gets.
type Customer struct {
	Name string
	Tier Tier
}

// Item is one priced line, in cents.
type Item struct {
	Name  string
	Price int
}

// Cart is a customer's shopping cart, mid-checkout.
type Cart struct {
	Customer Customer
	Items    []Item
	Discount Discounter
}

// Total returns what the customer owes after their tier's discount is
// applied to every line.
func (c Cart) Total() int {
	total := 0
	for _, item := range c.Items {
		total += c.Discount.Apply(item.Price)
	}
	return total
}
