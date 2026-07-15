package mgbx

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"fluxmaker/internal/domain"
	"fluxmaker/internal/num"
	"fluxmaker/internal/venue"
)

type Client struct {
	name     string
	identity string
	baseURL  string
	apiKey   string
	secret   string
	http     *http.Client
}

type envelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type orderPayload struct {
	OrderID     string `json:"orderId"`
	Symbol      string `json:"symbol"`
	OrderType   string `json:"orderType"`
	OrderSide   string `json:"orderSide"`
	TimeInForce string `json:"timeInForce"`
	Price       string `json:"price"`
	OrigQty     string `json:"origQty"`
	ExecutedQty string `json:"executedQty"`
	State       string `json:"state"`
	CreatedTime int64  `json:"createdTime"`
}

func New(name, baseURL, apiKey, secret string, timeout time.Duration) *Client {
	return NewWithIdentity(name, name, baseURL, apiKey, secret, timeout)
}

func NewWithIdentity(name, identity, baseURL, apiKey, secret string, timeout time.Duration) *Client {
	return &Client{name: name, identity: identity, baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, secret: secret, http: &http.Client{Timeout: timeout}}
}

func (c *Client) Name() string          { return c.name }
func (c *Client) StateIdentity() string { return c.identity }
func (c *Client) Capabilities() venue.Capabilities {
	// MGBX order responses do not expose the client order ID submitted by the
	// OMS, so this adapter deliberately manages every order on its dedicated
	// account. Native batch cancellation is derived from BatchCanceler.
	return venue.Capabilities{ClientOrderIDs: false}
}

func (c *Client) TopBook(ctx context.Context, symbol string) (domain.Book, error) {
	data, err := c.do(ctx, http.MethodGet, "/spot/v1/p/quotation/depth", url.Values{"symbol": []string{symbol}, "level": []string{"5"}}, false)
	if err != nil {
		return domain.Book{}, err
	}
	var raw struct {
		Timestamp int64      `json:"t"`
		Symbol    string     `json:"s"`
		Bids      [][]string `json:"b"`
		Asks      [][]string `json:"a"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return domain.Book{}, err
	}
	ts := time.Now().UTC()
	if raw.Timestamp > 0 {
		ts = time.UnixMilli(raw.Timestamp).UTC()
	}
	book := domain.Book{Venue: c.name, Symbol: raw.Symbol, Timestamp: ts}
	if book.Symbol == "" {
		book.Symbol = symbol
	}
	if len(raw.Bids) > 0 {
		if len(raw.Bids[0]) < 2 {
			return domain.Book{}, fmt.Errorf("decode MGBX bid: expected price and quantity")
		}
		bid, err := num.Parse(raw.Bids[0][0])
		if err != nil {
			return domain.Book{}, fmt.Errorf("decode MGBX bid price: %w", err)
		}
		bidQty, err := num.Parse(raw.Bids[0][1])
		if err != nil {
			return domain.Book{}, fmt.Errorf("decode MGBX bid quantity: %w", err)
		}
		book.BidPrice, book.BidQty = bid, bidQty
	}
	if len(raw.Asks) > 0 {
		if len(raw.Asks[0]) < 2 {
			return domain.Book{}, fmt.Errorf("decode MGBX ask: expected price and quantity")
		}
		ask, err := num.Parse(raw.Asks[0][0])
		if err != nil {
			return domain.Book{}, fmt.Errorf("decode MGBX ask price: %w", err)
		}
		askQty, err := num.Parse(raw.Asks[0][1])
		if err != nil {
			return domain.Book{}, fmt.Errorf("decode MGBX ask quantity: %w", err)
		}
		book.AskPrice, book.AskQty = ask, askQty
	}
	return book, nil
}

func (c *Client) MarketRules(ctx context.Context, symbol string) (domain.MarketRules, error) {
	data, err := c.do(ctx, http.MethodGet, "/spot/v1/p/symbol/configs", url.Values{}, false)
	if err != nil {
		return domain.MarketRules{}, err
	}
	var raw []struct {
		Symbol            string `json:"symbol"`
		BaseAsset         string `json:"baseAsset"`
		QuoteAsset        string `json:"quoteAsset"`
		PricePrecision    int    `json:"pricePrecision"`
		QuantityPrecision int    `json:"quantityPrecision"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return domain.MarketRules{}, err
	}
	for _, item := range raw {
		if strings.EqualFold(item.Symbol, symbol) {
			priceTick, err := precisionStep(item.PricePrecision)
			if err != nil {
				return domain.MarketRules{}, err
			}
			quantityStep, err := precisionStep(item.QuantityPrecision)
			if err != nil {
				return domain.MarketRules{}, err
			}
			return domain.MarketRules{Symbol: item.Symbol, BaseAsset: item.BaseAsset, QuoteAsset: item.QuoteAsset, PriceTick: priceTick, QuantityStep: quantityStep}, nil
		}
	}
	return domain.MarketRules{}, fmt.Errorf("MGBX symbol %s not found", symbol)
}

