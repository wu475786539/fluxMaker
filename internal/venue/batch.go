package venue

import (
	"context"
	"errors"
	"fmt"

	"fluxmaker/internal/domain"
)

// BatchPlacer is an optional venue capability. Implementations must preserve
// request ordering in the returned slice and return one result slot per
// request. An empty OrderID represents an item that was not confirmed.
type BatchPlacer interface {
	PlacePostOnlyBatch(ctx context.Context, requests []PlaceRequest) ([]domain.Order, error)
}

// BeforePlace is called immediately before an exchange mutation. The OMS uses
// it for fencing validation. A native batch is one mutation; the fallback
// validates before every individual order.
type BeforePlace func(context.Context) error

// PlacePostOnlyBatch is the common order-placement dispatcher used by the OMS. A venue
// with a safe native Post-Only batch endpoint handles the whole slice. Other
// venues automatically fall back to their single-order endpoint.
func PlacePostOnlyBatch(ctx context.Context, client Client, requests []PlaceRequest, before BeforePlace) ([]domain.Order, error) {
	orders := make([]domain.Order, len(requests))
	if len(requests) == 0 {
		return orders, nil
	}
	if batch, ok := client.(BatchPlacer); ok {
		if before != nil {
			if err := before(ctx); err != nil {
				return orders, err
			}
		}
		result, err := batch.PlacePostOnlyBatch(ctx, requests)
		if len(result) != len(requests) {
			lengthErr := fmt.Errorf("native batch returned %d results for %d requests", len(result), len(requests))
			return result, errors.Join(lengthErr, err)
		}
		return result, err
	}
	for index, request := range requests {
		if before != nil {
			if err := before(ctx); err != nil {
				return orders, err
			}
		}
		order, err := client.PlacePostOnly(ctx, request)
		if err != nil {
			return orders, err
		}
		orders[index] = order
	}
	return orders, nil
}
