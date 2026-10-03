package webpush

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrSubscriptionExpired indicates that the push subscription has expired or unsubscribed (HTTP 404 or 410).
var ErrSubscriptionExpired = errors.New("webpush: subscription has expired or is invalid")

// IsSubscriptionExpired returns true if err indicates the subscription has expired or is invalid.
func IsSubscriptionExpired(err error) bool {
	return errors.Is(err, ErrSubscriptionExpired)
}

// IsPermanentPushError reports whether a push service error should not be retried
// while the current issue and subscription remain unchanged.
func IsPermanentPushError(err error) bool {
	serviceError, ok := errors.AsType[interface {
		error
		Permanent() bool
	}](err)
	return ok && serviceError.Permanent()
}

const (
	// DefaultTTL is the default time-to-live for a push message in seconds (24 hours).
	DefaultTTL = 86400

	// Urgency levels according to RFC 8030 Section 5.3.
	UrgencyVeryLow = "very-low"
	UrgencyLow     = "low"
	UrgencyNormal  = "normal"
	UrgencyHigh    = "high"

	defaultHTTPTimeout = 15 * time.Second
)

// ClientOptions configures a Web Push Client.
type ClientOptions struct {
	HTTPClient *http.Client
	VAPIDKeys  VAPIDKeys
	Subject    string
}

// Options specifies delivery options for a Web Push message.
type Options struct {
	TTL     int
	Urgency string
	Topic   string
}

// Client represents a Web Push HTTP delivery client.
type Client struct {
	httpClient *http.Client
	vapidKeys  VAPIDKeys
	subject    string
}

func defaultCheckRedirect(req *http.Request, via []*http.Request) error {
	return errors.New("webpush: redirects are not allowed")
}

var disallowedPrefixes = []netip.Prefix{
	// IPv4 reserved ranges
	netip.MustParsePrefix("0.0.0.0/8"),       // Current network / loopback alias on Linux
	netip.MustParsePrefix("100.64.0.0/10"),   // Shared address space / CGNAT (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF Protocol Assignments (RFC 6890)
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 (RFC 5737)
	netip.MustParsePrefix("198.18.0.0/15"),   // Benchmark testing (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 (RFC 5737)
	netip.MustParsePrefix("240.0.0.0/4"),     // Reserved / future use, includes broadcast (RFC 1112)

	// IPv6 reserved / transition ranges
	netip.MustParsePrefix("::/96"),           // Deprecated IPv4-compatible (RFC 4291)
	netip.MustParsePrefix("64:ff9b::/96"),    // IPv4-IPv6 translation (RFC 6052)
	netip.MustParsePrefix("2001::/32"),       // Teredo tunneling (RFC 4380)
	netip.MustParsePrefix("2001:2::/48"),     // Benchmarking (RFC 5180)
	netip.MustParsePrefix("2002::/16"),       // 6to4 (RFC 3056)
	netip.MustParsePrefix("3ffe::/16"),       // 6bone (RFC 3701)
}

// IsDisallowedIP returns true if ip is loopback, private, link-local, multicast,
// unspecified, CGNAT, benchmark, or a reserved IPv4/IPv6 transition address that
// should not be contacted as a Web Push endpoint.
func IsDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	addr = addr.Unmap()

	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return true
	}

	return slices.ContainsFunc(disallowedPrefixes, func(prefix netip.Prefix) bool {
		return prefix.Contains(addr)
	})
}

func newDefaultHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("webpush: invalid dial address: %w", err)
			}
			ip := net.ParseIP(host)
			if IsDisallowedIP(ip) {
				return fmt.Errorf("webpush: connection to disallowed IP address %s is blocked", host)
			}
			return nil
		},
	}

	transport := &http.Transport{
		Proxy:                 nil, // Push services are public; explicitly disable proxy to prevent SSRF bypass.
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Timeout:       defaultHTTPTimeout,
		Transport:     transport,
		CheckRedirect: defaultCheckRedirect,
	}
}

