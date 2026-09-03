package oauthredirect

import (
	"net/url"
	"strings"
)

// SuccessURL returns a safe OAuth completion URL with the success result.
// Empty and invalid values fall back to the bridge's same-origin dashboard.
func SuccessURL(raw string) string {
	target := strings.TrimSpace(raw)
	if target == "" {
		target = "/"
	}
	u, err := url.Parse(target)
	if err != nil || !allowed(u) {
		u = &url.URL{Path: "/"}
	}
	query := u.Query()
	query.Set("oauth", "success")
	u.RawQuery = query.Encode()
	return u.String()
}

func allowed(u *url.URL) bool {
	if u == nil || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.IsAbs() {
		return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
	}
	return u.Host == "" && strings.HasPrefix(u.Path, "/")
}
