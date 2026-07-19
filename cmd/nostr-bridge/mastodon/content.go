package mastodon

import (
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/source"
	"golang.org/x/net/html"
)

const sensitiveMediaWarning = "Sensitive media"

// HTMLToText converts Mastodon-authored HTML into readable plain text.
func HTMLToText(input string) string {
	document, err := html.Parse(strings.NewReader(input))
	if err != nil {
		return ""
	}
	output := textRenderer{}
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && (hiddenElement(node.Data) || hasClass(node, "invisible")) {
			return
		}
		if node.Type == html.ElementNode {
			switch node.Data {
			case "p", "div", "blockquote", "pre", "li":
				output.breakLines(2)
			case "br":
				output.breakLines(1)
			}
		}
		if node.Type == html.TextNode {
			output.writeText(node.Data)
		}
		if node.Type == html.ElementNode && node.Data == "img" {
			output.writeToken(attribute(node, "alt"))
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == html.ElementNode {
			if node.Data == "span" && hasClass(node, "ellipsis") {
				output.builder.WriteRune('…')
				output.pendingSpace = false
			}
			if node.Data == "a" {
				output.appendLinkTarget(node)
			}
			switch node.Data {
			case "p", "div", "blockquote", "pre", "li":
				output.breakLines(2)
			}
		}
	}
	walk(document)
	return strings.TrimSpace(output.builder.String())
}

func hiddenElement(name string) bool {
	switch name {
	case "script", "style", "template", "head", "noscript", "iframe", "object", "svg", "math":
		return true
	default:
		return false
	}
}

type textRenderer struct {
	builder       strings.Builder
	pendingSpace  bool
	tokenBoundary bool
}

func (r *textRenderer) writeText(value string) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		if value != "" {
			r.pendingSpace = true
			r.tokenBoundary = false
		}
		return
	}
	leadingSpace := len(strings.TrimLeft(value, " \t\r\n\f")) != len(value)
	first, _ := utf8.DecodeRuneInString(fields[0])
	if (leadingSpace || (r.pendingSpace && (!r.tokenBoundary || !unicode.IsPunct(first)))) && r.builder.Len() > 0 && !strings.HasSuffix(r.builder.String(), "\n") {
		r.builder.WriteByte(' ')
	}
	r.builder.WriteString(strings.Join(fields, " "))
	r.pendingSpace = len(value) > 0 && strings.ContainsAny(value[len(value)-1:], " \t\r\n\f")
	r.tokenBoundary = false
}

func (r *textRenderer) writeToken(value string) {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return
	}
	if r.builder.Len() > 0 && !strings.HasSuffix(r.builder.String(), "\n") {
		r.builder.WriteByte(' ')
	}
	r.builder.WriteString(value)
	r.pendingSpace = true
	r.tokenBoundary = true
}

func (r *textRenderer) breakLines(count int) {
	value := r.builder.String()
	if strings.TrimSpace(value) == "" {
		return
	}
	trailing := len(value) - len(strings.TrimRight(value, "\n"))
	for trailing < count {
		r.builder.WriteByte('\n')
		trailing++
	}
	r.pendingSpace = false
	r.tokenBoundary = false
}

func (r *textRenderer) appendLinkTarget(node *html.Node) {
	var href string
	for _, attribute := range node.Attr {
		if attribute.Key == "href" {
			href = strings.TrimSpace(attribute.Val)
			break
		}
	}
	u, err := url.Parse(href)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return
	}
	visible := visibleTextContent(node)
	if visible == href {
		return
	}
	r.builder.WriteString(" (")
	r.builder.WriteString(href)
	r.builder.WriteByte(')')
	r.pendingSpace = false
}

func visibleTextContent(node *html.Node) string {
	renderer := textRenderer{}
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.ElementNode && (hiddenElement(current.Data) || hasClass(current, "invisible")) {
			return
		}
		if current.Type == html.TextNode {
			renderer.writeText(current.Data)
		}
		if current.Type == html.ElementNode && current.Data == "img" {
			renderer.writeToken(attribute(current, "alt"))
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if current.Type == html.ElementNode && current.Data == "span" && hasClass(current, "ellipsis") {
			renderer.builder.WriteRune('…')
			renderer.pendingSpace = false
		}
	}
	walk(node)
	return strings.TrimSpace(renderer.builder.String())
}

func attribute(node *html.Node, name string) string {
	for _, value := range node.Attr {
		if value.Key == name {
			return value.Val
		}
	}
	return ""
}

func hasClass(node *html.Node, name string) bool {
	for _, class := range strings.Fields(attribute(node, "class")) {
		if class == name {
			return true
		}
	}
	return false
}

// NormalizeStatus converts a supported Mastodon status to provider-neutral data.
func NormalizeStatus(status Status) (source.Post, bool, error) {
	if status.Visibility != "public" || status.Reblog != nil {
		return source.Post{}, false, nil
	}
	if !absoluteHTTPURL(status.URI) || !absoluteHTTPURL(status.Account.URI) {
		return source.Post{}, false, errors.New("Mastodon status requires canonical status and account URIs")
	}
	warning := strings.TrimSpace(status.SpoilerText)
	if warning == "" && status.Sensitive && len(status.MediaAttachments) > 0 {
		warning = sensitiveMediaWarning
	}
	body := HTMLToText(status.Content)
	text := body
	if warning != "" {
		text = warning
		if body != "" {
			text += "\n\n" + body
		}
	}
	post := source.Post{
		ID: "mastodon:" + status.URI, Author: source.ActorIdentity{Provider: "mastodon", ID: status.Account.URI},
		SourceURL: status.URL, Text: text, ContentWarning: warning, CreatedAt: status.CreatedAt,
	}
	if !absoluteHTTPURL(post.SourceURL) {
		post.SourceURL = status.URI
	}
	if status.InReplyToID != "" {
		post.ReplyToID = "mastodon:" + status.InReplyToID
	}
	for _, media := range status.MediaAttachments {
		if !absoluteHTTPURL(media.URL) || (media.Type != "image" && media.Type != "video" && media.Type != "gifv") {
			continue
		}
		mimeType := media.MIMEType
		if mimeType == "" {
			mimeType = mediaMIMEType(media.URL)
		}
		post.Attachments = append(post.Attachments, source.Attachment{URL: media.URL, MIMEType: mimeType, Description: media.Description, Blurhash: media.Blurhash, Width: media.Meta.Original.Width, Height: media.Meta.Original.Height})
	}
	return post, true, nil
}

func mediaMIMEType(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	path := strings.ToLower(u.Path)
	for extension, mimeType := range map[string]string{
		".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
		".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
	} {
		if strings.HasSuffix(path, extension) {
			return mimeType
		}
	}
	return ""
}

func absoluteHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
