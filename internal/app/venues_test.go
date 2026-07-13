package app

import (
	"context"
	"fmt"
	"testing"

	"fluxmaker/internal/config"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/venue"
)

type registryTestClient struct {
	name string
}

func (c *registryTestClient) Name() string { return c.name }
func (c *registryTestClient) TopBook(context.Context, string) (domain.Book, error) {
	return domain.Book{}, nil
}
func (c *registryTestClient) Balances(context.Context) ([]domain.Balance, error) { return nil, nil }
func (c *registryTestClient) OpenOrders(context.Context, string) ([]domain.Order, error) {
	return nil, nil
}
func (c *registryTestClient) PlacePostOnly(context.Context, venue.PlaceRequest) (domain.Order, error) {
	return domain.Order{}, nil
}
func (c *registryTestClient) CancelOrder(context.Context, string, string) error { return nil }

func TestBuildVenuesCreatesClientPerInstrument(t *testing.T) {
	cfg := config.Config{
		Mode: domain.ModeShadow,
		RPC:  config.RPCConfig{RequestTimeoutMS: 1000},
		Venues: map[string]config.VenueConfig{
			"binance": {
				Type:    "binance",
				Enabled: true,
				BaseURL: "https://api.binance.com",
				Markets: map[string]config.VenueMarketConfig{
					"token_a_usdt": {},
					"token_b_usdt": {},
				},
			},
		},
	}
	clients, err := BuildVenues(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected two per-instrument clients, got %d", len(clients))
	}
	if clients[venue.ClientKey("binance", "token_a_usdt")] == nil || clients[venue.ClientKey("binance", "token_b_usdt")] == nil {
		t.Fatal("missing per-instrument venue client")
	}
}

func TestBuildVenuesUsesRegisteredFactoryWithoutCoreTypeSwitch(t *testing.T) {
	var received venue.ClientOptions
	registry, err := venue.NewRegistry(venue.FactoryFunc{VenueType: "custom", New: func(options venue.ClientOptions) (venue.Client, error) {
		received = options
		return &registryTestClient{name: options.Name}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Mode: domain.ModeShadow,
		RPC:  config.RPCConfig{RequestTimeoutMS: 1250},
		Venues: map[string]config.VenueConfig{"custom-main": {
			Type: "custom", Enabled: true, BaseURL: "https://custom.invalid", SelfTradePrevention: "adapter-mode",
			Markets: map[string]config.VenueMarketConfig{"token_usdt": {}},
		}},
	}
	clients, err := BuildVenuesWithRegistry(context.Background(), cfg, nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	if clients[venue.ClientKey("custom-main", "token_usdt")] == nil {
		t.Fatal("custom factory client missing")
	}
	if received.Name != "custom-main/token_usdt" || received.BaseURL != "https://custom.invalid" || received.Timeout.String() != "1.25s" || received.SelfTradePrevention != "adapter-mode" {
		t.Fatalf("unexpected factory options: %+v", received)
	}
}

func TestDefaultRegistryCoversEveryPublishedAdapterSpec(t *testing.T) {
	registry, err := defaultVenueRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range venue.AdapterSpecs() {
		client, err := registry.Build(spec.Type, venue.ClientOptions{Name: spec.Type, StateIdentity: spec.Type, BaseURL: spec.ProductionBaseURL})
		if err != nil {
			t.Fatalf("adapter %s is advertised but not registered: %v", spec.Type, err)
		}
		if client == nil {
			t.Fatalf("adapter %s returned nil client", spec.Type)
		}
	}
}

func TestBuildVenuesIsolatesFactoryFailureByInstrument(t *testing.T) {
	registry, err := venue.NewRegistry(venue.FactoryFunc{VenueType: "custom", New: func(options venue.ClientOptions) (venue.Client, error) {
		if options.Name == "custom-main/bad" {
			return nil, fmt.Errorf("adapter unavailable")
		}
		return &registryTestClient{name: options.Name}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Mode: domain.ModeShadow,
		RPC:  config.RPCConfig{RequestTimeoutMS: 1000},
		Venues: map[string]config.VenueConfig{"custom-main": {
			Type: "custom", Enabled: true, Markets: map[string]config.VenueMarketConfig{"good": {}, "bad": {}},
		}},
	}
	clients, failures, err := buildVenuesIsolatedWithRegistry(context.Background(), cfg, nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	if clients[venue.ClientKey("custom-main", "good")] == nil {
		t.Fatal("healthy instrument client was not built")
	}
	if clients[venue.ClientKey("custom-main", "bad")] != nil {
		t.Fatal("failed instrument unexpectedly has a client")
	}
	if len(failures) != 1 || len(failures["bad"]) != 1 {
		t.Fatalf("factory failure was not isolated: %+v", failures)
	}
}
