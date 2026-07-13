package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
	"fluxmaker/internal/venue"
)

type Client struct {
	name             string
	identity         string
	baseURL          string
	apiKey           string
	secret           string
	stpMode          string
	http             *http.Client
	timeSyncMu       sync.Mutex
	timeOffsetMillis atomic.Int64
}

// Binance Spot does not expose a selective bulk-cancel endpoint for ordinary
// orders. Keep cancellation selective (so shared accounts retain unmanaged
// orders), but issue a small bounded number of DELETE requests concurrently.
// The OMS already chunks calls to at most 20 order IDs.
const maxConcurrentCancels = 5

func New(name, baseURL, apiKey, secret, stpMode string, timeout time.Duration) *Client {
	return NewWithIdentity(name, name, baseURL, apiKey, secret, stpMode, timeout)
}

func NewWithIdentity(name, identity, baseURL, apiKey, secret, stpMode string, timeout time.Duration) *Client {
	return &Client{name: name, identity: identity, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, secret: secret, stpMode: stpMode, http: &http.Client{Timeout: timeout}}
}

func (c *Client) Name() string          { return c.name }
func (c *Client) StateIdentity() string { return c.identity }
func (c *Client) Capabilities() venue.Capabilities {
	return venue.Capabilities{ClientOrderIDs: true}
}

func (c *Client) TopBook(ctx context.Context, symbol string) (domain.Book, error) {
	values := url.Values{"symbol": []string{symbol}}
	var raw struct {
		Symbol   string `json:"symbol"`
		BidPrice string `json:"bidPrice"`
		BidQty   string `json:"bidQty"`
		AskPrice string `json:"askPrice"`
		AskQty   string `json:"askQty"`
	}
	if err := c.public(ctx, http.MethodGet, "/api/v3/ticker/bookTicker", values, &raw); err != nil {
		return domain.Book{}, err
	}
	bid, err := num.Parse(raw.BidPrice)
	if err != nil {
		return domain.Book{}, err
	}
	bidQty, err := num.Parse(raw.BidQty)
	if err != nil {
		return domain.Book{}, err
	}
	ask, err := num.Parse(raw.AskPrice)
	if err != nil {
		return domain.Book{}, err
	}
	askQty, err := num.Parse(raw.AskQty)
	if err != nil {
		return domain.Book{}, err
	}
	return domain.Book{Venue: c.name, Symbol: raw.Symbol, BidPrice: bid, BidQty: bidQty, AskPrice: ask, AskQty: askQty, Timestamp: time.Now().UTC()}, nil
}

