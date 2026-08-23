package shipping

// groundCarrier is the shop's cheapest, slowest option: a flat rate per
// hundred grams (rounded up), with no per-zone fee.
type groundCarrier struct{}

func newGroundCarrier() Carrier { return groundCarrier{} }

func (groundCarrier) Quote(p Package) int {
	units := p.WeightGrams/100 + 1
	base := units * 40
	return base + base*FuelSurchargePercent/100
}
