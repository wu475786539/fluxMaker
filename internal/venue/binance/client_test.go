package binance

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"fluxmaker/internal/num"
)

func TestTopBookContract(t *testing.T) {
	c := New("binance", "https://api.invalid", "", "", "", time.Second)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"symbol":"BTCUSDT","bidPrice":"64005.02","bidQty":"1.8","askPrice":"64005.03","askQty":"4.0"}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	book, err := c.TopBook(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatal(err)
	}
	if book.BidPrice.Cmp(num.Must("64005.02")) != 0 || book.AskPrice.Cmp(num.Must("64005.03")) != 0 {
		t.Fatalf("book=%+v", book)
	}
}

func TestMarketRulesContract(t *testing.T) {
	c := New("binance", "https://api.invalid", "", "", "", time.Second)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"symbols":[{"symbol":"BTCUSDT","baseAsset":"BTC","quoteAsset":"USDT","filters":[{"filterType":"PRICE_FILTER","minPrice":"0.01","maxPrice":"1000000","tickSize":"0.01"},{"filterType":"LOT_SIZE","minQty":"0.0001","maxQty":"100","stepSize":"0.0001"},{"filterType":"NOTIONAL","minNotional":"5","maxNotional":"100000"},{"filterType":"MAX_NUM_ORDERS","maxNumOrders":200}]}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	rules, err := c.MarketRules(context.Background(), "BTCUSDT")
	if err != nil || rules.PriceTick.Cmp(num.Must("0.01")) != 0 || rules.QuantityStep.Cmp(num.Must("0.0001")) != 0 || rules.MaxOpenOrders != 200 {
		t.Fatalf("rules=%+v err=%v", rules, err)
	}
}

func TestRecentFillsContract(t *testing.T) {
	c := New("binance", "https://api.invalid", "key", "secret", "", time.Second)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v3/myTrades" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		body := `[{"symbol":"BTCUSDT","id":7,"orderId":9,"price":"64000","qty":"0.1","quoteQty":"6400","commission":"0.001","commissionAsset":"BNB","time":1783832918247,"isBuyer":true,"isMaker":true}]`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	fills, err := c.RecentFills(context.Background(), "BTCUSDT", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 || fills[0].TradeID != "7" || fills[0].Side != "BUY" || fills[0].Price.Cmp(num.Must("64000")) != 0 {
		t.Fatalf("fills=%+v", fills)
	}
}

func TestOrderDetailContract(t *testing.T) {
	c := New("binance", "https://api.invalid", "key", "secret", "", time.Second)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/v3/order" || r.URL.Query().Get("orderId") != "9" {
			t.Fatalf("request=%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		body := `{"symbol":"BTCUSDT","orderId":9,"clientOrderId":"fm-1","price":"64000","origQty":"0.1","executedQty":"0.05","status":"PARTIALLY_FILLED","side":"BUY","time":1783832918247}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	order, err := c.Order(context.Background(), "BTCUSDT", "9")
	if err != nil || order.OrderID != "9" || order.State != "PARTIALLY_FILLED" || order.ExecutedQty.Cmp(num.Must("0.05")) != 0 {
		t.Fatalf("order=%+v err=%v", order, err)
	}
}

func TestSignedRequestSynchronizesServerTimeAndRetriesTimestampError(t *testing.T) {
	c := New("binance", "https://api.invalid", "key", "secret", "", time.Second)
	serverTime := time.Now().UnixMilli() - 2500
	accountCalls := 0
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v3/time":
			body := `{"serverTime":` + strconv.FormatInt(serverTime, 10) + `}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
		case "/api/v3/account":
			accountCalls++
			if accountCalls == 1 {
				return &http.Response{StatusCode: 400, Body: io.NopCloser(bytes.NewBufferString(`{"code":-1021,"msg":"Timestamp ahead"}`)), Header: make(http.Header)}, nil
			}
			timestamp, err := strconv.ParseInt(r.URL.Query().Get("timestamp"), 10, 64)
			if err != nil || timestamp < serverTime-100 || timestamp > serverTime+500 {
				t.Fatalf("retry timestamp=%d server=%d err=%v", timestamp, serverTime, err)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(`{"balances":[]}`)), Header: make(http.Header)}, nil
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
			return nil, nil
		}
	})

	if _, err := c.Balances(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accountCalls != 2 {
		t.Fatalf("account calls=%d want=2", accountCalls)
	}
}

func TestCancelOrdersUsesBoundedConcurrency(t *testing.T) {
	c := New("binance", "https://api.invalid", "key", "secret", "", time.Second)
	var active atomic.Int64
	var maximum atomic.Int64
	var calls atomic.Int64
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v3/order" || r.URL.Query().Get("symbol") != "BTCUSDT" || r.URL.Query().Get("orderId") == "" {
			t.Fatalf("request=%s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		current := active.Add(1)
		for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
		}
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(`{}`)), Header: make(http.Header)}, nil
	})
	ids := make([]string, 12)
	for i := range ids {
		ids[i] = strconv.Itoa(i + 1)
	}
	if err := c.CancelOrders(context.Background(), "BTCUSDT", ids); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != int64(len(ids)) {
		t.Fatalf("calls=%d want=%d", calls.Load(), len(ids))
	}
	if maximum.Load() <= 1 || maximum.Load() > maxConcurrentCancels {
		t.Fatalf("maximum concurrency=%d want=2..%d", maximum.Load(), maxConcurrentCancels)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
