package githubauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultGitHubBaseURL = "https://github.com"
	maxResponseBytes     = 1 << 20 // 1 MiB
	defaultHTTPTimeout   = 15 * time.Second
)

// FlowState represents the lifecycle state of an in-memory GitHub Device Flow session.
type FlowState string

const (
	FlowStateIdle      FlowState = "idle"
	FlowStatePending   FlowState = "pending"
	FlowStateCompleted FlowState = "completed"
	FlowStateFailed    FlowState = "failed"
)

// ProxyStatus mirrors GET /v1/github/status from egress-proxy-admin.
type ProxyStatus struct {
	Mode               string `json:"mode"`
	UserTokenExpiresAt *int64 `json:"user_token_expires_at,omitempty"`
	AppConfigured      bool   `json:"app_configured"`
}

// FlowStatus exposes non-sensitive Device Flow state to the UI, API, and SSH gateway.
// It deliberately never includes device_code, access_token, or refresh_token.
type FlowStatus struct {
	State           FlowState  `json:"state"`
	UserCode        string     `json:"user_code,omitempty"`
	VerificationURI string     `json:"verification_uri,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// StatusSnapshot combines egress-proxy status with the current Device Flow session state.
type StatusSnapshot struct {
	DeviceFlowEnabled bool         `json:"device_flow_enabled"`
	Proxy             *ProxyStatus `json:"proxy,omitempty"`
	ProxyError        string       `json:"proxy_error,omitempty"`
	Flow              FlowStatus   `json:"flow"`
}

// Options configures a Manager.
type Options struct {
	ClientID            string
	EgressProxyAdminURL string
	GitHubBaseURL       string
	HTTPClient          *http.Client
	Logger              *slog.Logger
	Now                 func() time.Time
	MinPollInterval     time.Duration
	SlowDownStep        time.Duration
}

// Manager orchestrates GitHub App Device Flow authentication and forwards obtained
// user tokens to egress-proxy-admin without persisting secrets to disk.
type Manager struct {
	clientID      string
	adminBaseURL  string
	githubBaseURL string
	httpClient    *http.Client
	logger        *slog.Logger
	now           func() time.Time
	minInterval   time.Duration
	slowDownStep  time.Duration

	mu        sync.Mutex
	sessionID uint64
	cancel    context.CancelFunc
	flow      FlowStatus
}

// NewManager creates a Manager with hardened HTTP client defaults (timeout + no redirect following).
func NewManager(opts Options) *Manager {
	ghBase := strings.TrimRight(strings.TrimSpace(opts.GitHubBaseURL), "/")
	if ghBase == "" {
		ghBase = defaultGitHubBaseURL
	}
	adminBase := strings.TrimRight(strings.TrimSpace(opts.EgressProxyAdminURL), "/")

	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{
			Timeout: defaultHTTPTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	} else {
		cloned := *hc
		if cloned.Timeout <= 0 {
			cloned.Timeout = defaultHTTPTimeout
		}
		if cloned.CheckRedirect == nil {
			cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}
		}
		hc = &cloned
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	minInterval := opts.MinPollInterval
	if minInterval <= 0 {
		minInterval = 5 * time.Second
	}
	slowDownStep := opts.SlowDownStep
	if slowDownStep <= 0 {
		slowDownStep = 5 * time.Second
	}

	return &Manager{
		clientID:      strings.TrimSpace(opts.ClientID),
		adminBaseURL:  adminBase,
		githubBaseURL: ghBase,
		httpClient:    hc,
		logger:        logger,
		now:           nowFn,
		minInterval:   minInterval,
		slowDownStep:  slowDownStep,
		flow:          FlowStatus{State: FlowStateIdle},
	}
}

// Enabled reports whether GitHub App Device Flow is configured.
func (m *Manager) Enabled() bool {
	return m.clientID != ""
}

// Close cancels any active background polling session.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

// GetProxyStatus queries GET /v1/github/status on egress-proxy-admin.
func (m *Manager) GetProxyStatus(ctx context.Context) (*ProxyStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.adminBaseURL+"/v1/github/status", nil)
	if err != nil {
		return nil, fmt.Errorf("build egress-proxy status request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query egress-proxy status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("egress-proxy status returned HTTP %d", resp.StatusCode)
	}
	var st ProxyStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&st); err != nil {
		return nil, fmt.Errorf("decode egress-proxy status: %w", err)
	}
	return &st, nil
}

// Logout calls DELETE /v1/github/user-token on egress-proxy-admin and clears any active flow state.
func (m *Manager) Logout(ctx context.Context) (*ProxyStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, m.adminBaseURL+"/v1/github/user-token", nil)
	if err != nil {
		return nil, fmt.Errorf("build egress-proxy logout request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("delete github user token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("egress-proxy logout returned HTTP %d", resp.StatusCode)
	}
	var st ProxyStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&st); err != nil {
		return nil, fmt.Errorf("decode egress-proxy logout response: %w", err)
	}

	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.flow = FlowStatus{State: FlowStateIdle}
	m.mu.Unlock()

	return &st, nil
}

// Status returns a combined snapshot of egress-proxy status and current Device Flow state.
func (m *Manager) Status(ctx context.Context) StatusSnapshot {
	snap := StatusSnapshot{
		DeviceFlowEnabled: m.Enabled(),
	}
	st, err := m.GetProxyStatus(ctx)
	if err != nil {
		snap.ProxyError = err.Error()
	} else {
		snap.Proxy = st
	}

	m.mu.Lock()
	snap.Flow = m.flow
	m.mu.Unlock()
	return snap
}

type deviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

type accessTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Interval     int    `json:"interval"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type putUserTokenRequest struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// StartDeviceFlow initiates a GitHub App Device Flow and starts background polling.
// Only one Device Flow may be active at a time; starting a new one cancels any previous session.
// The returned channel receives the terminal FlowStatus (completed or failed) and is then closed.
func (m *Manager) StartDeviceFlow(ctx context.Context) (FlowStatus, <-chan FlowStatus, error) {
	if !m.Enabled() {
		return FlowStatus{}, nil, errors.New("github device flow is disabled (GITHUB_APP_CLIENT_ID is not configured)")
	}

	dcResp, err := m.requestDeviceCode(ctx)
	if err != nil {
		return FlowStatus{}, nil, err
	}

	expiresIn := time.Duration(dcResp.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 15 * time.Minute
	}
	expiresAt := m.now().UTC().Add(expiresIn)

	interval := time.Duration(dcResp.Interval) * time.Second
	if interval < m.minInterval {
		interval = m.minInterval
	}

	status := FlowStatus{
		State:           FlowStatePending,
		UserCode:        dcResp.UserCode,
		VerificationURI: dcResp.VerificationURI,
		ExpiresAt:       &expiresAt,
	}

	pollCtx, cancel := context.WithDeadline(context.Background(), expiresAt)
	done := make(chan FlowStatus, 1)

	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.sessionID++
	sid := m.sessionID
	m.cancel = cancel
	m.flow = status
	m.mu.Unlock()

	deviceCode := dcResp.DeviceCode
	dcResp.DeviceCode = ""

	go m.pollLoop(pollCtx, sid, deviceCode, interval, done)

	return status, done, nil
}

func (m *Manager) requestDeviceCode(ctx context.Context) (*deviceCodeResponse, error) {
	form := url.Values{}
	form.Set("client_id", m.clientID)
	encoded := form.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.githubBaseURL+"/login/device/code", strings.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("build device code request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request github device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github device code endpoint returned HTTP %d", resp.StatusCode)
	}
	var dc deviceCodeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&dc); err != nil {
		return nil, fmt.Errorf("decode github device code response: %w", err)
	}
	if dc.Error != "" {
		return nil, fmt.Errorf("github device code error: %s", dc.Error)
	}
	if dc.DeviceCode == "" || dc.UserCode == "" || dc.VerificationURI == "" {
		return nil, errors.New("github device code response missing required fields")
	}
	return &dc, nil
}

