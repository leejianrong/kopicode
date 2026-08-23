package shipping

import "testing"

func TestGroundQuoteIncludesSurcharge(t *testing.T) {
	c, err := NewCarrier("ground")
	if err != nil {
		t.Fatalf("NewCarrier(ground): %v", err)
	}
	got := c.Quote(Package{WeightGrams: 250, Zone: "domestic"})
	if want := 124; got != want {
		t.Errorf("ground Quote = %d, want %d (base 120 plus the %d%% fuel surcharge)",
			got, want, FuelSurchargePercent)
	}
}

func TestAirQuoteIncludesSurchargeDomestic(t *testing.T) {
	c, err := NewCarrier("air")
	if err != nil {
		t.Fatalf("NewCarrier(air): %v", err)
	}
	got := c.Quote(Package{WeightGrams: 250, Zone: "domestic"})
	if want := 374; got != want {
		t.Errorf("air domestic Quote = %d, want %d (base 360 plus the %d%% fuel surcharge)",
			got, want, FuelSurchargePercent)
	}
}

func TestAirQuoteIncludesSurchargeInternational(t *testing.T) {
	c, err := NewCarrier("air")
	if err != nil {
		t.Fatalf("NewCarrier(air): %v", err)
	}
	got := c.Quote(Package{WeightGrams: 250, Zone: "international"})
	if want := 894; got != want {
		t.Errorf("air international Quote = %d, want %d (base 860 plus the %d%% fuel surcharge)",
			got, want, FuelSurchargePercent)
	}
}

func TestFreightQuoteIncludesSurchargeAboveMinimum(t *testing.T) {
	c, err := NewCarrier("freight")
	if err != nil {
		t.Fatalf("NewCarrier(freight): %v", err)
	}
	got := c.Quote(Package{WeightGrams: 5000, Zone: "domestic"})
	if want := 1300; got != want {
		t.Errorf("freight Quote = %d, want %d (base 1250 plus the %d%% fuel surcharge)",
			got, want, FuelSurchargePercent)
	}
}

func TestFreightQuoteIncludesSurchargeAtMinimum(t *testing.T) {
	c, err := NewCarrier("freight")
	if err != nil {
		t.Fatalf("NewCarrier(freight): %v", err)
	}
	got := c.Quote(Package{WeightGrams: 200, Zone: "domestic"})
	if want := 832; got != want {
		t.Errorf("freight Quote below the minimum = %d, want %d (the $8.00 minimum plus the %d%% fuel surcharge)",
			got, want, FuelSurchargePercent)
	}
}

func TestNewCarrierUnknownKey(t *testing.T) {
	if _, err := NewCarrier("drone"); err == nil {
		t.Fatal("NewCarrier(drone) succeeded for a carrier that was never registered")
	}
}