func precisionStep(precision int) (num.Decimal, error) {
	if precision < 0 || precision > 30 {
		return num.Decimal{}, fmt.Errorf("unsupported precision %d", precision)
	}
	if precision == 0 {
		return num.One(), nil
	}
	return num.Parse("0." + strings.Repeat("0", precision-1) + "1")
}

func (c *Client) Balances(ctx context.Context) ([]domain.Balance, error) {
	data, err := c.do(ctx, http.MethodGet, "/spot/v1/u/balance/spot", url.Values{}, true)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Coin             string `json:"coin"`
		Balance          string `json:"balance"`
		Freeze           string `json:"freeze"`
		AvailableBalance string `json:"availableBalance"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	result := make([]domain.Balance, 0, len(raw))
	for _, b := range raw {
		free, err := num.Parse(b.AvailableBalance)
		if err != nil {
			return nil, err
		}
		locked, err := num.Parse(b.Freeze)
		if err != nil {
			return nil, err
		}
		result = append(result, domain.Balance{Asset: b.Coin, Free: free, Locked: locked})
	}
	return result, nil
}

func (c *Client) OpenOrders(ctx context.Context, symbol string) ([]domain.Order, error) {
	const pageSize = 100
	result := make([]domain.Order, 0, pageSize)
	for page := 1; page <= 1000; page++ {
		values := url.Values{"symbol": []string{symbol}, "state": []string{"9"}, "page": []string{strconv.Itoa(page)}, "size": []string{strconv.Itoa(pageSize)}}
		data, err := c.do(ctx, http.MethodGet, "/spot/v1/u/trade/order/list", values, true)
		if err != nil {
			return nil, err
		}
		var raw struct {
			Page  int            `json:"page"`
			PS    int            `json:"ps"`
			Total int64          `json:"total"`
			Items []orderPayload `json:"items"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		for _, item := range raw.Items {
			order, err := c.parseOrder(item)
			if err != nil {
				return nil, err
			}
			result = append(result, order)
		}
		if len(raw.Items) == 0 {
			return result, nil
		}
		if raw.Total > 0 {
			if int64(len(result)) >= raw.Total {
				return result, nil
			}
		} else if len(raw.Items) < pageSize {
			return result, nil
		}
	}
	return nil, fmt.Errorf("MGBX open-order pagination exceeded safety limit")
}

func (c *Client) Order(ctx context.Context, _ string, orderID string) (domain.Order, error) {
	data, err := c.do(ctx, http.MethodGet, "/spot/v1/u/trade/order/detail", url.Values{"orderId": []string{orderID}}, true)
	if err != nil {
		return domain.Order{}, err
	}
	var raw orderPayload
	if err := json.Unmarshal(data, &raw); err != nil {
		return domain.Order{}, fmt.Errorf("decode MGBX order detail: %w", err)
	}
	return c.parseOrder(raw)
}

