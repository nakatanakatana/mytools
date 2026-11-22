package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/nakatanakatana/mytools/internal/sandboxcontrol/k8s"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeEnsurer struct {
	calledWith string
	sandbox    *k8s.SandboxInfo
	err        error
}

func (f *fakeEnsurer) EnsureRunningAndReady(_ context.Context, name string) (*k8s.SandboxInfo, error) {
	f.calledWith = name
	if f.err != nil {
		return nil, f.err
	}
	if f.sandbox != nil {
		return f.sandbox, nil
	}
	return &k8s.SandboxInfo{Name: name, Ready: true}, nil
}

func TestPreviewHandler_HTTPAndHeaders(t *testing.T) {
	var (
		gotPath      string
		gotQuery     string
		gotID        string
		gotNamespace string
		gotPort      string
		gotEmail     string
	)
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotID = r.Header.Get(HeaderSandboxID)
		gotNamespace = r.Header.Get(HeaderSandboxNamespace)
		gotPort = r.Header.Get(HeaderSandboxPort)
		gotEmail = r.Header.Get(HeaderSandboxUserEmail)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("proxied-ok"))
	}))
	defer router.Close()

	ensurer := &fakeEnsurer{}
	h, err := NewPreviewHandler(router.URL, "sandbox", false, ensurer)
	require.NoError(t, err)

	// Missing Tailscale-User-Login -> 401
	req := httptest.NewRequest(http.MethodGet, "/preview/dev-1/3000/hello?x=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Valid request -> proxied with headers
	req = httptest.NewRequest(http.MethodGet, "/preview/dev-1/3000/hello?x=1", nil)
	req.Header.Set(HeaderTailscaleUserLogin, "alice@example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "proxied-ok", rec.Body.String())
	assert.Equal(t, "dev-1", ensurer.calledWith)
	assert.Equal(t, "/hello", gotPath)
	assert.Equal(t, "x=1", gotQuery)
	assert.Equal(t, "dev-1", gotID)
	assert.Equal(t, "sandbox", gotNamespace)
	assert.Equal(t, "3000", gotPort)
	assert.Equal(t, "alice@example.com", gotEmail)
}

func TestPreviewHandler_WebSocket(t *testing.T) {
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "dev-ws", r.Header.Get(HeaderSandboxID))
		assert.Equal(t, "4096", r.Header.Get(HeaderSandboxPort))
		assert.Equal(t, "alice@example.com", r.Header.Get(HeaderSandboxUserEmail))

		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()

		typ, reader, err := c.Reader(r.Context())
		if err != nil {
			return
		}
		msg, err := io.ReadAll(reader)
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), typ, append([]byte("echo:"), msg...))
	}))
	defer router.Close()

	ensurer := &fakeEnsurer{}
	h, err := NewPreviewHandler(router.URL, "sandbox", false, ensurer)
	require.NoError(t, err)

	front := httptest.NewServer(h)
	defer front.Close()

	wsURL := "ws" + strings.TrimPrefix(front.URL, "http") + "/preview/dev-ws/4096/ws"
	conn, _, err := websocket.Dial(t.Context(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{
			HeaderTailscaleUserLogin: []string{"alice@example.com"},
		},
	})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	require.NoError(t, conn.Write(t.Context(), websocket.MessageText, []byte("ping")))
	_, data, err := conn.Read(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "echo:ping", string(data))
}
