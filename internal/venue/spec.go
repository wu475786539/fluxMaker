package venue

import "strings"

// AdapterSpec is non-secret metadata shared by validation and the admin UI.
// It keeps exchange-specific safety requirements out of the engine and avoids
// duplicating venue defaults in JavaScript.
type AdapterSpec struct {
	Type                        string `json:"type"`
	Name                        string `json:"name"`
	ProductionBaseURL           string `json:"production_base_url"`
	TestnetBaseURL              string `json:"testnet_base_url,omitempty"`
	DefaultSelfTradePrevention  string `json:"default_self_trade_prevention,omitempty"`
	RequiresSelfTradePrevention bool   `json:"requires_self_trade_prevention"`
	RequiresDedicatedAccount    bool   `json:"requires_dedicated_account"`
}

var adapterSpecs = []AdapterSpec{
	{
		Type: "binance", Name: "Binance", ProductionBaseURL: "https://api.binance.com", TestnetBaseURL: "https://testnet.binance.vision",
		DefaultSelfTradePrevention: "EXPIRE_BOTH", RequiresSelfTradePrevention: true,
	},
	{
		Type: "mgbx", Name: "MGBX", ProductionBaseURL: "https://open.mgbx.com", RequiresDedicatedAccount: true,
	},
}

func AdapterSpecs() []AdapterSpec {
	return append([]AdapterSpec(nil), adapterSpecs...)
}

func AdapterSpecFor(typeName string) (AdapterSpec, bool) {
	normalized := strings.ToLower(strings.TrimSpace(typeName))
	for _, spec := range adapterSpecs {
		if spec.Type == normalized {
			return spec, true
		}
	}
	return AdapterSpec{}, false
}
