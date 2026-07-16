package app

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/credentials"
	"fluxmaker/internal/domain"
	"fluxmaker/internal/venue"
	"fluxmaker/internal/venue/binance"
	"fluxmaker/internal/venue/mgbx"
)

func BuildVenues(ctx context.Context, cfg config.Config, credentialService *credentials.Service) (map[string]venue.Client, error) {
	registry, err := defaultVenueRegistry()
	if err != nil {
		return nil, err
	}
	return BuildVenuesWithRegistry(ctx, cfg, credentialService, registry)
}

// BuildVenuesIsolated builds every instrument connection independently. A bad
// credential or adapter for one instrument must not prevent unrelated
// instruments from starting; the returned failures are handed to the engine's
// per-instrument preflight gate.
func BuildVenuesIsolated(ctx context.Context, cfg config.Config, credentialService *credentials.Service) (map[string]venue.Client, map[string][]string, error) {
	registry, err := defaultVenueRegistry()
	if err != nil {
		return nil, nil, err
	}
	return buildVenuesIsolatedWithRegistry(ctx, cfg, credentialService, registry)
}

func defaultVenueRegistry() (*venue.Registry, error) {
	return venue.NewRegistry(
		venue.FactoryFunc{VenueType: "binance", New: func(options venue.ClientOptions) (venue.Client, error) {
			return binance.NewWithIdentity(options.Name, options.StateIdentity, options.BaseURL, options.APIKey, options.APISecret, options.SelfTradePrevention, options.Timeout), nil
		}},
		venue.FactoryFunc{VenueType: "mgbx", New: func(options venue.ClientOptions) (venue.Client, error) {
			return mgbx.NewWithIdentity(options.Name, options.StateIdentity, options.BaseURL, options.APIKey, options.APISecret, options.Timeout), nil
		}},
	)
}

func BuildVenuesWithRegistry(ctx context.Context, cfg config.Config, credentialService *credentials.Service, registry *venue.Registry) (map[string]venue.Client, error) {
	clients, failures, err := buildVenuesIsolatedWithRegistry(ctx, cfg, credentialService, registry)
	if err != nil {
		return nil, err
	}
	if len(failures) > 0 {
		ids := make([]string, 0, len(failures))
		for instrumentID := range failures {
			ids = append(ids, instrumentID)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("instrument %s: %s", ids[0], strings.Join(failures[ids[0]], "; "))
	}
	return clients, nil
}

func buildVenuesIsolatedWithRegistry(ctx context.Context, cfg config.Config, credentialService *credentials.Service, registry *venue.Registry) (map[string]venue.Client, map[string][]string, error) {
	if registry == nil {
		return nil, nil, fmt.Errorf("venue registry is unavailable")
	}
	clients := make(map[string]venue.Client)
	failures := make(map[string][]string)
	// Venue REST calls (order placement/cancel) are slower and more critical than
	// oracle RPC reads, so floor them at 8s even when request_timeout_ms is set low
	// for the oracle. Order POSTs can't be safely retried, so give them room.
	timeout := cfg.RequestTimeout()
	if timeout < 8*time.Second {
		timeout = 8 * time.Second
	}
	for name, venueCfg := range cfg.Venues {
		if !venueCfg.Enabled {
			continue
		}
		for instrumentID, market := range venueCfg.Markets {
			apiKey, secret := "", ""
			if market.CredentialID > 0 {
				if credentialService == nil {
					failures[instrumentID] = append(failures[instrumentID], fmt.Sprintf("venue %s: credential service unavailable", name))
					continue
				}
				resolved, err := credentialService.Resolve(ctx, market.CredentialID, venueCfg.Type)
				if err != nil {
					failures[instrumentID] = append(failures[instrumentID], fmt.Sprintf("venue %s credential: %v", name, err))
					continue
				}
				apiKey, secret = resolved.APIKey, resolved.APISecret
			} else if cfg.Mode == domain.ModeLive && venueCfg.TradingEnabled {
				failures[instrumentID] = append(failures[instrumentID], fmt.Sprintf("venue %s: credential is required for live trading", name))
				continue
			}
			clientName := name + "/" + instrumentID
			stateIdentity := fmt.Sprintf("%s/%s/credential-%d", strings.ToLower(name), strings.ToLower(instrumentID), market.CredentialID)
			client, err := registry.Build(venueCfg.Type, venue.ClientOptions{
				Name:                clientName,
				StateIdentity:       stateIdentity,
				BaseURL:             venueCfg.BaseURL,
				APIKey:              apiKey,
				APISecret:           secret,
				SelfTradePrevention: venueCfg.SelfTradePrevention,
				Timeout:             timeout,
			})
			if err != nil {
				failures[instrumentID] = append(failures[instrumentID], fmt.Sprintf("venue %s client: %v", name, err))
				continue
			}
			clients[venue.ClientKey(name, instrumentID)] = client
		}
	}
	return clients, failures, nil
}
