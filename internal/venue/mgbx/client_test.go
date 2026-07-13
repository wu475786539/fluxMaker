package mgbx

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"fluxmaker/internal/num"
)

func TestTopBookContract(t *testing.T) {
	c := New("mgbx", "https://api.invalid", "", "", time.Second)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"msg":"success","data":{"t":1783832918247,"s":"BTC_USDT","u":1,"b":[["64004.38","11.38603"]],"a":[["64005.67","9.60533"]]}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	book, err := c.TopBook(context.Background(), "BTC_USDT")
	if err != nil {
		t.Fatal(err)
	}
	if book.BidPrice.Cmp(num.Must("64004.38")) != 0 || book.AskPrice.Cmp(num.Must("64005.67")) != 0 {
		t.Fatalf("book=%+v", book)
	}
}

func TestMarketRulesContract(t *testing.T) {
	c := New("mgbx", "https://api.invalid", "", "", time.Second)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"msg":"success","data":[{"symbol":"BTC_USDT","baseAsset":"BTC","quoteAsset":"USDT","pricePrecision":2,"quantityPrecision":5}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	rules, err := c.MarketRules(context.Background(), "BTC_USDT")
	if err != nil || rules.PriceTick.Cmp(num.Must("0.01")) != 0 || rules.QuantityStep.Cmp(num.Must("0.00001")) != 0 {
		t.Fatalf("rules=%+v err=%v", rules, err)
	}
}

func TestOpenOrdersReadsEveryPage(t *testing.T) {
	c := New("mgbx", "https://api.invalid", "key", "secret", time.Second)
	pages := 0
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		pages++
		page := r.URL.Query().Get("page")
		var body string
		switch page {
		case "1":
			body = `{"code":0,"msg":"success","data":{"page":1,"ps":100,"total":3,"items":[{"orderId":"1","symbol":"BTC_USDT","orderSide":"BUY","price":"10","origQty":"1","executedQty":"0","state":"NEW","createdTime":1},{"orderId":"2","symbol":"BTC_USDT","orderSide":"SELL","price":"11","origQty":"1","executedQty":"0","state":"NEW","createdTime":2}]}}`
		case "2":
			body = `{"code":0,"msg":"success","data":{"page":2,"ps":100,"total":3,"items":[{"orderId":"3","symbol":"BTC_USDT","orderSide":"SELL","price":"12","origQty":"1","executedQty":"0","state":"NEW","createdTime":3}]}}`
		default:
			t.Fatalf("unexpected page %s", page)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	orders, err := c.OpenOrders(context.Background(), "BTC_USDT")
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 3 || pages != 2 || orders[2].OrderID != "3" {
		t.Fatalf("orders=%+v pages=%d", orders, pages)
	}
}

func TestOrderDetailAndBatchCancel(t *testing.T) {
	c := New("mgbx", "https://api.invalid", "key", "secret", time.Second)
	batchCalls := 0
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/spot/v1/u/trade/order/detail":
			body := `{"code":0,"msg":"success","data":{"orderId":"9","symbol":"BTC_USDT","orderSide":"BUY","price":"10","origQty":"2","executedQty":"1","state":"PARTIALLY_FILLED","createdTime":1}}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
		case "/spot/v1/u/trade/order/batch/cancel":
			batchCalls++
			var ids []string
			if err := json.Unmarshal([]byte(r.URL.Query().Get("orderIdsJson")), &ids); err != nil {
				t.Fatal(err)
			}
			if len(ids) > 20 {
				t.Fatalf("batch size=%d", len(ids))
			}
			body := `{"code":0,"msg":"success","data":true}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
		default:
			t.Fatalf("path=%s", r.URL.Path)
			return nil, nil
		}
	})
	order, err := c.Order(context.Background(), "BTC_USDT", "9")
	if err != nil || order.State != "PARTIALLY_FILLED" || order.ExecutedQty.Cmp(num.Must("1")) != 0 {
		t.Fatalf("order=%+v err=%v", order, err)
	}
	ids := make([]string, 45)
	for i := range ids {
		ids[i] = string(rune('a' + i%20))
	}
	if err := c.CancelOrders(context.Background(), "BTC_USDT", ids); err != nil {
		t.Fatal(err)
	}
	if batchCalls != 3 {
		t.Fatalf("batch calls=%d want=3", batchCalls)
	}
}

func TestRecentFillsUsesHistoricalOrderAggregates(t *testing.T) {
	c := New("mgbx", "https://api.invalid", "key", "secret", time.Second)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/spot/v1/u/trade/order/history" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		body := `{"code":0,"msg":"success","data":{"items":[{"orderId":"9","symbol":"BTC_USDT","orderSide":"SELL","avgPrice":"64000","executedQty":"0.2","createdTime":1783832918247},{"orderId":"10","symbol":"BTC_USDT","orderSide":"BUY","avgPrice":"0","executedQty":"0","createdTime":1783832918248}]}}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body)), Header: make(http.Header)}, nil
	})
	fills, err := c.RecentFills(context.Background(), "BTC_USDT", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 || fills[0].TradeID != "order:9" || !fills[0].Aggregate || fills[0].QuoteQuantity.Cmp(num.Must("12800")) != 0 {
		t.Fatalf("fills=%+v", fills)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
