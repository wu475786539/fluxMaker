package venue

import (
	"context"
	"testing"

	"fluxmaker/internal/domain"
)

type capabilityTestClient struct {
	clientOrderIDs bool
}

func (c capabilityTestClient) Name() string { return "test" }
func (c capabilityTestClient) TopBook(context.Context, string) (domain.Book, error) {
	return domain.Book{}, nil
}
func (c capabilityTestClient) Balances(context.Context) ([]domain.Balance, error) { return nil, nil }
func (c capabilityTestClient) OpenOrders(context.Context, string) ([]domain.Order, error) {
	return nil, nil
}
func (c capabilityTestClient) PlacePostOnly(context.Context, PlaceRequest) (domain.Order, error) {
	return domain.Order{}, nil
}
func (c capabilityTestClient) CancelOrder(context.Context, string, string) error { return nil }
func (c capabilityTestClient) Capabilities() Capabilities {
	return Capabilities{ClientOrderIDs: c.clientOrderIDs}
}

func TestOrderManagementComesFromClientCapabilities(t *testing.T) {
	if ManagesAllOrders(capabilityTestClient{clientOrderIDs: true}) {
		t.Fatal("client-order-id adapter must preserve unmanaged orders")
	}
	if !ManagesAllOrders(capabilityTestClient{clientOrderIDs: false}) {
		t.Fatal("adapter without client order IDs must manage its dedicated account")
	}
}

func TestRegistryRejectsDuplicateAndUnknownFactories(t *testing.T) {
	factory := FactoryFunc{VenueType: "test", New: func(ClientOptions) (Client, error) {
		return capabilityTestClient{clientOrderIDs: true}, nil
	}}
	registry, err := NewRegistry(factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(factory); err == nil {
		t.Fatal("duplicate factory registration succeeded")
	}
	if _, err := registry.Build("unknown", ClientOptions{}); err == nil {
		t.Fatal("unknown venue factory build succeeded")
	}
}
