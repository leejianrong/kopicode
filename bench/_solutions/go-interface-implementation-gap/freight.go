package shipping

// freightCarrier is for heavy packages: a flat rate per kilogram (rounded
// down), with a minimum charge below which it doesn't bother quoting less.
type freightCarrier struct{}

func newFreightCarrier() Carrier { return freightCarrier{} }

func (freightCarrier) Quote(p Package) int {
	base := (p.WeightGrams / 1000) * 250
	if base < 800 {
		base = 800
	}
	return base + base*FuelSurchargePercent/100
}
