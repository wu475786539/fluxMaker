package pancakev2

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RPCClient struct {
	urls           []string
	http           *http.Client
	nextID         atomic.Uint64
	blockMu        sync.Mutex
	latestBlock    Block
	blockFetchedAt time.Time
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type Block struct {
	Number    uint64
	Timestamp uint64
	Tag       string
}

type PairInfo struct {
	PairAddress string `json:"pair_address"`
	Token0      string `json:"token0"`
	Token1      string `json:"token1"`
	Factory     string `json:"factory"`
}

func NewRPCClient(urls []string, timeout time.Duration) *RPCClient {
	return &RPCClient{urls: append([]string(nil), urls...), http: &http.Client{Timeout: timeout}}
}

func (c *RPCClient) request(ctx context.Context, method string, params any, out any) error {
	var lastErr error
	for _, endpoint := range c.urls {
		id := c.nextID.Add(1)
		payload, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode/100 != 2 {
			lastErr = fmt.Errorf("rpc http %d: %s", resp.StatusCode, string(body))
			continue
		}
		var decoded rpcResponse
		if err := json.Unmarshal(body, &decoded); err != nil {
			lastErr = err
			continue
		}
		if decoded.Error != nil {
			lastErr = fmt.Errorf("rpc error %d: %s", decoded.Error.Code, decoded.Error.Message)
			continue
		}
		if err := json.Unmarshal(decoded.Result, out); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no rpc endpoints configured")
	}
	return lastErr
}

func (c *RPCClient) LatestBlock(ctx context.Context) (Block, error) {
	c.blockMu.Lock()
	defer c.blockMu.Unlock()
	if !c.blockFetchedAt.IsZero() && time.Since(c.blockFetchedAt) < 500*time.Millisecond {
		return c.latestBlock, nil
	}
	var raw struct {
		Number    string `json:"number"`
		Timestamp string `json:"timestamp"`
	}
	if err := c.request(ctx, "eth_getBlockByNumber", []any{"latest", false}, &raw); err != nil {
		return Block{}, err
	}
	number, err := parseHexUint64(raw.Number)
	if err != nil {
		return Block{}, fmt.Errorf("decode block number: %w", err)
	}
	ts, err := parseHexUint64(raw.Timestamp)
	if err != nil {
		return Block{}, fmt.Errorf("decode block timestamp: %w", err)
	}
	block := Block{Number: number, Timestamp: ts, Tag: "0x" + strconv.FormatUint(number, 16)}
	c.latestBlock = block
	c.blockFetchedAt = time.Now().UTC()
	return block, nil
}

func (c *RPCClient) ChainID(ctx context.Context) (uint64, error) {
	var raw string
	if err := c.request(ctx, "eth_chainId", []any{}, &raw); err != nil {
		return 0, err
	}
	return parseHexUint64(raw)
}

func (c *RPCClient) CallAt(ctx context.Context, to, data, blockTag string) ([]byte, error) {
	var result string
	params := []any{map[string]string{"to": to, "data": data}, blockTag}
	if err := c.request(ctx, "eth_call", params, &result); err != nil {
		return nil, err
	}
	return decodeHexResult(result)
}

func decodeHexResult(result string) ([]byte, error) {
	result = strings.TrimPrefix(result, "0x")
	if len(result)%2 != 0 {
		result = "0" + result
	}
	b, err := hex.DecodeString(result)
	if err != nil {
		return nil, fmt.Errorf("decode eth_call result: %w", err)
	}
	return b, nil
}

// BatchCall issues several eth_call requests in a single JSON-RPC batch (one
// HTTP round-trip) at the same block tag and returns decoded results in the same
// order as calls. Collapsing a pair's getReserves/price0/price1 reads from three
// sequential round-trips into one removes most of the oracle's per-cycle latency.
type BatchCall struct {
	To   string
	Data string
}

func (c *RPCClient) BatchCall(ctx context.Context, calls []BatchCall, blockTag string) ([][]byte, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	requests := make([]rpcCall, len(calls))
	for i, call := range calls {
		requests[i] = rpcCall{Method: "eth_call", Params: []any{map[string]string{"to": call.To, "data": call.Data}, blockTag}}
	}
	raw, err := c.batchRequest(ctx, requests)
	if err != nil {
		return nil, err
	}
	results := make([][]byte, len(raw))
	for i, message := range raw {
		var result string
		if err := json.Unmarshal(message, &result); err != nil {
			return nil, fmt.Errorf("decode eth_call result: %w", err)
		}
		decoded, err := decodeHexResult(result)
		if err != nil {
			return nil, err
		}
		results[i] = decoded
	}
	return results, nil
}

type rpcCall struct {
	Method string
	Params any
}

// batchRequest sends one JSON-RPC batch and returns results ordered to match
// calls. Responses are matched by request id (the spec does not guarantee
// response order), and any transport, HTTP, or per-item error fails over to the
// next configured endpoint just like a single request.
func (c *RPCClient) batchRequest(ctx context.Context, calls []rpcCall) ([]json.RawMessage, error) {
	var lastErr error
	for _, endpoint := range c.urls {
		requests := make([]rpcRequest, len(calls))
		indexByID := make(map[uint64]int, len(calls))
		for i, call := range calls {
			id := c.nextID.Add(1)
			requests[i] = rpcRequest{JSONRPC: "2.0", ID: id, Method: call.Method, Params: call.Params}
			indexByID[id] = i
		}
		payload, err := json.Marshal(requests)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode/100 != 2 {
			lastErr = fmt.Errorf("rpc http %d: %s", resp.StatusCode, string(body))
			continue
		}
		var decoded []rpcResponse
		if err := json.Unmarshal(body, &decoded); err != nil {
			// A node may reject a malformed batch with a single error object
			// rather than an array; surface that instead of a decode error.
			var single rpcResponse
			if json.Unmarshal(body, &single) == nil && single.Error != nil {
				lastErr = fmt.Errorf("rpc error %d: %s", single.Error.Code, single.Error.Message)
				continue
			}
			lastErr = err
			continue
		}
		if len(decoded) != len(calls) {
			lastErr = fmt.Errorf("rpc batch returned %d results for %d requests", len(decoded), len(calls))
			continue
		}
		results := make([]json.RawMessage, len(calls))
		filled := make([]bool, len(calls))
		var itemErr error
		for _, item := range decoded {
			index, ok := indexByID[item.ID]
			if !ok {
				itemErr = fmt.Errorf("rpc batch returned unexpected id %d", item.ID)
				break
			}
			if item.Error != nil {
				itemErr = fmt.Errorf("rpc error %d: %s", item.Error.Code, item.Error.Message)
				break
			}
			if filled[index] {
				itemErr = fmt.Errorf("rpc batch returned duplicate id %d", item.ID)
				break
			}
			results[index] = item.Result
			filled[index] = true
		}
		if itemErr != nil {
			lastErr = itemErr
			continue
		}
		return results, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no rpc endpoints configured")
	}
	return nil, lastErr
}

func (c *RPCClient) InspectPair(ctx context.Context, pairAddress string) (PairInfo, error) {
	pairAddress = normalizeAddress(pairAddress)
	token0Raw, err := c.CallAt(ctx, pairAddress, selectorToken0, "latest")
	if err != nil {
		return PairInfo{}, fmt.Errorf("read token0: %w", err)
	}
	token1Raw, err := c.CallAt(ctx, pairAddress, selectorToken1, "latest")
	if err != nil {
		return PairInfo{}, fmt.Errorf("read token1: %w", err)
	}
	factoryRaw, err := c.CallAt(ctx, pairAddress, selectorFactory, "latest")
	if err != nil {
		return PairInfo{}, fmt.Errorf("read factory: %w", err)
	}
	token0, err := wordAddress(token0Raw)
	if err != nil {
		return PairInfo{}, fmt.Errorf("decode token0: %w", err)
	}
	token1, err := wordAddress(token1Raw)
	if err != nil {
		return PairInfo{}, fmt.Errorf("decode token1: %w", err)
	}
	factory, err := wordAddress(factoryRaw)
	if err != nil {
		return PairInfo{}, fmt.Errorf("decode factory: %w", err)
	}
	if token0 == token1 || token0 == "0x0000000000000000000000000000000000000000" || token1 == "0x0000000000000000000000000000000000000000" {
		return PairInfo{}, fmt.Errorf("pair returned invalid token addresses")
	}
	return PairInfo{PairAddress: pairAddress, Token0: token0, Token1: token1, Factory: factory}, nil
}

func parseHexUint64(s string) (uint64, error) {
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 16, 64)
}
