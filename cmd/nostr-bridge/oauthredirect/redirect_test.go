package oauthredirect

import "testing"

func TestSuccessURL(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
	}{
		{name: "default", want: "/?oauth=success"},
		{name: "relative", input: "/nostr-bridge?source=oauth", want: "/nostr-bridge?oauth=success&source=oauth"},
		{name: "absolute", input: "https://dashboard.example/nostr-bridge", want: "https://dashboard.example/nostr-bridge?oauth=success"},
		{name: "scheme relative", input: "//attacker.example/", want: "/?oauth=success"},
		{name: "non HTTP scheme", input: "javascript:alert(1)", want: "/?oauth=success"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SuccessURL(tt.input); got != tt.want {
				t.Fatalf("SuccessURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
