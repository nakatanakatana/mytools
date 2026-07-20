package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBytes = 4 << 20
	maxPages         = 100
)

type TokenSource interface {
	Token(context.Context) (Token, error)
}

type ClientOptions struct {
	BaseURL    string
	Tokens     TokenSource
	HTTPClient *http.Client
	MaxRetries int
	Sleep      func(context.Context, time.Duration) error
	Now        func() time.Time
}

type Client struct {
	base       *url.URL
	tokens     TokenSource
	http       *http.Client
	maxRetries int
	sleep      func(context.Context, time.Duration) error
	now        func() time.Time
}

func NewClient(o ClientOptions) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(o.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, errors.New("invalid Mastodon base URL")
	}
	if o.Tokens == nil {
		return nil, errors.New("mastodon token source is required")
	}
	base.Path, base.RawPath, base.RawQuery, base.Fragment = "", "", "", ""
	if o.HTTPClient == nil {
		o.HTTPClient = http.DefaultClient
	}
	if o.MaxRetries < 0 {
		return nil, errors.New("mastodon retry count cannot be negative")
	}
	if o.MaxRetries == 0 {
		o.MaxRetries = 2
	}
	if o.Sleep == nil {
		o.Sleep = sleepContext
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Client{base: base, tokens: o.Tokens, http: o.HTTPClient, maxRetries: o.MaxRetries, sleep: o.Sleep, now: o.Now}, nil
}

func (c *Client) Account(ctx context.Context) (Account, error) {
	var result Account
	err := c.getOne(ctx, "/api/v1/accounts/verify_credentials", &result)
	return result, err
}
func (c *Client) Following(ctx context.Context, accountID string) ([]Account, error) {
	var result []Account
	err := c.getPages(ctx, "/api/v1/accounts/"+url.PathEscape(accountID)+"/following", &result)
	return result, err
}
func (c *Client) Lists(ctx context.Context) ([]List, error) {
	var result []List
	err := c.getPages(ctx, "/api/v1/lists", &result)
	return result, err
}
func (c *Client) ListAccounts(ctx context.Context, listID string) ([]Account, error) {
	var result []Account
	err := c.getPages(ctx, "/api/v1/lists/"+url.PathEscape(listID)+"/accounts", &result)
	return result, err
}
func (c *Client) HomeTimeline(ctx context.Context) ([]Status, error) {
	var result []Status
	err := c.getPages(ctx, "/api/v1/timelines/home", &result)
	return result, err
}
func (c *Client) ListTimeline(ctx context.Context, listID string) ([]Status, error) {
	var result []Status
	err := c.getPages(ctx, "/api/v1/timelines/list/"+url.PathEscape(listID), &result)
	return result, err
}

// HomeTimelineSince reads the authenticated home timeline after a durable ID.
func (c *Client) HomeTimelineSince(ctx context.Context, sinceID string, limit int) ([]Status, error) {
	page, err := c.HomeTimelinePage(ctx, sinceID, "", limit)
	return page.Statuses, err
}

// ListTimelineSince reads one configured list timeline after a durable ID.
func (c *Client) ListTimelineSince(ctx context.Context, listID, sinceID string, limit int) ([]Status, error) {
	page, err := c.ListTimelinePage(ctx, listID, sinceID, "", limit)
	return page.Statuses, err
}

func (c *Client) HomeTimelinePage(ctx context.Context, sinceID, maxID string, limit int) (TimelinePage, error) {
	return c.statusTimelinePage(ctx, "/api/v1/timelines/home", sinceID, maxID, limit)
}
func (c *Client) ListTimelinePage(ctx context.Context, listID, sinceID, maxID string, limit int) (TimelinePage, error) {
	return c.statusTimelinePage(ctx, "/api/v1/timelines/list/"+url.PathEscape(listID), sinceID, maxID, limit)
}
func (c *Client) statusTimelinePage(ctx context.Context, path, sinceID, maxID string, limit int) (TimelinePage, error) {
	u := c.base.ResolveReference(&url.URL{Path: path})
	q := u.Query()
	if strings.TrimSpace(sinceID) != "" {
		q.Set("since_id", sinceID)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(min(limit, 40)))
	}
	if strings.TrimSpace(maxID) != "" {
		q.Set("max_id", maxID)
	}
	u.RawQuery = q.Encode()
	var values []Status
	link, err := c.getJSONPage(ctx, u, &values)
	if err != nil {
		return TimelinePage{}, err
	}
	next, err := c.nextURL(link)
	if err != nil {
		return TimelinePage{}, err
	}
	nextID := ""
	if next != nil {
		nextID = next.Query().Get("max_id")
	}
	if limit > 0 && len(values) > limit {
		values = values[:limit]
	}
	return TimelinePage{Statuses: values, NextMaxID: nextID}, nil
}

