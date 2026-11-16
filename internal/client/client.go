package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"recall-miner/internal/httpx"
)

type Client struct {
	httpClient     *http.Client
	internalAPIKey string
}

func New(httpClient *http.Client, internalAPIKey string) *Client {
	return &Client{httpClient: httpClient, internalAPIKey: internalAPIKey}
}

func (c *Client) RequestJSON(ctx context.Context, method, url string, payload any, out any) error {
	return c.requestJSON(ctx, method, url, payload, out, 0)
}

// RequestJSONLimited bounds the complete success body before decoding, including
// unknown fields and trailing content. Existing RequestJSON callers are unchanged.
func (c *Client) RequestJSONLimited(ctx context.Context, method, url string, payload any, out any, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("response byte limit must be positive")
	}
	return c.requestJSON(ctx, method, url, payload, out, maxBytes)
}
func (c *Client) requestJSON(ctx context.Context, method, url string, payload any, out any, maxBytes int64) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.internalAPIKey != "" {
		request.Header.Set("X-Internal-Api-Key", c.internalAPIKey)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return httpx.StatusError(response.StatusCode, fmt.Sprintf("upstream %s returned %d: %s", url, response.StatusCode, strings.TrimSpace(string(body))))
	}
	if maxBytes > 0 {
		data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
		if err != nil {
			return err
		}
		if int64(len(data)) > maxBytes {
			return fmt.Errorf("upstream response exceeds byte limit")
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(data, out)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}