func (c *Client) MarketRules(ctx context.Context, symbol string) (domain.MarketRules, error) {
	var raw struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
			Filters    []struct {
				Type        string `json:"filterType"`
				TickSize    string `json:"tickSize"`
				StepSize    string `json:"stepSize"`
				MinQty      string `json:"minQty"`
				MaxQty      string `json:"maxQty"`
				MinPrice    string `json:"minPrice"`
				MaxPrice    string `json:"maxPrice"`
				MinNotional string `json:"minNotional"`
				MaxNotional string `json:"maxNotional"`
				Notional    string `json:"notional"`
				Limit       int    `json:"maxNumOrders"`
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := c.public(ctx, http.MethodGet, "/api/v3/exchangeInfo", url.Values{"symbol": []string{symbol}}, &raw); err != nil {
		return domain.MarketRules{}, err
	}
	if len(raw.Symbols) != 1 {
		return domain.MarketRules{}, fmt.Errorf("binance symbol %s not found", symbol)
	}
	item := raw.Symbols[0]
	rules := domain.MarketRules{Symbol: item.Symbol, BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset}
	for _, filter := range item.Filters {
		switch filter.Type {
		case "PRICE_FILTER":
			rules.PriceTick = parseOptionalDecimal(filter.TickSize)
			rules.MinPrice = parseOptionalDecimal(filter.MinPrice)
			rules.MaxPrice = parseOptionalDecimal(filter.MaxPrice)
		case "LOT_SIZE":
			rules.QuantityStep = parseOptionalDecimal(filter.StepSize)
			rules.MinQuantity = parseOptionalDecimal(filter.MinQty)
			rules.MaxQuantity = parseOptionalDecimal(filter.MaxQty)
		case "MIN_NOTIONAL":
			rules.MinNotional = parseOptionalDecimal(filter.MinNotional)
			if rules.MinNotional.IsZero() {
				rules.MinNotional = parseOptionalDecimal(filter.Notional)
			}
		case "NOTIONAL":
			rules.MinNotional = parseOptionalDecimal(filter.MinNotional)
			rules.MaxNotional = parseOptionalDecimal(filter.MaxNotional)
		case "MAX_NUM_ORDERS":
			rules.MaxOpenOrders = filter.Limit
		}
	}
	if !rules.PriceTick.IsPositive() || !rules.QuantityStep.IsPositive() {
		return domain.MarketRules{}, fmt.Errorf("binance symbol %s returned incomplete trading rules", symbol)
	}
	return rules, nil
}

func parseOptionalDecimal(value string) num.Decimal {
	if value == "" {
		return num.FromInt64(0)
	}
	parsed, err := num.Parse(value)
	if err != nil {
		return num.FromInt64(0)
	}
	return parsed
}

func (c *Client) Balances(ctx context.Context) ([]domain.Balance, error) {
	var raw struct {
		Balances []struct{ Asset, Free, Locked string } `json:"balances"`
	}
	if err := c.signed(ctx, http.MethodGet, "/api/v3/account", url.Values{}, &raw); err != nil {
		return nil, err
	}
	result := make([]domain.Balance, 0, len(raw.Balances))
	for _, b := range raw.Balances {
		free, err := num.Parse(b.Free)
		if err != nil {
			return nil, err
		}
		locked, err := num.Parse(b.Locked)
		if err != nil {
			return nil, err
		}
		result = append(result, domain.Balance{Asset: b.Asset, Free: free, Locked: locked})
	}
	return result, nil
}

func (c *Client) OpenOrders(ctx context.Context, symbol string) ([]domain.Order, error) {
	var raw []struct {
		Symbol        string `json:"symbol"`
		OrderID       int64  `json:"orderId"`
		ClientOrderID string `json:"clientOrderId"`
		Price         string `json:"price"`
		OrigQty       string `json:"origQty"`
		ExecutedQty   string `json:"executedQty"`
		Status        string `json:"status"`
		Side          string `json:"side"`
		Time          int64  `json:"time"`
	}
	if err := c.signed(ctx, http.MethodGet, "/api/v3/openOrders", url.Values{"symbol": []string{symbol}}, &raw); err != nil {
		return nil, err
	}
	orders := make([]domain.Order, 0, len(raw))
	for _, item := range raw {
		price, err := num.Parse(item.Price)
		if err != nil {
			return nil, err
		}
		qty, err := num.Parse(item.OrigQty)
		if err != nil {
			return nil, err
		}
		executed, err := num.Parse(item.ExecutedQty)
		if err != nil {
			return nil, err
		}
		orders = append(orders, domain.Order{Venue: c.name, OrderID: strconv.FormatInt(item.OrderID, 10), ClientID: item.ClientOrderID, Symbol: item.Symbol, Side: domain.Side(item.Side), Price: price, Quantity: qty, ExecutedQty: executed, State: domain.OrderState(item.Status), CreatedAt: time.UnixMilli(item.Time).UTC()})
	}
	return orders, nil
}

func (c *Client) Order(ctx context.Context, symbol, orderID string) (domain.Order, error) {
	var raw struct {
		Symbol        string `json:"symbol"`
		OrderID       int64  `json:"orderId"`
		ClientOrderID string `json:"clientOrderId"`
		Price         string `json:"price"`
		OrigQty       string `json:"origQty"`
		ExecutedQty   string `json:"executedQty"`
		Status        string `json:"status"`
		Side          string `json:"side"`
		Time          int64  `json:"time"`
	}
	values := url.Values{"symbol": []string{symbol}, "orderId": []string{orderID}}
	if err := c.signed(ctx, http.MethodGet, "/api/v3/order", values, &raw); err != nil {
		return domain.Order{}, err
	}
	price, err := num.Parse(raw.Price)
	if err != nil {
		return domain.Order{}, err
	}
	quantity, err := num.Parse(raw.OrigQty)
	if err != nil {
		return domain.Order{}, err
	}
	executed, err := num.Parse(raw.ExecutedQty)
	if err != nil {
		return domain.Order{}, err
	}
	return domain.Order{Venue: c.name, OrderID: strconv.FormatInt(raw.OrderID, 10), ClientID: raw.ClientOrderID, Symbol: raw.Symbol, Side: domain.Side(raw.Side), Price: price, Quantity: quantity, ExecutedQty: executed, State: domain.OrderState(raw.Status), CreatedAt: time.UnixMilli(raw.Time).UTC()}, nil
}

func (c *Client) RecentFills(ctx context.Context, symbol string, limit int) ([]domain.Fill, error) {
	if limit < 1 || limit > 1000 {
		limit = 50
	}
	var raw []struct {
		Symbol          string `json:"symbol"`
		ID              int64  `json:"id"`
		OrderID         int64  `json:"orderId"`
		Price           string `json:"price"`
		Qty             string `json:"qty"`
		QuoteQty        string `json:"quoteQty"`
		Commission      string `json:"commission"`
		CommissionAsset string `json:"commissionAsset"`
		Time            int64  `json:"time"`
		IsBuyer         bool   `json:"isBuyer"`
		IsMaker         bool   `json:"isMaker"`
	}
	values := url.Values{"symbol": []string{symbol}, "limit": []string{strconv.Itoa(limit)}}
	if err := c.signed(ctx, http.MethodGet, "/api/v3/myTrades", values, &raw); err != nil {
		return nil, err
	}
	result := make([]domain.Fill, 0, len(raw))
	for _, item := range raw {
		price, err := num.Parse(item.Price)
		if err != nil {
			return nil, err
		}
		quantity, err := num.Parse(item.Qty)
		if err != nil {
			return nil, err
		}
		quoteQuantity, err := num.Parse(item.QuoteQty)
		if err != nil {
			return nil, err
		}
		fee, err := num.Parse(item.Commission)
		if err != nil {
			return nil, err
		}
		side := domain.Sell
		if item.IsBuyer {
			side = domain.Buy
		}
		result = append(result, domain.Fill{Venue: c.name, TradeID: strconv.FormatInt(item.ID, 10), OrderID: strconv.FormatInt(item.OrderID, 10), Symbol: item.Symbol, Side: side, Price: price, Quantity: quantity, QuoteQuantity: quoteQuantity, Fee: fee, FeeAsset: item.CommissionAsset, Maker: item.IsMaker, Timestamp: time.UnixMilli(item.Time).UTC()})
	}
	return result, nil
}

func (c *Client) PlacePostOnly(ctx context.Context, request venue.PlaceRequest) (domain.Order, error) {
	values := url.Values{
		"symbol":           []string{request.Symbol},
		"side":             []string{string(request.Side)},
		"type":             []string{"LIMIT_MAKER"},
		"quantity":         []string{request.Quantity.String()},
		"price":            []string{request.Price.String()},
		"newClientOrderId": []string{request.ClientID},
		"newOrderRespType": []string{"RESULT"},
	}
	if c.stpMode != "" {
		values.Set("selfTradePreventionMode", c.stpMode)
	}
	var raw struct {
		Symbol        string `json:"symbol"`
		OrderID       int64  `json:"orderId"`
		ClientOrderID string `json:"clientOrderId"`
		Price         string `json:"price"`
		OrigQty       string `json:"origQty"`
		ExecutedQty   string `json:"executedQty"`
		Status        string `json:"status"`
	}
	if err := c.signed(ctx, http.MethodPost, "/api/v3/order", values, &raw); err != nil {
		return domain.Order{}, err
	}
	price, _ := num.Parse(raw.Price)
	qty, _ := num.Parse(raw.OrigQty)
	executed, _ := num.Parse(raw.ExecutedQty)
	return domain.Order{Venue: c.name, OrderID: strconv.FormatInt(raw.OrderID, 10), ClientID: raw.ClientOrderID, Symbol: raw.Symbol, Side: request.Side, Price: price, Quantity: qty, ExecutedQty: executed, State: domain.OrderState(raw.Status), CreatedAt: time.Now().UTC()}, nil
}

func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	return c.signed(ctx, http.MethodDelete, "/api/v3/order", url.Values{"symbol": []string{symbol}, "orderId": []string{orderID}}, &struct{}{})
}

