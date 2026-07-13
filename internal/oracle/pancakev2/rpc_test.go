package pancakev2

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatchCallSingleRoundTripPreservesOrder(t *testing.T) {
	rpc := NewRPCClient([]string{"http://rpc.invalid"}, time.Second)
	var calls atomic.Int64
	rpc.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var batch []struct {
			ID     uint64            `json:"id"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &batch); err != nil {
			t.Errorf("expected a JSON-RPC batch array: %v", err)
		}
		// Echo each call's data selector back as its result, then shuffle the
		// responses to prove BatchCall reorders by request id, not array position.
		responses := make([]map[string]any, len(batch))
		for i, item := range batch {
			var call map[string]string
			_ = json.Unmarshal(item.Params[0], &call)
			responses[i] = map[string]any{"jsonrpc": "2.0", "id": item.ID, "result": call["data"]}
		}
		for i, j := 0, len(responses)-1; i < j; i, j = i+1, j-1 {
			responses[i], responses[j] = responses[j], responses[i]
		}
		body, _ := json.Marshal(responses)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	results, err := rpc.BatchCall(context.Background(), []BatchCall{
		{To: "0xaaa", Data: "0x01"},
		{To: "0xbbb", Data: "0x02"},
		{To: "0xccc", Data: "0x03"},
	}, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 HTTP round-trip for 3 calls, got %d", calls.Load())
	}
	want := []string{"01", "02", "03"}
	for i, r := range results {
		if hex.EncodeToString(r) != want[i] {
			t.Fatalf("result[%d]=%x want %s (order not preserved)", i, r, want[i])
		}
	}
}

func TestBatchCallPropagatesSubError(t *testing.T) {
	rpc := NewRPCClient([]string{"http://rpc.invalid"}, time.Second)
	rpc.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		var batch []struct {
			ID uint64 `json:"id"`
		}
		_ = json.Unmarshal(raw, &batch)
		responses := make([]map[string]any, len(batch))
		for i, item := range batch {
			if i == 1 {
				responses[i] = map[string]any{"jsonrpc": "2.0", "id": item.ID, "error": map[string]any{"code": -32000, "message": "execution reverted"}}
			} else {
				responses[i] = map[string]any{"jsonrpc": "2.0", "id": item.ID, "result": "0x01"}
			}
		}
		body, _ := json.Marshal(responses)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	_, err := rpc.BatchCall(context.Background(), []BatchCall{{To: "0xa", Data: "0x1"}, {To: "0xb", Data: "0x2"}}, "latest")
	if err == nil || !strings.Contains(err.Error(), "execution reverted") {
		t.Fatalf("expected sub-error to propagate, got %v", err)
	}
}

func TestLatestBlockCoalescesConcurrentRequests(t *testing.T) {
	rpc := NewRPCClient([]string{"http://rpc.invalid"}, time.Second)
	var calls atomic.Int64
	rpc.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		body := `{"jsonrpc":"2.0","id":1,"result":{"number":"0x10","timestamp":"0x20"}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			block, err := rpc.LatestBlock(context.Background())
			if err != nil || block.Number != 16 {
				t.Errorf("block=%+v err=%v", block, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("latest block requests=%d want=1", calls.Load())
	}
}
