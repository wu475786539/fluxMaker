package venue

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
)

// ClientKey identifies a client/account connection for one venue market.
// Credentials may differ between instruments on the same exchange.
func ClientKey(venueName, instrumentID string) string {
	return strings.ToLower(strings.TrimSpace(venueName)) + "|" + strings.ToLower(strings.TrimSpace(instrumentID))
}

type PlaceRequest struct {
	Symbol          string
	Side            domain.Side
	Price           num.Decimal
	Quantity        num.Decimal
	ClientID        string
	FenceGeneration uint64
}

type Client interface {
	Name() string
	TopBook(ctx context.Context, symbol string) (domain.Book, error)
	Balances(ctx context.Context) ([]domain.Balance, error)
	OpenOrders(ctx context.Context, symbol string) ([]domain.Order, error)
	PlacePostOnly(ctx context.Context, request PlaceRequest) (domain.Order, error)
	CancelOrder(ctx context.Context, symbol, orderID string) error
}

// Capabilities describes adapter behavior that changes how the OMS must treat
// an account. Execution code reads this declaration instead of branching on a
// concrete exchange name, so a new adapter does not require engine changes.
type Capabilities struct {
	ClientOrderIDs    bool
	NativeBatchPlace  bool
	NativeBatchCancel bool
	OrderLookup       bool
	RecentFills       bool
	MarketRules       bool
}

type CapabilityProvider interface {
	Capabilities() Capabilities
}

// CapabilitiesOf returns the adapter declaration and also derives optional
// interface capabilities. Derivation prevents a declaration from hiding an
// implemented batch/reader interface.
func CapabilitiesOf(client Client) Capabilities {
	// Preserve unmanaged account orders unless an adapter explicitly declares
	// that client order IDs are unavailable and the account is dedicated.
	capabilities := Capabilities{ClientOrderIDs: true}
	if provider, ok := client.(CapabilityProvider); ok {
		capabilities = provider.Capabilities()
	}
	_, capabilities.NativeBatchPlace = client.(BatchPlacer)
	_, capabilities.NativeBatchCancel = client.(BatchCanceler)
	_, capabilities.OrderLookup = client.(OrderReader)
	_, capabilities.RecentFills = client.(FillReader)
	_, capabilities.MarketRules = client.(RuleReader)
	return capabilities
}

// ManagesAllOrders is required only for adapters that cannot submit and read
// back client order IDs. Such adapters must be bound to a dedicated account.
func ManagesAllOrders(client Client) bool {
	return !CapabilitiesOf(client).ClientOrderIDs
}

type ClientOptions struct {
	Name                string
	StateIdentity       string
	BaseURL             string
	APIKey              string
	APISecret           string
	SelfTradePrevention string
	Timeout             time.Duration
}

type Factory interface {
	Type() string
	Build(options ClientOptions) (Client, error)
}

type FactoryFunc struct {
	VenueType string
	New       func(ClientOptions) (Client, error)
}

func (f FactoryFunc) Type() string { return strings.ToLower(strings.TrimSpace(f.VenueType)) }

func (f FactoryFunc) Build(options ClientOptions) (Client, error) {
	if f.New == nil {
		return nil, fmt.Errorf("venue factory %q has no constructor", f.Type())
	}
	return f.New(options)
}

// Registry is immutable after application startup in normal use. Tests can
// provide a small custom registry to prove that core construction is not tied
// to Binance or MGBX.
type Registry struct {
	factories map[string]Factory
}

func NewRegistry(factories ...Factory) (*Registry, error) {
	registry := &Registry{factories: make(map[string]Factory, len(factories))}
	for _, factory := range factories {
		if err := registry.Register(factory); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (r *Registry) Register(factory Factory) error {
	if r == nil {
		return fmt.Errorf("venue registry is nil")
	}
	if factory == nil {
		return fmt.Errorf("venue factory is nil")
	}
	typeName := strings.ToLower(strings.TrimSpace(factory.Type()))
	if typeName == "" {
		return fmt.Errorf("venue factory type is required")
	}
	if r.factories == nil {
		r.factories = make(map[string]Factory)
	}
	if _, exists := r.factories[typeName]; exists {
		return fmt.Errorf("venue factory %q is already registered", typeName)
	}
	r.factories[typeName] = factory
	return nil
}

func (r *Registry) Build(typeName string, options ClientOptions) (Client, error) {
	if r == nil {
		return nil, fmt.Errorf("venue registry is unavailable")
	}
	normalized := strings.ToLower(strings.TrimSpace(typeName))
	factory, ok := r.factories[normalized]
	if !ok {
		return nil, fmt.Errorf("unsupported venue type %q", typeName)
	}
	client, err := factory.Build(options)
	if err != nil {
		return nil, fmt.Errorf("build venue %s: %w", normalized, err)
	}
	if client == nil {
		return nil, fmt.Errorf("venue factory %q returned a nil client", normalized)
	}
	return client, nil
}

type FillReader interface {
	RecentFills(ctx context.Context, symbol string, limit int) ([]domain.Fill, error)
}

// OrderReader resolves an order that may not yet be visible in the open-order
// list. It is used to reconcile asynchronous order creation safely.
type OrderReader interface {
	Order(ctx context.Context, symbol, orderID string) (domain.Order, error)
}

// BatchCanceler lets venue adapters use their native bulk cancellation API.
// Implementations must split requests according to the venue's batch limit.
type BatchCanceler interface {
	CancelOrders(ctx context.Context, symbol string, orderIDs []string) error
}

type StateIdentity interface {
	StateIdentity() string
}

type RuleReader interface {
	MarketRules(ctx context.Context, symbol string) (domain.MarketRules, error)
}
