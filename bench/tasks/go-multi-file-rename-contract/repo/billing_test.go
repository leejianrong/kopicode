package billing

import "testing"

func TestDiscounterRates(t *testing.T) {
	d := NewDiscounter()
	if got := d.Apply(1000, TierRegular); got != 950 {
		t.Errorf("Apply(1000, regular) = %d, want 950", got)
	}
	if got := d.Apply(1000, TierGold); got != 850 {
		t.Errorf("Apply(1000, gold) = %d, want 850", got)
	}
}

func goldCart() Cart {
	return Cart{
		Customer: Customer{Name: "Priya", Tier: TierGold},
		Items: []Item{
			{Name: "Widget", Price: 1000},
			{Name: "Gadget", Price: 2000},
		},
		Discount: NewDiscounter(),
	}
}

func regularCart() Cart {
	return Cart{
		Customer: Customer{Name: "Sam", Tier: TierRegular},
		Items: []Item{
			{Name: "Widget", Price: 1000},
			{Name: "Gadget", Price: 2000},
		},
		Discount: NewDiscounter(),
	}
}

func TestCartTotalRegularTier(t *testing.T) {
	if got, want := regularCart().Total(), 2850; got != want {
		t.Errorf("regular Cart.Total() = %d, want %d", got, want)
	}
}

func TestCartTotalGoldTier(t *testing.T) {
	if got, want := goldCart().Total(), 2550; got != want {
		t.Errorf("gold Cart.Total() = %d, want %d", got, want)
	}
}

func TestInvoiceRegularTier(t *testing.T) {
	inv := GenerateInvoice(regularCart())
	if inv.Total != 2850 {
		t.Errorf("regular invoice total = %d, want 2850", inv.Total)
	}
}

func TestInvoiceGoldTier(t *testing.T) {
	inv := GenerateInvoice(goldCart())
	if inv.Total != 2550 {
		t.Errorf("gold invoice total = %d, want 2550 (15%% off every line, not the regular 5%%)", inv.Total)
	}
	for _, line := range inv.Lines {
		want := line.OriginalPrice * 85 / 100
		if line.DiscountedPrice != want {
			t.Errorf("invoice line %s: discounted price = %d, want %d", line.Item, line.DiscountedPrice, want)
		}
	}
}

func TestReceiptRegularTier(t *testing.T) {
	r := BuildReceipt(regularCart())
	if r.Total != 2850 {
		t.Errorf("regular receipt total = %d, want 2850", r.Total)
	}
}

func TestReceiptGoldTier(t *testing.T) {
	r := BuildReceipt(goldCart())
	if r.Total != 2550 {
		t.Errorf("gold receipt total = %d, want 2550 (15%% off every line, not the regular 5%%)", r.Total)
	}
}
