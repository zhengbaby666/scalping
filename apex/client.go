package apex

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Client Apex Pro REST 客户端
type Client struct {
	baseURL    string
	apiKey     string
	apiSecret  string
	passphrase string
	httpClient *http.Client
}

// NewClient 创建 REST 客户端
func NewClient(baseURL, apiKey, apiSecret, passphrase string) *Client {
	return &Client{
		baseURL:    baseURL,
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		passphrase: passphrase,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// ---------- 公共数据结构 ----------

// OrderBook 订单簿快照
type OrderBook struct {
	Bids [][]string `json:"bids"` // [[price, size], ...]
	Asks [][]string `json:"asks"`
}

// Position 持仓信息
type Position struct {
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"` // LONG / SHORT
	Size     float64 `json:"size,string"`
	EntryPrice float64 `json:"entryPrice,string"`
	UnrealizedPnl float64 `json:"unrealizedPnl,string"`
}

// Account 账户信息
type Account struct {
	EquityValue    float64 `json:"equityValue,string"`
	AvailableValue float64 `json:"availableValue,string"`
}

// Order 订单信息
type Order struct {
	ID          string  `json:"id"`
	Symbol      string  `json:"symbol"`
	Side        string  `json:"side"`   // BUY / SELL
	Type        string  `json:"type"`   // LIMIT / MARKET
	Price       float64 `json:"price,string"`
	Size        float64 `json:"size,string"`
	FilledSize  float64 `json:"filledSize,string"`
	Status      string  `json:"status"` // OPEN / FILLED / CANCELED
	CreatedAt   int64   `json:"createdAt"`
}

// PlaceOrderReq 下单请求
type PlaceOrderReq struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	Type          string `json:"type"`
	Size          string `json:"size"`
	Price         string `json:"price,omitempty"`
	TimeInForce   string `json:"timeInForce"`   // GTT / IOC / FOK / POST_ONLY
	ReduceOnly    bool   `json:"reduceOnly"`
	ClientOrderID string `json:"clientOrderId,omitempty"`
}

// ---------- 签名工具 ----------

// sign 生成 HMAC-SHA256 签名（Apex Pro 签名规范）
func (c *Client) sign(timestamp, method, path, body string) string {
	message := timestamp + method + path + body
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// request 发送带签名的 HTTP 请求
func (c *Client) request(method, path string, payload interface{}) ([]byte, error) {
	var bodyStr string
	var bodyReader io.Reader

	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		bodyStr = string(b)
		bodyReader = bytes.NewBufferString(bodyStr)
	}

	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig := c.sign(timestamp, method, path, bodyStr)

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("APEX-API-KEY", c.apiKey)
	req.Header.Set("APEX-SIGNATURE", sig)
	req.Header.Set("APEX-TIMESTAMP", timestamp)
	req.Header.Set("APEX-PASSPHRASE", c.passphrase)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))
	}

	return data, nil
}

// ---------- 公开接口 ----------

// GetOrderBook 获取订单簿（公开接口，无需签名）
func (c *Client) GetOrderBook(symbol string) (*OrderBook, error) {
	url := fmt.Sprintf("%s/api/v1/depth?symbol=%s&limit=5", c.baseURL, symbol)
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		Data *OrderBook `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// ---------- 私有接口 ----------

// GetAccount 获取账户信息
func (c *Client) GetAccount() (*Account, error) {
	data, err := c.request("GET", "/api/v1/account", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data *Account `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// GetPositions 获取所有持仓
func (c *Client) GetPositions() ([]Position, error) {
	data, err := c.request("GET", "/api/v1/positions", nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []Position `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// PlaceOrder 下单
func (c *Client) PlaceOrder(req *PlaceOrderReq) (*Order, error) {
	data, err := c.request("POST", "/api/v1/order", req)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data *Order `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

// CancelOrder 撤销单个订单
func (c *Client) CancelOrder(orderID string) error {
	path := fmt.Sprintf("/api/v1/order?id=%s", orderID)
	_, err := c.request("DELETE", path, nil)
	return err
}

// CancelAllOrders 撤销某交易对所有订单
func (c *Client) CancelAllOrders(symbol string) error {
	path := fmt.Sprintf("/api/v1/open-orders?symbol=%s", symbol)
	_, err := c.request("DELETE", path, nil)
	return err
}

// GetOpenOrders 获取当前挂单
func (c *Client) GetOpenOrders(symbol string) ([]Order, error) {
	path := fmt.Sprintf("/api/v1/open-orders?symbol=%s", symbol)
	data, err := c.request("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data []Order `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Data, nil
}
