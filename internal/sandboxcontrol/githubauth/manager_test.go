package githubauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_DeviceFlowHappyPathWithSlowDownAndLogout(t *testing.T) {
	var pollCount atomic.Int32
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			require.NoError(t, r.ParseForm())
			assert.Equal(t, "Iv1.testclient", r.FormValue("client_id"))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-secret-code",
				"user_code":        "ABCD-1234",
				"verification_uri": "https://github.com/login/device",
				"expires_in":       600,
				"interval":         0,
			})
		case "/login/oauth/access_token":
			require.NoError(t, r.ParseForm())
			assert.Equal(t, "Iv1.testclient", r.FormValue("client_id"))
			assert.Equal(t, "dev-secret-code", r.FormValue("device_code"))
			assert.Equal(t, "urn:ietf:params:oauth:grant-type:device_code", r.FormValue("grant_type"))

			n := pollCount.Add(1)
			switch n {
			case 1:
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			case 2:
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  "ghu_secret_token",
					"expires_in":    28800,
					"refresh_token": "ghr_secret_refresh",
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ghSrv.Close()

	var (
		gotAccessToken  string
		gotRefreshToken string
		gotExpiresIn    int
		gotContentLen   int64
		mode            = "app_installation"
	)
	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/github/status":
			var exp *int64
			if mode == "user_oauth" {
				v := int64(1763810000)
				exp = &v
			}
			_ = json.NewEncoder(w).Encode(ProxyStatus{
				Mode:               mode,
				UserTokenExpiresAt: exp,
				AppConfigured:      true,
			})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/github/user-token":
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			gotContentLen = r.ContentLength
			var body putUserTokenRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			gotAccessToken = body.AccessToken
			gotRefreshToken = body.RefreshToken
			gotExpiresIn = body.ExpiresIn
			mode = "user_oauth"
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "user_oauth", "expires_at": 1763810000})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/github/user-token":
			mode = "app_installation"
			_ = json.NewEncoder(w).Encode(map[string]any{"mode": "app_installation"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer adminSrv.Close()

	mgr := NewManager(Options{
		ClientID:            "Iv1.testclient",
		EgressProxyAdminURL: adminSrv.URL,
		GitHubBaseURL:       ghSrv.URL,
		MinPollInterval:     10 * time.Millisecond,
		SlowDownStep:        10 * time.Millisecond,
	})
	defer mgr.Close()

	status, done, err := mgr.StartDeviceFlow(t.Context())
	require.NoError(t, err)
	assert.Equal(t, FlowStatePending, status.State)
	assert.Equal(t, "ABCD-1234", status.UserCode)
	assert.Equal(t, "https://github.com/login/device", status.VerificationURI)

	// Ensure FlowStatus JSON never leaks sensitive fields.
	raw, err := json.Marshal(status)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "dev-secret-code")

	select {
	case final := <-done:
		assert.Equal(t, FlowStateCompleted, final.State)
		assert.Empty(t, final.Error)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for device flow completion")
	}

	assert.Equal(t, "ghu_secret_token", gotAccessToken)
	assert.Equal(t, "ghr_secret_refresh", gotRefreshToken)
	assert.Equal(t, 28800, gotExpiresIn)
	assert.Greater(t, gotContentLen, int64(0))

	snap := mgr.Status(t.Context())
	require.NotNil(t, snap.Proxy)
	assert.Equal(t, "user_oauth", snap.Proxy.Mode)
	assert.Equal(t, FlowStateCompleted, snap.Flow.State)

	// Logout reverts to app_installation.
	out, err := mgr.Logout(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "app_installation", out.Mode)
}

func TestManager_DeviceFlowDeniedAndConcurrentCancellation(t *testing.T) {
	var codeCalls atomic.Int32
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			n := codeCalls.Add(1)
			code := "CODE-1"
			if n > 1 {
				code = "CODE-2"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dev-" + code,
				"user_code":        code,
				"verification_uri": "https://github.com/login/device",
				"expires_in":       600,
				"interval":         0,
			})
		case "/login/oauth/access_token":
			require.NoError(t, r.ParseForm())
			if r.FormValue("device_code") == "dev-CODE-1" {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
		}
	}))
	defer ghSrv.Close()

	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ProxyStatus{Mode: "unconfigured"})
	}))
	defer adminSrv.Close()

	mgr := NewManager(Options{
		ClientID:            "Iv1.test",
		EgressProxyAdminURL: adminSrv.URL,
		GitHubBaseURL:       ghSrv.URL,
		MinPollInterval:     15 * time.Millisecond,
	})
	defer mgr.Close()

	st1, done1, err := mgr.StartDeviceFlow(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "CODE-1", st1.UserCode)

	// Starting a second flow must cancel the first one.
	st2, done2, err := mgr.StartDeviceFlow(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "CODE-2", st2.UserCode)

	select {
	case f1 := <-done1:
		assert.Equal(t, FlowStateFailed, f1.State)
	case <-time.After(2 * time.Second):
		t.Fatal("first session was not cancelled")
	}

	select {
	case f2 := <-done2:
		assert.Equal(t, FlowStateFailed, f2.State)
		assert.Contains(t, f2.Error, "denied")
	case <-time.After(2 * time.Second):
		t.Fatal("second session did not finish")
	}
}

func TestManager_RejectsRedirectOnTokenPut(t *testing.T) {
	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
	}))
	defer adminSrv.Close()

	mgr := NewManager(Options{
		ClientID:            "Iv1.test",
		EgressProxyAdminURL: adminSrv.URL,
	})
	err := mgr.putUserToken(t.Context(), putUserTokenRequest{AccessToken: "ghu_secret"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "302")
	assert.NotContains(t, err.Error(), "ghu_secret")
}

func TestManager_DisabledWhenClientIDEmpty(t *testing.T) {
	mgr := NewManager(Options{
		EgressProxyAdminURL: "http://127.0.0.1:1",
	})
	assert.False(t, mgr.Enabled())
	_, _, err := mgr.StartDeviceFlow(t.Context())
	require.Error(t, err)
}