func (m *Manager) pollLoop(ctx context.Context, sid uint64, deviceCode string, interval time.Duration, done chan<- FlowStatus) {
	defer func() {
		deviceCode = ""
		close(done)
	}()

	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			final := m.finishSession(sid, FlowStateFailed, "device flow expired or cancelled")
			done <- final
			return
		case <-timer.C:
		}

		tokResp, err := m.pollAccessToken(ctx, deviceCode)
		if err != nil {
			if ctx.Err() != nil {
				final := m.finishSession(sid, FlowStateFailed, "device flow expired or cancelled")
				done <- final
				return
			}
			m.logger.Warn("github device flow poll transient error", "error", err)
			continue
		}

		if tokResp.AccessToken != "" {
			putErr := m.putUserToken(ctx, putUserTokenRequest{
				AccessToken:  tokResp.AccessToken,
				ExpiresIn:    tokResp.ExpiresIn,
				RefreshToken: tokResp.RefreshToken,
			})
			tokResp.AccessToken = ""
			tokResp.RefreshToken = ""
			if putErr != nil {
				m.logger.Error("failed to register github user token with egress-proxy", "error", putErr)
				final := m.finishSession(sid, FlowStateFailed, fmt.Sprintf("failed to register token with egress-proxy: %v", putErr))
				done <- final
				return
			}
			m.logger.Info("registered github user token via device flow")
			final := m.finishSession(sid, FlowStateCompleted, "")
			done <- final
			return
		}

		switch tokResp.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			next := interval + m.slowDownStep
			if tokResp.Interval > 0 {
				if serverInterval := time.Duration(tokResp.Interval) * time.Second; serverInterval > next {
					next = serverInterval
				}
			}
			interval = next
			continue
		case "expired_token":
			final := m.finishSession(sid, FlowStateFailed, "device code expired before authorization completed")
			done <- final
			return
		case "access_denied":
			final := m.finishSession(sid, FlowStateFailed, "authorization was denied by user")
			done <- final
			return
		default:
			msg := tokResp.Error
			if msg == "" {
				msg = "unexpected empty response from github token endpoint"
			}
			final := m.finishSession(sid, FlowStateFailed, fmt.Sprintf("github device flow failed: %s", msg))
			done <- final
			return
		}
	}
}

func (m *Manager) pollAccessToken(ctx context.Context, deviceCode string) (*accessTokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", m.clientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	encoded := form.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.githubBaseURL+"/login/oauth/access_token", strings.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("build access token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll github access token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github access token endpoint returned HTTP %d", resp.StatusCode)
	}
	var tr accessTokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&tr); err != nil {
		return nil, fmt.Errorf("decode github access token response: %w", err)
	}
	return &tr, nil
}

func (m *Manager) putUserToken(ctx context.Context, payload putUserTokenRequest) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errors.New("marshal user token payload")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, m.adminBaseURL+"/v1/github/user-token", bytes.NewReader(body))
	if err != nil {
		return errors.New("build egress-proxy user-token request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.ContentLength = int64(len(body))

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("egress-proxy user-token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("egress-proxy user-token returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (m *Manager) finishSession(sid uint64, state FlowState, errMsg string) FlowStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	final := FlowStatus{
		State: state,
		Error: errMsg,
	}
	if m.sessionID == sid {
		m.flow = final
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
	}
	return final
}
