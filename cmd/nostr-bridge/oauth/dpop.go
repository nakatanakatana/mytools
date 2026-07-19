package oauth

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

const dpopErrorBodyLimit = 4096

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for name, entries := range values {
		cloned[name] = append([]string(nil), entries...)
	}
	return cloned
}

func (c *Client) doDPoPFormRequest(
	ctx context.Context,
	operation string,
	endpoint string,
	key *ecdsa.PrivateKey,
	nonce string,
	form func() (url.Values, error),
) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		values, err := form()
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
		if err != nil {
			return nil, fmt.Errorf("create %s request: %w", operation, err)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		proof, err := dpopProof(key, http.MethodPost, request.URL, nonce)
		if err != nil {
			return nil, err
		}
		request.Header.Set("DPoP", proof)
		response, err := c.httpClient.Do(request)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", operation, err)
		}
		if response.StatusCode != http.StatusBadRequest {
			return response, nil
		}

		body, readErr := io.ReadAll(io.LimitReader(response.Body, dpopErrorBodyLimit+1))
		_ = response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(body))
		if readErr != nil {
			return response, nil
		}
		var oauthError struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &oauthError)
		nonces := response.Header.Values("DPoP-Nonce")
		headerPresent := len(nonces) > 0
		log.Printf("%s: DPoP-Nonce header present=%t", operation, headerPresent)
		validNonce := len(nonces) == 1 && strings.TrimSpace(nonces[0]) != ""
		if attempt != 0 || oauthError.Error != "use_dpop_nonce" || !validNonce {
			return response, nil
		}
		nonce = nonces[0]
	}
	panic("unreachable")
}
