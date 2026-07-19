// Package bluesky provides authenticated reads from the Bluesky API.
package bluesky

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	bridgeoauth "github.com/nakatanakatana/mytools/cmd/nostr-bridge/oauth"
)

// SourceClient is the Bluesky read API needed by the bridge.
type SourceClient interface {
	Timeline(context.Context, string, int) (Page, error)
	Follows(context.Context) ([]Actor, error)
	List(context.Context, string) (List, error)
	Profile(context.Context, string) (Profile, error)
}

// ClientOptions configures an OAuth-authenticated Bluesky API client.
type ClientOptions struct {
	HTTPClient *http.Client
	BaseURL    string
	Token      bridgeoauth.Token
	AccountDID string
}

// Client reads Bluesky XRPC endpoints on behalf of one authenticated account.
type Client struct {
	httpClient  *http.Client
	baseURL     string
	accessToken string
	dpopKey     *ecdsa.PrivateKey
	dpopNonce   string
	accountDID  string
}

// Actor identifies a Bluesky account.
type Actor struct {
	DID string `json:"did"`
}

// List is a Bluesky list and its members.
type List struct {
	URI         string
	Name        string
	Description string
	Members     []Actor
}

// Profile is the Bluesky profile information required by the bridge.
type Profile struct {
	DID         string `json:"did"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Avatar      string `json:"avatar"`
}

// Post is the minimal timeline post representation needed by later sync stages.
type Post struct {
	URI        string    `json:"uri"`
	CID        string    `json:"cid"`
	AuthorDID  string    `json:"-"`
	Text       string    `json:"-"`
	CreatedAt  time.Time `json:"-"`
	ReplyToURI string    `json:"-"`
}

// Page is one page of timeline results.
type Page struct {
	Posts  []Post
	Cursor string
}

// NewClient returns a Bluesky client using a DPoP-bound OAuth access token.
func NewClient(options ClientOptions) (*Client, error) {
	if strings.TrimSpace(options.BaseURL) == "" || strings.TrimSpace(options.Token.AccessToken) == "" || options.Token.DPoPKey == nil || strings.TrimSpace(options.AccountDID) == "" {
		return nil, errors.New("Bluesky client requires base URL, DPoP-bound access token, and account DID") //nolint:staticcheck // Bluesky is a product name.
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	return &Client{httpClient: options.HTTPClient, baseURL: strings.TrimRight(options.BaseURL, "/"), accessToken: options.Token.AccessToken, dpopKey: options.Token.DPoPKey, dpopNonce: options.Token.DPoPNonce, accountDID: options.AccountDID}, nil
}

// Timeline returns one authenticated timeline page.
func (c *Client) Timeline(ctx context.Context, cursor string, limit int) (Page, error) {
	query := url.Values{}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", limit))
	}
	var response struct {
		Cursor string `json:"cursor"`
		Feed   []struct {
			Post struct {
				URI    string `json:"uri"`
				CID    string `json:"cid"`
				Author Actor  `json:"author"`
				Record struct {
					Text      string    `json:"text"`
					CreatedAt time.Time `json:"createdAt"`
					Reply     *struct {
						Parent struct {
							URI string `json:"uri"`
						} `json:"parent"`
					} `json:"reply"`
				} `json:"record"`
			} `json:"post"`
		} `json:"feed"`
	}
	if err := c.get(ctx, "app.bsky.feed.getTimeline", query, &response); err != nil {
		return Page{}, err
	}
	page := Page{Cursor: response.Cursor, Posts: make([]Post, 0, len(response.Feed))}
	for _, item := range response.Feed {
		post := Post{URI: item.Post.URI, CID: item.Post.CID, AuthorDID: item.Post.Author.DID, Text: item.Post.Record.Text, CreatedAt: item.Post.Record.CreatedAt}
		if item.Post.Record.Reply != nil {
			post.ReplyToURI = item.Post.Record.Reply.Parent.URI
		}
		page.Posts = append(page.Posts, post)
	}
	return page, nil
}

// Follows returns every account actually followed by the authenticated account.
func (c *Client) Follows(ctx context.Context) ([]Actor, error) {
	return c.actors(ctx, "app.bsky.graph.getFollows", "follows", url.Values{"actor": {c.accountDID}})
}

// List returns the requested list's stable metadata and every member.
func (c *Client) List(ctx context.Context, listURI string) (List, error) {
	list := List{URI: listURI}
	query := url.Values{"list": {listURI}}
	for {
		var response struct {
			Cursor string `json:"cursor"`
			List   struct {
				URI         string `json:"uri"`
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"list"`
			Items []struct {
				Subject Actor `json:"subject"`
			} `json:"items"`
		}
		if err := c.get(ctx, "app.bsky.graph.getList", query, &response); err != nil {
			return List{}, err
		}
		if response.List.URI != "" {
			list.URI = response.List.URI
		}
		list.Name = response.List.Name
		list.Description = response.List.Description
		for _, item := range response.Items {
			list.Members = append(list.Members, item.Subject)
		}
		if response.Cursor == "" {
			return list, nil
		}
		query.Set("cursor", response.Cursor)
	}
}