func (c *Client) parseOrder(item orderPayload) (domain.Order, error) {
	price, err := num.Parse(item.Price)
	if err != nil {
		return domain.Order{}, err
	}
	qty, err := num.Parse(item.OrigQty)
	if err != nil {
		return domain.Order{}, err
	}
	executed, err := num.Parse(item.ExecutedQty)
	if err != nil {
		return domain.Order{}, err
	}
	return domain.Order{Venue: c.name, OrderID: item.OrderID, Symbol: item.Symbol, Side: domain.Side(item.OrderSide), Price: price, Quantity: qty, ExecutedQty: executed, State: domain.OrderState(item.State), CreatedAt: time.UnixMilli(item.CreatedTime).UTC()}, nil
}

// RecentFills uses MGBX historical orders because the REST API currently
// exposes aggregate executed quantity and average price rather than individual
// private fill rows. Aggregate=true makes that distinction explicit to callers.
func (c *Client) RecentFills(ctx context.Context, symbol string, limit int) ([]domain.Fill, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	values := url.Values{"symbol": []string{symbol}, "limit": []string{strconv.Itoa(limit)}}
	data, err := c.do(ctx, http.MethodGet, "/spot/v1/u/trade/order/history", values, true)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Items []struct {
			OrderID     string `json:"orderId"`
			Symbol      string `json:"symbol"`
			OrderSide   string `json:"orderSide"`
			AvgPrice    string `json:"avgPrice"`
			ExecutedQty string `json:"executedQty"`
			CreatedTime int64  `json:"createdTime"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	result := make([]domain.Fill, 0, len(raw.Items))
	for _, item := range raw.Items {
		quantity, err := num.Parse(item.ExecutedQty)
		if err != nil {
			return nil, err
		}
		if !quantity.IsPositive() {
			continue
		}
		price, err := num.Parse(item.AvgPrice)
		if err != nil {
			return nil, err
		}
		result = append(result, domain.Fill{Venue: c.name, TradeID: "order:" + item.OrderID, OrderID: item.OrderID, Symbol: item.Symbol, Side: domain.Side(item.OrderSide), Price: price, Quantity: quantity, QuoteQuantity: price.Mul(quantity), Aggregate: true, Timestamp: time.UnixMilli(item.CreatedTime).UTC()})
	}
	return result, nil
}

func (c *Client) PlacePostOnly(ctx context.Context, request venue.PlaceRequest) (domain.Order, error) {
	values := url.Values{
		"symbol":      []string{request.Symbol},
		"direction":   []string{string(request.Side)},
		"tradeType":   []string{"LIMIT"},
		"totalAmount": []string{request.Quantity.String()},
		"price":       []string{request.Price.String()},
		"timeInForce": []string{"GTX"},
	}
	data, err := c.do(ctx, http.MethodPost, "/spot/v1/u/trade/order/create", values, true)
	if err != nil {
		return domain.Order{}, err
	}
	var orderID string
	if err := json.Unmarshal(data, &orderID); err != nil {
		return domain.Order{}, fmt.Errorf("decode MGBX order id: %w", err)
	}
	return domain.Order{Venue: c.name, OrderID: orderID, Symbol: request.Symbol, Side: request.Side, Price: request.Price, Quantity: request.Quantity, State: domain.OrderUnknown, CreatedAt: time.Now().UTC()}, nil
}

// PlacePostOnlyBatch creates several post-only orders in a single signed request
// via MGBX's batch endpoint. Implementing this makes venue.CapabilitiesOf derive
// NativeBatchPlace=true, so the OMS submits the whole slice at once. Every order
// carries timeInForce=GTX to preserve the maker-only guarantee exactly like
// PlacePostOnly. Results keep request order; an item the exchange did not confirm
// gets an empty OrderID so the OMS treats the batch as partially unknown and
// reconciles it on the next cycle.
func (c *Client) PlacePostOnlyBatch(ctx context.Context, requests []venue.PlaceRequest) ([]domain.Order, error) {
	orders := make([]domain.Order, len(requests))
	if len(requests) == 0 {
		return orders, nil
	}
	type orderPayload struct {
		Symbol      string `json:"symbol"`
		Direction   string `json:"direction"`
		TradeType   string `json:"tradeType"`
		TotalAmount string `json:"totalAmount"`
		Price       string `json:"price"`
		TimeInForce string `json:"timeInForce"`
	}
	payload := make([]orderPayload, len(requests))
	for i, request := range requests {
		payload[i] = orderPayload{Symbol: request.Symbol, Direction: string(request.Side), TradeType: "LIMIT", TotalAmount: request.Quantity.String(), Price: request.Price.String(), TimeInForce: "GTX"}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return orders, err
	}
	data, err := c.do(ctx, http.MethodPost, "/spot/v1/u/trade/order/batch/create", url.Values{"ordersJsonStr": []string{string(encoded)}}, true)
	if err != nil {
		return orders, err
	}
	var results []struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &results); err != nil {
		return orders, fmt.Errorf("decode MGBX batch order results: %w", err)
	}
	for i, request := range requests {
		orders[i] = domain.Order{Venue: c.name, Symbol: request.Symbol, Side: request.Side, Price: request.Price, Quantity: request.Quantity, State: domain.OrderUnknown, CreatedAt: time.Now().UTC()}
		if i < len(results) && results[i].Code == 0 {
			var orderID string
			if err := json.Unmarshal(results[i].Data, &orderID); err == nil {
				orders[i].OrderID = orderID
			}
		}
	}
	return orders, nil
}

func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	_, err := c.do(ctx, http.MethodPost, "/spot/v1/u/trade/order/cancel", url.Values{"orderId": []string{orderID}}, true)
	return err
}

func (c *Client) CancelOrders(ctx context.Context, _ string, orderIDs []string) error {
	for start := 0; start < len(orderIDs); start += 20 {
		end := min(start+20, len(orderIDs))
		encoded, err := json.Marshal(orderIDs[start:end])
		if err != nil {
			return err
		}
		if _, err := c.do(ctx, http.MethodPost, "/spot/v1/u/trade/order/batch/cancel", url.Values{"orderIdsJson": []string{string(encoded)}}, true); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, values url.Values, authenticated bool) (json.RawMessage, error) {
	values = cloneValues(values)
	endpoint := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if authenticated {
		if c.apiKey == "" || c.secret == "" {
			return nil, fmt.Errorf("MGBX credentials are not configured")
		}
		timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
		canonical := signaturePayload(values, timestamp)
		mac := hmac.New(sha256.New, []byte(c.secret))
		mac.Write([]byte(canonical))
		nonce, err := randomNonce()
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Access-Key", c.apiKey)
		req.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
		req.Header.Set("X-Request-Timestamp", timestamp)
		req.Header.Set("X-Request-Nonce", nonce)
	}
	if encoded := values.Encode(); encoded != "" {
		req.URL.RawQuery = encoded
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("MGBX http %d: %s", resp.StatusCode, string(body))
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode MGBX response: %w", err)
	}
	if env.Code != 0 {
		message := env.Msg
		if message == "" {
			message = env.Message
		}
		return nil, fmt.Errorf("MGBX code %d: %s", env.Code, message)
	}
	return env.Data, nil
}

// signaturePayload follows the exchange's TreeMap examples byte for byte:
// values are sorted by key but remain raw while signing (JSON brackets, quotes
// and spaces must not be query-escaped), and the separator before timestamp is
// retained even when there are no ordinary parameters. URL encoding happens
// later and affects only the request URI, not the HMAC input.
func signaturePayload(values url.Values, timestamp string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(values))
	for _, key := range keys {
		for _, value := range values[key] {
			parts = append(parts, key+"="+value)
		}
	}
	return strings.Join(parts, "&") + "&timestamp=" + timestamp
}

func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func cloneValues(v url.Values) url.Values {
	result := make(url.Values, len(v))
	for key, values := range v {
		result[key] = append([]string(nil), values...)
	}
	return result
}