func (c *Client) getOne(ctx context.Context, path string, target any) error {
	u := c.base.ResolveReference(&url.URL{Path: path})
	return c.getJSON(ctx, u, target)
}

func getPage[T any](c *Client, ctx context.Context, path string, result *[]T) error {
	next := c.base.ResolveReference(&url.URL{Path: path})
	for page := 0; next != nil; page++ {
		if page >= maxPages {
			return errors.New("mastodon pagination exceeded limit")
		}
		var values []T
		link, err := c.getJSONPage(ctx, next, &values)
		if err != nil {
			return err
		}
		*result = append(*result, values...)
		next, err = c.nextURL(link)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) getPages(ctx context.Context, path string, target any) error {
	switch result := target.(type) {
	case *[]Account:
		return getPage(c, ctx, path, result)
	case *[]List:
		return getPage(c, ctx, path, result)
	case *[]Status:
		return getPage(c, ctx, path, result)
	default:
		return errors.New("unsupported Mastodon page type")
	}
}

func (c *Client) getJSON(ctx context.Context, u *url.URL, target any) error {
	_, err := c.getJSONPage(ctx, u, target)
	return err
}

func (c *Client) getJSONPage(ctx context.Context, u *url.URL, target any) (string, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.tokens.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("load Mastodon access token: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return "", errors.New("create Mastodon request")
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		req.Header.Set("Accept", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			if attempt < c.maxRetries {
				if err := c.sleep(ctx, retryDelay(attempt)); err != nil {
					return "", err
				}
				continue
			}
			return "", errors.New("mastodon request failed")
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			decodeErr := decodeResponse(resp.Body, target)
			_ = resp.Body.Close()
			if decodeErr != nil {
				return "", decodeErr
			}
			return strings.Join(resp.Header.Values("Link"), ","), nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
		if isTransient(resp.StatusCode) && attempt < c.maxRetries {
			delay := c.retryAfter(resp.Header.Get("Retry-After"), attempt)
			if err := c.sleep(ctx, delay); err != nil {
				return "", err
			}
			continue
		}
		return "", fmt.Errorf("mastodon request rejected with status %d", resp.StatusCode)
	}
}

func decodeResponse(body io.Reader, target any) error {
	data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return errors.New("invalid Mastodon response")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return errors.New("invalid Mastodon response")
	}
	return nil
}
func isTransient(status int) bool          { return status == http.StatusTooManyRequests || status >= 500 }
func retryDelay(attempt int) time.Duration { return time.Duration(1<<min(attempt, 5)) * time.Second }
func (c *Client) retryAfter(raw string, attempt int) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil && at.After(c.now()) {
		return at.Sub(c.now())
	}
	return retryDelay(attempt)
}
func sleepContext(ctx context.Context, delay time.Duration) error {
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) nextURL(link string) (*url.URL, error) {
	raw := nextLink(link)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid Mastodon pagination link")
	}
	u = c.base.ResolveReference(u)
	if !sameOrigin(c.base, u) || u.User != nil {
		return nil, errors.New("cross-origin Mastodon pagination link")
	}
	return u, nil
}
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
func nextLink(header string) string {
	for _, part := range splitLinks(header) {
		left, right := strings.IndexByte(part, '<'), strings.IndexByte(part, '>')
		if left < 0 || right <= left {
			continue
		}
		for _, param := range strings.Split(part[right+1:], ";") {
			pieces := strings.SplitN(strings.TrimSpace(param), "=", 2)
			if len(pieces) == 2 && strings.EqualFold(pieces[0], "rel") {
				for _, rel := range strings.Fields(strings.Trim(pieces[1], `"`)) {
					if strings.EqualFold(rel, "next") {
						return part[left+1 : right]
					}
				}
			}
		}
	}
	return ""
}
func splitLinks(header string) []string {
	var out []string
	start := 0
	quoted, angled := false, false
	for i, r := range header {
		switch r {
		case '"':
			if !angled {
				quoted = !quoted
			}
		case '<':
			if !quoted {
				angled = true
			}
		case '>':
			if !quoted {
				angled = false
			}
		case ',':
			if !quoted && !angled {
				out = append(out, strings.TrimSpace(header[start:i]))
				start = i + 1
			}
		}
	}
	if strings.TrimSpace(header[start:]) != "" {
		out = append(out, strings.TrimSpace(header[start:]))
	}
	return out
}
