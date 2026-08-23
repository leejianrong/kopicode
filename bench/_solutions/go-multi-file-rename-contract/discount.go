// Package billing prices a shopping cart, invoices it, and prints a receipt
// for it. All three paths share one thing: how much of a discount a line
// item gets, which depends on the customer's loyalty tier.
package billing

// Tier is a customer's loyalty tier. Gold customers get a bigger discount
// than everyone else.
type Tier string

const (
	TierRegular Tier = "regular"
	TierGold    Tier = "gold"
)

// Discounter turns a line price into the price the customer actually pays,
// given the customer's tier.
type Discounter interface {
	Apply(price int, tier Tier) int
}

// percentOff is the only implementation. Its rate depends on the tier it is
// given, not on anything it stores itself.
type percentOff struct {
	regularPercent int
	goldPercent    int
}

// NewDiscounter returns the discount rule this shop uses: 5% off for
// everyone, 15% off for gold customers.
func NewDiscounter() Discounter {
	return &percentOff{regularPercent: 5, goldPercent: 15}
}

func (d *percentOff) Apply(price int, tier Tier) int {
	percent := d.regularPercent
	if tier == TierGold {
		percent = d.goldPercent
	}
	off := price * percent / 100
	return price - off
}
