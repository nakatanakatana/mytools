package oauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	bridgestore "github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
)

// RefreshReason identifies the bounded set of OAuth token write triggers.
type RefreshReason string

const (
	RefreshReasonAuthorizationCode RefreshReason = "authorization_code"
	RefreshReasonOnDemand          RefreshReason = "on_demand"
	RefreshReasonMaintenance       RefreshReason = "maintenance"
)

// RefreshErrorClass identifies a secret-free, bounded category of refresh failure.
type RefreshErrorClass string

const (
	RefreshErrorTimeout             RefreshErrorClass = "timeout"
	RefreshErrorConnection          RefreshErrorClass = "connection"
	RefreshErrorRateLimit           RefreshErrorClass = "rate_limited"
	RefreshErrorServer              RefreshErrorClass = "server"
	RefreshErrorInvalidGrant        RefreshErrorClass = "invalid_grant"
	RefreshErrorMissingRefreshToken RefreshErrorClass = "missing_refresh_token"
	RefreshErrorDecrypt             RefreshErrorClass = "decrypt"
	RefreshErrorDPoPKey             RefreshErrorClass = "dpop_key"
	RefreshErrorProtocol            RefreshErrorClass = "protocol"
)

// Status describes the locally persisted authorization without refreshing it.
type Status struct {
	AccessTokenValid       bool
	AccessTokenExpiry      time.Time
	AuthorizationAvailable bool
	ReauthRequired         bool
	LastRefreshSucceededAt time.Time
	NextMaintenanceRefresh time.Time
	LastRefreshErrorClass  RefreshErrorClass
}

// ClientObserver receives bounded, secret-free token-write and local status
// events from authorization-code and on-demand client operations.
type ClientObserver interface {
	RefreshSucceeded(time.Time, RefreshReason)
	RefreshFailed(time.Time, RefreshReason, RefreshErrorClass)
	AuthorizationStatusChanged(time.Time, Status)
}

// NopClientObserver provides no-op default observer methods.
type NopClientObserver struct{}

func (NopClientObserver) RefreshSucceeded(time.Time, RefreshReason) {}

func (NopClientObserver) RefreshFailed(time.Time, RefreshReason, RefreshErrorClass) {}

func (NopClientObserver) AuthorizationStatusChanged(time.Time, Status) {}

// RefreshResult describes whether a due check performed a refresh.
type RefreshResult struct {
	Reason    RefreshReason
	Refreshed bool
	Token     Token
}

// RefreshError reports a bounded failure class without retaining response
// descriptions, token material, or other secret-bearing causes.
type RefreshError struct {
	Class             RefreshErrorClass
	ReauthRequired    bool
	PersistenceFailed bool
}

func (e *RefreshError) Error() string {
	if e == nil {
		return "OAuth token refresh failed"
	}
	return fmt.Sprintf(
		"OAuth token refresh failed: class=%s reauth_required=%t persistence_failed=%t",
		boundedRefreshErrorClass(e.Class),
		e.ReauthRequired,
		e.PersistenceFailed,
	)
}

// AuthorizationStatus inspects locally persisted authorization without making
// discovery or token endpoint requests and without changing the stored row.
func (c *Client) AuthorizationStatus(ctx context.Context, accountDID string, period time.Duration) (Status, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	stored, err := c.loadTokenLocked(ctx, accountDID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Status{}, nil
		}
		return Status{}, err
	}
	lastRefresh := effectiveLastRefresh(stored)
	status := Status{
		ReauthRequired:         stored.ReauthRequired,
		LastRefreshSucceededAt: lastRefresh,
		LastRefreshErrorClass:  RefreshErrorClass(stored.LastRefreshErrorClass),
	}
	if !lastRefresh.IsZero() {
		status.NextMaintenanceRefresh = lastRefresh.Add(period)
	}

	payload, err := c.decryptTokenPayload(stored)
	if err != nil {
		status.ReauthRequired = true
		status.LastRefreshErrorClass = RefreshErrorDecrypt
		return status, nil
	}
	if _, err := payload.DPoPKey.ecdsa(); err != nil {
		status.ReauthRequired = true
		status.LastRefreshErrorClass = RefreshErrorDPoPKey
		return status, nil
	}
	status.AccessTokenExpiry = payload.Expiry
	status.AccessTokenValid = strings.TrimSpace(payload.AccessToken) != "" && (payload.Expiry.IsZero() || payload.Expiry.After(c.now()))
	if strings.TrimSpace(payload.RefreshToken) == "" {
		status.ReauthRequired = true
		status.LastRefreshErrorClass = RefreshErrorMissingRefreshToken
		return status, nil
	}
	status.AuthorizationAvailable = !status.ReauthRequired
	return status, nil
}

// RefreshIfDue refreshes the persisted authorization when its durable refresh
// origin is at least period old. The row is reloaded while refreshes are
// serialized so concurrent callers cannot rotate the same refresh token.
func (c *Client) RefreshIfDue(ctx context.Context, accountDID string, period time.Duration) (RefreshResult, error) {
	result := RefreshResult{Reason: RefreshReasonMaintenance}
	if c.scope.Provider != "bluesky" || strings.TrimSpace(c.scope.Account) == "" || accountDID != c.scope.Account {
		return result, fmt.Errorf("Bluesky maintenance refresh: %w", bridgestore.ErrSourceScopeMismatch)
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	stored, err := c.loadTokenLocked(ctx, accountDID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, nil
		}
		return result, err
	}
	if stored.ReauthRequired {
		return result, nil
	}
	payload, err := c.decryptTokenPayload(stored)
	if err != nil {
		return result, c.persistRefreshFailureLocked(ctx, stored.AccountDID, RefreshErrorDecrypt, true)
	}
	key, err := payload.DPoPKey.ecdsa()
	if err != nil {
		return result, c.persistRefreshFailureLocked(ctx, stored.AccountDID, RefreshErrorDPoPKey, true)
	}
	if effectiveLastRefresh(stored).Add(period).After(c.now()) {
		return result, nil
	}
	token, err := c.refreshTokenLocked(ctx, stored.AccountDID, payload, key)
	if err != nil {
		return result, err
	}
	result.Refreshed = true
	result.Token = token
	return result, nil
}

func effectiveLastRefresh(stored bridgestore.OAuthToken) time.Time {
	value := stored.LastRefreshAt
	if value <= 0 {
		value = stored.UpdatedAt
	}
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0)
}

func boundedRefreshErrorClass(class RefreshErrorClass) RefreshErrorClass {
	switch class {
	case RefreshErrorTimeout,
		RefreshErrorConnection,
		RefreshErrorRateLimit,
		RefreshErrorServer,
		RefreshErrorInvalidGrant,
		RefreshErrorMissingRefreshToken,
		RefreshErrorDecrypt,
		RefreshErrorDPoPKey,
		RefreshErrorProtocol:
		return class
	default:
		return RefreshErrorProtocol
	}
}
