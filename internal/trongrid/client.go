package trongrid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL, apiKey string
	http            *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, http: &http.Client{Timeout: 20 * time.Second}}
}

type TRC20Transfer struct {
	TransactionID string `json:"transaction_id"`
	TokenInfo     struct {
		Symbol   string `json:"symbol"`
		Address  string `json:"address"`
		Decimals int    `json:"decimals"`
	} `json:"token_info"`
	BlockTimestamp int64  `json:"block_timestamp"`
	From           string `json:"from"`
	To             string `json:"to"`
	Type           string `json:"type"`
	Value          string `json:"value"`
}
type trc20Resp struct {
	Data    []TRC20Transfer `json:"data"`
	Success bool            `json:"success"`
	Meta    struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"meta"`
}

func (c *Client) AccountTRC20(ctx context.Context, address, contract string, onlyConfirmed bool, minTimestamp int64) ([]TRC20Transfer, error) {
	u, err := url.Parse(c.baseURL + "/v1/accounts/" + url.PathEscape(address) + "/transactions/trc20")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("limit", "200")
	q.Set("only_to", "true")
	q.Set("order_by", "block_timestamp,asc")
	if contract != "" {
		q.Set("contract_address", contract)
	}
	if onlyConfirmed {
		q.Set("only_confirmed", "true")
	}
	if minTimestamp > 0 {
		q.Set("min_timestamp", fmt.Sprint(minTimestamp))
	}
	q.Set("max_timestamp", fmt.Sprint(time.Now().UnixMilli()))
	var transfers []TRC20Transfer
	seen := map[string]bool{}
	for {
		u.RawQuery = q.Encode()
		out, err := c.fetchPage(ctx, u.String())
		if err != nil {
			return nil, err
		}
		transfers = append(transfers, out.Data...)
		next := out.Meta.Fingerprint
		if next == "" {
			return transfers, nil
		}
		if seen[next] {
			return nil, fmt.Errorf("trongrid repeated pagination cursor")
		}
		seen[next] = true
		q.Set("fingerprint", next)
	}
}

func (c *Client) fetchPage(ctx context.Context, address string) (*trc20Resp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("TRON-PRO-API-KEY", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("trongrid http %d: %s", resp.StatusCode, string(raw))
	}
	var out trc20Resp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if !out.Success {
		return nil, fmt.Errorf("trongrid response success=false")
	}
	return &out, nil
}
