package shipping

import "fmt"

// carrierRegistry maps the key a shipping method selection resolves to, to
// the carrier that handles it. NewCarrier is the only supported way to get a
// Carrier -- constructing one of the concrete types directly bypasses
// whichever key a caller meant to ask for.
var carrierRegistry = map[string]func() Carrier{
	"ground":  newGroundCarrier,
	"air":     newAirCarrier,
	"freight": newFreightCarrier,
}

// NewCarrier returns the carrier registered under key.
func NewCarrier(key string) (Carrier, error) {
	factory, ok := carrierRegistry[key]
	if !ok {
		return nil, fmt.Errorf("shipping: no carrier registered for %q", key)
	}
	return factory(), nil
}
