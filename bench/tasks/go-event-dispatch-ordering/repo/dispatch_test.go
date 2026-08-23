package dispatch

import "testing"

func TestPipelineProducesReportAndNotification(t *testing.T) {
	p := NewPipeline()
	orders := []*Order{
		{ID: "O-1", CustomerID: "C-100", AmountCents: 4599},
		{ID: "O-2", CustomerID: "C-200", AmountCents: 1250},
	}
	if err := p.Dispatch(orders); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if want := "Order O-1: Amara Okafor, $45.99"; orders[0].ReportLine != want {
		t.Errorf("orders[0].ReportLine = %q, want %q", orders[0].ReportLine, want)
	}
	if want := "Hi Priya Nandakumar, your order O-2 has shipped!"; orders[1].Notification != want {
		t.Errorf("orders[1].Notification = %q, want %q", orders[1].Notification, want)
	}
}

// TestReplayPipelineRegeneratesTheSameText guards the second, independent
// place the pipeline is wired up: support tooling's on-demand replay, built
// by NewReplayPipeline rather than NewPipeline. A fix applied to only one
// of the two builders leaves this failing.
func TestReplayPipelineRegeneratesTheSameText(t *testing.T) {
	p := NewReplayPipeline()
	orders := []*Order{
		{ID: "O-9", CustomerID: "C-300", AmountCents: 999},
	}
	if err := p.Dispatch(orders); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if want := "Order O-9: Ben Rosales, $9.99"; orders[0].ReportLine != want {
		t.Errorf("ReportLine = %q, want %q", orders[0].ReportLine, want)
	}
	if want := "Hi Ben Rosales, your order O-9 has shipped!"; orders[0].Notification != want {
		t.Errorf("Notification = %q, want %q", orders[0].Notification, want)
	}
}

// TestRegistryOrdersByPriorityNotRegistrationOrder exercises the shared
// machinery directly, in yet another registration order than either
// pipeline builder uses: a Registry has to hand handlers back
// lowest-priority-first no matter what order Register was called in.
func TestRegistryOrdersByPriorityNotRegistrationOrder(t *testing.T) {
	r := &Registry{}
	r.Register(NotifyHandler{}, 40)
	r.Register(ValidateHandler{}, 10)
	r.Register(ReportHandler{}, 30)
	r.Register(EnrichHandler{}, 20)

	var names []string
	for _, h := range r.Handlers() {
		names = append(names, h.Name())
	}
	want := []string{"validate", "enrich", "report", "notify"}
	if len(names) != len(want) {
		t.Fatalf("Handlers() returned %d handlers, want %d", len(names), len(want))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("Handlers() order = %v, want %v", names, want)
			break
		}
	}
}

func TestValidateRejectsOrderWithNoCustomer(t *testing.T) {
	p := NewPipeline()
	orders := []*Order{{ID: "O-lonely"}}
	if err := p.Dispatch(orders); err == nil {
		t.Fatal("Dispatch accepted an order with no customer ID")
	}
}

func TestEnrichRejectsUnknownCustomer(t *testing.T) {
	p := NewPipeline()
	orders := []*Order{{ID: "O-ghost", CustomerID: "C-999"}}
	if err := p.Dispatch(orders); err == nil {
		t.Fatal("Dispatch accepted an order for an unknown customer")
	}
}