// NormalizeTopic sanitizes a topic string according to RFC 8030 Section 5.4.
// It replaces invalid characters with '-', ensures the length does not exceed 32 octets,
// and appends a short SHA-256 hash suffix when truncating to avoid collision.
func NormalizeTopic(topic string) string {
	if topic == "" {
		return ""
	}
	sanitized := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, topic)
	if len(sanitized) <= 32 {
		return sanitized
	}
	h := sha256.Sum256([]byte(topic))
	return fmt.Sprintf("%s-%x", sanitized[:23], h[:4])
}

// NewClient creates a new Web Push client with the given options.
// If opts.HTTPClient is nil, a secure http.Client with a default 15s timeout,
// SSRF protection, and redirect blocking is used.
func NewClient(opts ClientOptions) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = newDefaultHTTPClient()
	}
	return &Client{
		httpClient: hc,
		vapidKeys:  opts.VAPIDKeys,
		subject:    opts.Subject,
	}
}

// Send encrypts the payload and delivers it to the subscription endpoint via Web Push protocol.
func (c *Client) Send(ctx context.Context, sub Subscription, payload []byte, opts Options) error {
	if c.vapidKeys.PublicKey == "" || c.vapidKeys.PrivateKey == "" {
		return errors.New("webpush: vapid keys are required for push delivery")
	}

	u, err := url.ParseRequestURI(sub.Endpoint)
	if err != nil {
		return &redactedError{message: "webpush: invalid endpoint URL", err: err}
	}
	if u.Host == "" {
		return errors.New("webpush: invalid endpoint URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("webpush: invalid endpoint scheme %q", u.Scheme)
	}
	if u.User != nil {
		return errors.New("webpush: endpoint must not contain user credentials")
	}

	encrypted, err := Encrypt(payload, sub)
	if err != nil {
		return fmt.Errorf("encrypt payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(encrypted))
	if err != nil {
		return &redactedError{message: "webpush: create push request failed", err: err}
	}

	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")

	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	ttl = min(ttl, 2419200) // Max TTL: 4 weeks (RFC 8030)
	req.Header.Set("TTL", strconv.Itoa(ttl))

	urgency := opts.Urgency
	switch urgency {
	case UrgencyVeryLow, UrgencyLow, UrgencyNormal, UrgencyHigh:
	default:
		urgency = UrgencyHigh
	}
	req.Header.Set("Urgency", urgency)

	if topic := NormalizeTopic(opts.Topic); topic != "" {
		req.Header.Set("Topic", topic)
	}

	vapidHeader, err := BuildVAPIDHeader(sub.Endpoint, c.subject, &c.vapidKeys, time.Now().Add(12*time.Hour))
	if err != nil {
		return fmt.Errorf("build vapid header: %w", err)
	}
	req.Header.Set("Authorization", vapidHeader)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &redactedError{message: "webpush: push transport failed (details redacted)", err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil

	case http.StatusNotFound, http.StatusGone:
		return fmt.Errorf("%w: status %d", ErrSubscriptionExpired, resp.StatusCode)

	default:
		return &pushServiceError{
			statusCode: resp.StatusCode,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
	}
}

type pushServiceError struct {
	statusCode int
	retryAfter time.Duration
}

func (e *pushServiceError) Error() string {
	return fmt.Sprintf("webpush: push service returned status %d (%s)", e.statusCode, http.StatusText(e.statusCode))
}

func (e *pushServiceError) RetryAfter() time.Duration {
	return e.retryAfter
}

func (e *pushServiceError) Permanent() bool {
	return e.statusCode >= 400 && e.statusCode < 500 &&
		e.statusCode != http.StatusRequestTimeout &&
		e.statusCode != http.StatusTooEarly &&
		e.statusCode != http.StatusTooManyRequests
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, 24*time.Hour)
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		return min(retryAt.Sub(now), 24*time.Hour)
	}
	return 0
}

type redactedError struct {
	message string
	err     error
}

func (e *redactedError) Error() string {
	return e.message
}

func (e *redactedError) Is(target error) bool {
	return errors.Is(e.err, target)
}