func (c *Client) CancelOrders(ctx context.Context, symbol string, orderIDs []string) error {
	if len(orderIDs) == 0 {
		return nil
	}
	workerCount := min(maxConcurrentCancels, len(orderIDs))
	jobs := make(chan string, len(orderIDs))
	for _, orderID := range orderIDs {
		jobs <- orderID
	}
	close(jobs)

	var wg sync.WaitGroup
	var errorsMu sync.Mutex
	var failures []error
	worker := func() {
		defer wg.Done()
		for orderID := range jobs {
			if err := c.CancelOrder(ctx, symbol, orderID); err != nil {
				errorsMu.Lock()
				failures = append(failures, fmt.Errorf("cancel order %s: %w", orderID, err))
				errorsMu.Unlock()
			}
		}
	}
	for range workerCount {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	return errors.Join(failures...)
}

func (c *Client) public(ctx context.Context, method, path string, values url.Values, out any) error {
	return c.do(ctx, method, path, values, false, out)
}

func (c *Client) signed(ctx context.Context, method, path string, values url.Values, out any) error {
	if c.apiKey == "" || c.secret == "" {
		return fmt.Errorf("binance credentials are not configured")
	}
	err := c.signedOnce(ctx, method, path, values, out)
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Code != -1021 {
		return err
	}
	if syncErr := c.syncServerTime(ctx); syncErr != nil {
		return fmt.Errorf("%w; synchronize binance server time: %v", err, syncErr)
	}
	return c.signedOnce(ctx, method, path, values, out)
}

func (c *Client) signedOnce(ctx context.Context, method, path string, values url.Values, out any) error {
	values = cloneValues(values)
	timestamp := time.Now().UnixMilli() + c.timeOffsetMillis.Load()
	values.Set("timestamp", strconv.FormatInt(timestamp, 10))
	values.Set("recvWindow", "5000")
	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(values.Encode()))
	values.Set("signature", hex.EncodeToString(mac.Sum(nil)))
	return c.do(ctx, method, path, values, true, out)
}