// Profile returns one Bluesky profile.
func (c *Client) Profile(ctx context.Context, did string) (Profile, error) {
	var profile Profile
	if err := c.get(ctx, "app.bsky.actor.getProfile", url.Values{"actor": {did}}, &profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func (c *Client) actors(ctx context.Context, endpoint, field string, query url.Values) ([]Actor, error) {
	actors := make([]Actor, 0)
	for {
		var response struct {
			Cursor  string  `json:"cursor"`
			Follows []Actor `json:"follows"`
			Items   []struct {
				Subject Actor `json:"subject"`
			} `json:"items"`
		}
		if err := c.get(ctx, endpoint, query, &response); err != nil {
			return nil, err
		}
		if field == "follows" {
			actors = append(actors, response.Follows...)
		} else {
			for _, item := range response.Items {
				actors = append(actors, item.Subject)
			}
		}
		if response.Cursor == "" {
			return actors, nil
		}
		query.Set("cursor", response.Cursor)
	}
}

func (c *Client) get(ctx context.Context, endpoint string, query url.Values, destination any) error {
	response, err := c.doGet(ctx, endpoint, query)
	if err != nil {
		return err
	}
	if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusBadRequest) && response.Header.Get("DPoP-Nonce") != "" {
		c.dpopNonce = response.Header.Get("DPoP-Nonce")
		_ = response.Body.Close()
		response, err = c.doGet(ctx, endpoint, query)
		if err != nil {
			return err
		}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("request Bluesky %s: unexpected status %s", endpoint, response.Status)
	}
	if nonce := response.Header.Get("DPoP-Nonce"); nonce != "" {
		c.dpopNonce = nonce
	}
	if err := json.NewDecoder(response.Body).Decode(destination); err != nil {
		return fmt.Errorf("decode Bluesky %s response: %w", endpoint, err)
	}
	return nil
}

func (c *Client) doGet(ctx context.Context, endpoint string, query url.Values) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/xrpc/"+endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Bluesky %s request: %w", endpoint, err)
	}
	proof, err := dpopProof(c.dpopKey, request.Method, request.URL, c.dpopNonce)
	if err != nil {
		return nil, fmt.Errorf("create Bluesky DPoP proof: %w", err)
	}
	request.Header.Set("Authorization", "DPoP "+c.accessToken)
	request.Header.Set("DPoP", proof)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request Bluesky %s: %w", endpoint, err)
	}
	return response, nil
}

func dpopProof(key *ecdsa.PrivateKey, method string, requestURL *url.URL, nonce string) (string, error) {
	htu := *requestURL
	htu.RawQuery = ""
	htu.Fragment = ""
	claims := map[string]any{"jti": randomString(16), "htm": method, "htu": htu.String(), "iat": time.Now().Unix()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	return signJWT(key, claims, map[string]any{"typ": "dpop+jwt", "jwk": publicJWK(&key.PublicKey)})
}

func randomString(size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func publicJWK(key *ecdsa.PublicKey) map[string]string {
	encoded, err := key.Bytes()
	if err != nil || len(encoded) != 65 {
		panic("invalid internal P-256 public key")
	}
	return map[string]string{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(encoded[1:33]), "y": base64.RawURLEncoding.EncodeToString(encoded[33:])}
}

func signJWT(key *ecdsa.PrivateKey, claims map[string]any, header map[string]any) (string, error) {
	header["alg"] = "ES256"
	encodedHeader, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(encodedHeader) + "." + base64.RawURLEncoding.EncodeToString(encodedClaims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
