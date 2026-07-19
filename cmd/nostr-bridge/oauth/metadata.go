package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

const metadataBodyLimit = 32 << 10

type authorizationServerMetadata struct {
	Issuer                             string `json:"issuer"`
	AuthorizationEndpoint              string `json:"authorization_endpoint"`
	TokenEndpoint                      string `json:"token_endpoint"`
	PushedAuthorizationRequestEndpoint string `json:"pushed_authorization_request_endpoint"`
	RequirePushedAuthorizationRequests bool   `json:"require_pushed_authorization_requests"`
}

func (c *Client) discoverAuthorizationServer(ctx context.Context, issuer string) (authorizationServerMetadata, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return authorizationServerMetadata{}, fmt.Errorf("create authorization server metadata request: %w", err)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return authorizationServerMetadata{}, fmt.Errorf("fetch authorization server metadata: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return authorizationServerMetadata{}, fmt.Errorf("authorization server metadata: status %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return authorizationServerMetadata{}, errors.New("authorization server metadata has invalid Content-Type")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, metadataBodyLimit+1))
	if err != nil {
		return authorizationServerMetadata{}, fmt.Errorf("read authorization server metadata: %w", err)
	}
	if len(body) > metadataBodyLimit {
		return authorizationServerMetadata{}, errors.New("authorization server metadata is too large")
	}

	var metadata authorizationServerMetadata
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&metadata); err != nil {
		return authorizationServerMetadata{}, fmt.Errorf("decode authorization server metadata: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return authorizationServerMetadata{}, errors.New("authorization server metadata has trailing JSON")
	}
	if metadata.Issuer != issuer {
		return authorizationServerMetadata{}, errors.New("authorization server metadata issuer mismatch")
	}
	for _, endpoint := range []struct {
		name  string
		value string
	}{
		{"authorization_endpoint", metadata.AuthorizationEndpoint},
		{"token_endpoint", metadata.TokenEndpoint},
		{"pushed_authorization_request_endpoint", metadata.PushedAuthorizationRequestEndpoint},
	} {
		if err := validateOAuthEndpoint(endpoint.name, endpoint.value); err != nil {
			return authorizationServerMetadata{}, err
		}
	}
	if !metadata.RequirePushedAuthorizationRequests {
		return authorizationServerMetadata{}, errors.New("authorization server metadata does not require_pushed_authorization_requests")
	}
	return metadata, nil
}

func validateOAuthEndpoint(name, value string) error {
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return fmt.Errorf("authorization server metadata has invalid %s", name)
	}
	return nil
}
