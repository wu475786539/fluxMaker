package pancakev2

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"sync"
	"testing"
	"time"

	"fluxmaker/internal/config"
	"fluxmaker/internal/num"
)

func TestPancakeV2SpotAndTWAP(t *testing.T) {
	pair := "0x1111111111111111111111111111111111111111"
	token0 := "0x2222222222222222222222222222222222222222"
	token1 := "0x3333333333333333333333333333333333333333"
	r0 := new(big.Int).Mul(big.NewInt(10), pow10(18))
	r1 := new(big.Int).Mul(big.NewInt(20), pow10(6))
	price0 := new(big.Int).Quo(new(big.Int).Lsh(new(big.Int).Set(r1), 112), r0)
	price1 := new(big.Int).Quo(new(big.Int).Lsh(new(big.Int).Set(r0), 112), r1)
	factory := "0x4444444444444444444444444444444444444444"
	state := struct {
		sync.Mutex
		timestamp uint64
	}{timestamp: 1_000}

	handle := func(reqBody []byte) map[string]any {
		var request struct {
			ID     uint64            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(reqBody, &request)
		state.Lock()
		ts := state.timestamp
		state.Unlock()
		var result any
		if request.Method == "eth_getBlockByNumber" {
			result = map[string]string{"number": "0x1", "timestamp": "0x" + new(big.Int).SetUint64(ts).Text(16)}
		} else if request.Method == "eth_chainId" {
			result = "0x38"
		} else {
			var call map[string]string
			_ = json.Unmarshal(request.Params[0], &call)
			switch call["data"] {
			case selectorToken0:
				result = "0x" + hex.EncodeToString(addressWord(token0))
			case selectorToken1:
				result = "0x" + hex.EncodeToString(addressWord(token1))
			case selectorFactory:
				result = "0x" + hex.EncodeToString(addressWord(factory))
			case selectorDecimals:
				if call["to"] == token0 {
					result = "0x" + hex.EncodeToString(intWord(big.NewInt(18)))
				} else {
					result = "0x" + hex.EncodeToString(intWord(big.NewInt(6)))
				}
			case selectorGetReserves:
				result = "0x" + hex.EncodeToString(joinWords(r0, r1, new(big.Int).SetUint64(ts)))
			case selectorPrice0CumulativeLast:
				elapsed := new(big.Int).SetUint64(ts - 1_000)
				result = "0x" + hex.EncodeToString(intWord(new(big.Int).Mul(price0, elapsed)))
			case selectorPrice1CumulativeLast:
				elapsed := new(big.Int).SetUint64(ts - 1_000)
				result = "0x" + hex.EncodeToString(intWord(new(big.Int).Mul(price1, elapsed)))
			}
		}
		return map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}
	}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		trimmed := bytes.TrimSpace(raw)
		var body []byte
		if len(trimmed) > 0 && trimmed[0] == '[' {
			var batch []json.RawMessage
			_ = json.Unmarshal(trimmed, &batch)
			responses := make([]map[string]any, len(batch))
			for i, item := range batch {
				responses[i] = handle(item)
			}
			body, _ = json.Marshal(responses)
		} else {
			body, _ = json.Marshal(handle(trimmed))
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	rpc := NewRPCClient([]string{"http://rpc.invalid"}, time.Second)
	rpc.http.Transport = transport
	info, err := rpc.InspectPair(context.Background(), pair)
	if err != nil {
		t.Fatal(err)
	}
	if info.Token0 != token0 || info.Token1 != token1 || info.Factory != factory {
		t.Fatalf("unexpected pair info: %+v", info)
	}
	oracle := New(rpc)
	in := config.InstrumentConfig{ID: "token_usdt", Reference: config.ReferenceConfig{Type: "pancake_v2", TWAPWindowSeconds: 60, MaxSpotTWAPDeviationBPS: 5, AllowSpotDuringWarmup: true, Legs: []config.PairLegConfig{{PairAddress: pair, ExpectedFactory: factory, BaseToken: token0, QuoteToken: token1}}}}
	first, err := oracle.Price(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.TWAPReady || first.Price.Cmp(num.Must("2")) != 0 {
		t.Fatalf("first=%+v", first)
	}
	state.Lock()
	state.timestamp = 1_060
	state.Unlock()
	rpc.blockMu.Lock()
	rpc.blockFetchedAt = time.Time{}
	rpc.blockMu.Unlock()
	second, err := oracle.Price(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !second.TWAPReady {
		t.Fatal("TWAP should be ready")
	}
	deviation := second.Price.Sub(num.Must("2")).Abs().Div(num.Must("2")).Mul(num.Must("10000"))
	if deviation.Cmp(num.Must("1")) > 0 {
		t.Fatalf("twap=%s deviation=%s", second.Price.String(), deviation.String())
	}
	state.Lock()
	state.timestamp = 1_061
	state.Unlock()
	rpc.blockMu.Lock()
	rpc.blockFetchedAt = time.Time{}
	rpc.blockMu.Unlock()
	third, err := oracle.Price(context.Background(), in)
	if err != nil || !third.TWAPReady || third.Confidence != "high" {
		t.Fatalf("TWAP readiness was not retained inside the next window: third=%+v err=%v", third, err)
	}
}

func intWord(v *big.Int) []byte {
	b := make([]byte, 32)
	raw := v.Bytes()
	copy(b[32-len(raw):], raw)
	return b
}
func addressWord(address string) []byte {
	raw, _ := hex.DecodeString(address[2:])
	b := make([]byte, 32)
	copy(b[12:], raw)
	return b
}
func joinWords(values ...*big.Int) []byte {
	var result []byte
	for _, v := range values {
		result = append(result, intWord(v)...)
	}
	return result
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