func (c *Client) syncServerTime(ctx context.Context) error {
	c.timeSyncMu.Lock()
	defer c.timeSyncMu.Unlock()
	started := time.Now()
	var raw struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := c.public(ctx, http.MethodGet, "/api/v3/time", url.Values{}, &raw); err != nil {
		return err
	}
	if raw.ServerTime <= 0 {
		return fmt.Errorf("binance returned invalid server time")
	}
	localMidpoint := started.UnixMilli() + time.Since(started).Milliseconds()/2
	c.timeOffsetMillis.Store(raw.ServerTime - localMidpoint)
	return nil
}

type apiError struct {
	Status int
	Code   int
	Msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("binance http %d code=%d: %s", e.Status, e.Code, e.Msg)
}

func (c *Client) do(ctx context.Context, method, path string, values url.Values, authenticated bool, out any) error {
	endpoint := c.baseURL + path
	if encoded := values.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return err
	}
	if authenticated {
		req.Header.Set("X-MBX-APIKEY", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var apiErr struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(body, &apiErr)
		return &apiError{Status: resp.StatusCode, Code: apiErr.Code, Msg: apiErr.Msg}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode binance response: %w", err)
	}
	return nil
}

func cloneValues(v url.Values) url.Values {
	result := make(url.Values, len(v))
	for key, values := range v {
		result[key] = append([]string(nil), values...)
	}
	return result
}
