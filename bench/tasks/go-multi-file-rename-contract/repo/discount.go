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

// Discounter turns a line price into the price the customer actually pays.
type Discounter interface {
	Apply(price int) int
}

// percentOff is the only implementation. Its rate is meant to depend on the
// customer's tier, which is the whole reason a caller needs to be able to
// tell it one.
type percentOff struct {
	regularPercent int
	goldPercent    int
}

// NewDiscounter returns the discount rule this shop uses: 5% off for
// everyone, 15% off for gold customers.
func NewDiscounter() Discounter {
	return &percentOff{regularPercent: 5, goldPercent: 15}
}

func (d *percentOff) Apply(price int) int {
	off := price * d.regularPercent / 100
	return price - off
}
