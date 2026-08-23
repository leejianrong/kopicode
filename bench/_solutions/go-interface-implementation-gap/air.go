package shipping

// airCarrier is the fastest option: a flat rate per hundred grams (rounded
// up), plus a flat handling fee when the package is leaving the country.
type airCarrier struct{}

func newAirCarrier() Carrier { return airCarrier{} }

func (airCarrier) Quote(p Package) int {
	units := p.WeightGrams/100 + 1
	base := units * 120
	if p.Zone == "international" {
		base += 500
	}
	return base + base*FuelSurchargePercent/100
}
