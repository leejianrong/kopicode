// Package shipping quotes the cost of shipping a package by carrier. Every
// carrier's Quote is supposed to add the shop's current fuel surcharge on top
// of its own base rate -- the shop pays that surcharge to its carriers
// either way, so a quote that leaves it off is a quote the shop eats the
// loss on.
package shipping

// Package is what is being shipped.
type Package struct {
	WeightGrams int
	Zone        string // "domestic" or "international"
}

// FuelSurchargePercent is the percentage added on top of every carrier's
// base rate to cover the shop's current fuel cost. It is shop-wide, not
// specific to any one carrier.
const FuelSurchargePercent = 4

// Carrier quotes the cost, in cents, of shipping a package.
type Carrier interface {
	Quote(p Package) int
}
