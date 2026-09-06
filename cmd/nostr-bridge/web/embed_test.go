package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestHandlerServesEmbeddedShellAndAssets(t *testing.T) {
	handler := Handler()

	tests := []struct {
		name            string
		path            string
		wantContentType string
		wantBody        []string
	}{
		{
			name:            "shell",
			path:            "/",
			wantContentType: "text/html",
			wantBody:        []string{"nostr-bridge", "provider-list", "初期同期", "id=\"oauth-callback-status\"", "id=\"status-error\"", "id=\"action-error\""},
		},
		{
			name:            "javascript",
			path:            "/app.js",
			wantContentType: "text/javascript",
			wantBody:        []string{"provider-card-template"},
		},
		{
			name:            "stylesheet",
			path:            "/styles.css",
			wantContentType: "text/css",
			wantBody:        []string{".status-ready"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tt.path, nil))

			if recorder.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, body = %s", tt.path, recorder.Code, recorder.Body.String())
			}
			if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, tt.wantContentType) {
				t.Fatalf("GET %s Content-Type = %q, want to contain %q", tt.path, contentType, tt.wantContentType)
			}
			for _, wantBody := range tt.wantBody {
				if !strings.Contains(recorder.Body.String(), wantBody) {
					t.Fatalf("GET %s body does not contain %q: %s", tt.path, wantBody, recorder.Body.String())
				}
			}
		})
	}
}

func TestHandlerReturnsNotFoundForMissingAsset(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/missing.js", nil))

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET /missing.js status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestProviderCardTemplateLeavesProviderSetupControlsToCardCreation(t *testing.T) {
	recorder := httptest.NewRecorder()
	Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	document, err := html.Parse(strings.NewReader(recorder.Body.String()))
	if err != nil {
		t.Fatalf("parse shell: %v", err)
	}
	template := findElementByID(document, "provider-card-template")
	if template == nil {
		t.Fatal("provider card template was not served")
	}
	if findElementByAttribute(template, "data-provider-setup") == nil {
		t.Fatal("provider card template does not contain a provider setup container")
	}
	if findElement(template, "form") != nil || findElement(template, "button") != nil || findElement(template, "input") != nil {
		t.Fatal("provider card template contains controls that may be shown for the wrong provider")
	}
}

func findElementByID(root *html.Node, id string) *html.Node {
	return findElementByAttributeValue(root, "id", id)
}

func findElementByAttribute(root *html.Node, name string) *html.Node {
	if root.Type == html.ElementNode {
		for _, attribute := range root.Attr {
			if attribute.Key == name {
				return root
			}
		}
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := findElementByAttribute(child, name); found != nil {
			return found
		}
	}
	return nil
}

func findElementByAttributeValue(root *html.Node, name, value string) *html.Node {
	if root.Type == html.ElementNode {
		for _, attribute := range root.Attr {
			if attribute.Key == name && attribute.Val == value {
				return root
			}
		}
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := findElementByAttributeValue(child, name, value); found != nil {
			return found
		}
	}
	return nil
}

func findElement(root *html.Node, name string) *html.Node {
	if root.Type == html.ElementNode && root.Data == name {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := findElement(child, name); found != nil {
			return found
		}
	}
	return nil
}

func TestClientAssetsReferenceStatusAndOAuthRoutes(t *testing.T) {
	handler := Handler()

	tests := []struct {
		path string
		want []string
	}{
		{
			path: "/app.js",
			want: []string{
				"/api/status",
				"/oauth/bluesky/start",
				"/oauth/mastodon/start",
				"showOAuthCallbackStatus();\nloadStatus();",
				"const oauthInFlight = { bluesky: false, mastodon: false };",
				"function syncOAuthControls()",
				"function updateProviderCard(",
				"let statusRequest = null;",
				"function reconcileProviderCards(",
				"card.isConnected && providerList.isConnected",
				"showStatusError(requestErrorMessage);",
				"showActionError(\"Bluesky ハンドルを入力してください。\");",
				"overallStatus.textContent !== overallText",
				"providerEmptyState.hidden = cards.length > 0;",
				"if (document.activeElement !== handleInput",
				"clearStatusError();",
				"clearActionError();",
				"let oauthCallbackPending = false;",
				"同期状況を更新しました。",
				"oauthInFlight.bluesky = false;",
				"oauthInFlight.mastodon = false;",
				"function createBlueskySetup()",
				"function createMastodonSetup()",
				"const setup = card.querySelector(\"[data-provider-setup]\");",
				"status.outbox?.ready === false",
				"if (provider.access_token_expired === true)",
				"if (provider.degraded === true)",
			},
		},
		{
			path: "/",
			want: []string{
				"<link rel=\"stylesheet\" href=\"/styles.css\">",
				"<script src=\"/app.js\" defer></script>",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tt.path, nil))

			for _, want := range tt.want {
				if !strings.Contains(recorder.Body.String(), want) {
					t.Fatalf("GET %s body does not contain %q: %s", tt.path, want, recorder.Body.String())
				}
			}
		})
	}
}
