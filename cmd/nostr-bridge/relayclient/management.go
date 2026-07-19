package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"fiatjaf.com/nostr"
)

const (
	managementMediaType     = "application/nostr+json+rpc"
	managementResponseLimit = 64 << 10
)

type ManagementClient interface {
	AllowPubKey(context.Context, nostr.PubKey, string) error
	UnallowPubKey(context.Context, nostr.PubKey, string) error
}

type HTTPManagementClient struct {
	Endpoint   *url.URL
	SigningURL *url.URL
	AdminKey   nostr.SecretKey
	HTTPClient *http.Client
	Now        func() time.Time
}

func NewHTTPManagementClient(endpoint, signingURL *url.URL, adminKey nostr.SecretKey) (*HTTPManagementClient, error) {
	client := &HTTPManagementClient{Endpoint: endpoint, SigningURL: signingURL, AdminKey: adminKey}
	if err := client.validate(); err != nil {
		return nil, err
	}
	return client, nil
}

type managementRequest struct {
	Method string   `json:"method"`
	Params []string `json:"params"`
}

type managementResponse struct {
	Result *bool  `json:"result"`
	Error  string `json:"error"`
}

func (c *HTTPManagementClient) AllowPubKey(ctx context.Context, pubkey nostr.PubKey, reason string) error {
	if !validPubKey(pubkey) {
		return errors.New("invalid relay management pubkey")
	}
	params := []string{pubkey.Hex()}
	if reason != "" {
		params = append(params, reason)
	}
	return c.call(ctx, "allowpubkey", params)
}

func (c *HTTPManagementClient) UnallowPubKey(ctx context.Context, pubkey nostr.PubKey, reason string) error {
	if !validPubKey(pubkey) {
		return errors.New("invalid relay management pubkey")
	}
	params := []string{pubkey.Hex()}
	if reason != "" {
		params = append(params, reason)
	}
	return c.call(ctx, "unallowpubkey", params)
}

func validPubKey(pubkey nostr.PubKey) bool {
	_, err := nostr.PubKeyFromHex(pubkey.Hex())
	return err == nil
}

func (c *HTTPManagementClient) call(ctx context.Context, method string, params []string) error {
	if err := c.validate(); err != nil {
		return err
	}
	signingURL := c.SigningURL
	if signingURL == nil {
		signingURL = c.Endpoint
	}
	endpoint := c.Endpoint.String()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	body, err := json.Marshal(managementRequest{Method: method, Params: params})
	if err != nil {
		return errors.New("encode relay management request")
	}
	authorization, err := signNIP98(signingURL.String(), http.MethodPost, body, c.AdminKey, now())
	if err != nil {
		return fmt.Errorf("prepare relay management request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("prepare relay management request")
	}
	req.Header.Set("Content-Type", managementMediaType)
	req.Header.Set("Authorization", authorization)
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return errors.New("relay management request failed")
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, managementResponseLimit+1))
	if err != nil {
		return errors.New("read relay management response")
	}
	if len(responseBody) > managementResponseLimit {
		return errors.New("relay management response too large")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("relay management returned status %d", response.StatusCode)
	}
	var result managementResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return errors.New("decode relay management response")
	}
	if result.Error != "" {
		return errors.New("relay management error")
	}
	if result.Result == nil || !*result.Result {
		return errors.New("invalid response from relay management")
	}
	return nil
}

func (c *HTTPManagementClient) validate() error {
	if c == nil || !validManagementURL(c.Endpoint) {
		return errors.New("invalid relay management configuration")
	}
	signingURL := c.SigningURL
	if signingURL == nil {
		signingURL = c.Endpoint
	}
	if !validManagementURL(signingURL) || c.Endpoint.EscapedPath() != signingURL.EscapedPath() || c.Endpoint.RawQuery != signingURL.RawQuery {
		return errors.New("invalid relay management signing configuration")
	}
	return nil
}

func validManagementURL(value *url.URL) bool {
	return value != nil && (value.Scheme == "http" || value.Scheme == "https") && value.Host != "" && value.User == nil && value.Fragment == ""
}
