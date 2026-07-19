package mastodon

import (
	"testing"
	"time"
)

func TestHTMLToTextRetainsReadableStructureLinksAndEntities(t *testing.T) {
	input := `<p>Hello &amp; welcome<br>next <a href="https://example.com/page">page</a> <a href="https://social.example/@alice">@alice</a></p><p><a href="https://example.com/page">https://example.com/page</a></p>`
	want := "Hello & welcome\nnext page (https://example.com/page) @alice (https://social.example/@alice)\n\nhttps://example.com/page"
	if got := HTMLToText(input); got != want {
		t.Fatalf("HTMLToText() = %q, want %q", got, want)
	}
}

func TestHTMLToTextHandlesMalformedHTMLAndDiscardsActiveContent(t *testing.T) {
	input := `<p>before <b>bold<p>after <a href="javascript:alert(1)">safe label</a><script>alert("no")</script><style>.no{display:block}</style><template>hidden</template>`
	want := "before bold\n\nafter safe label"
	if got := HTMLToText(input); got != want {
		t.Fatalf("HTMLToText() = %q, want %q", got, want)
	}
}

func TestHTMLToTextPreservesInlinePunctuationAndWordSpacing(t *testing.T) {
	input := `<p>Hello <strong>bold</strong> world! Mention: <span>@alice</span>.</p>`
	want := "Hello bold world! Mention: @alice."
	if got := HTMLToText(input); got != want {
		t.Fatalf("HTMLToText() = %q, want %q", got, want)
	}
}

func TestHTMLToTextPreservesImageAltWithWordSpacing(t *testing.T) {
	input := `<p>Hello<img src="https://cdn.example/wave.png" alt=" :wave: " title="ignored">world<img src="bad" alt="">!</p>`
	want := "Hello :wave: world!"
	if got := HTMLToText(input); got != want {
		t.Fatalf("HTMLToText() = %q, want %q", got, want)
	}
}

func TestHTMLToTextHandlesMastodonDisplayLinks(t *testing.T) {
	input := `<p>See <a href="https://example.com/@alice/very/long/path"><span class="invisible">https://</span><span class="ellipsis">example.com/@alice</span><span class="invisible">/very/long/path</span></a> and <a href="https://example.net/whole">https://example.net/whole</a>.</p>`
	want := "See example.com/@alice… (https://example.com/@alice/very/long/path) and https://example.net/whole."
	if got := HTMLToText(input); got != want {
		t.Fatalf("HTMLToText() = %q, want %q", got, want)
	}
}

func TestNormalizeStatusRejectsNonPublicAndBoosts(t *testing.T) {
	cases := []Status{
		{Visibility: "unlisted"}, {Visibility: "quiet_public"}, {Visibility: "private"}, {Visibility: "direct"},
		{Visibility: "public", Reblog: &Status{ID: "boosted"}},
	}
	for _, status := range cases {
		if _, ok, err := NormalizeStatus(status); err != nil || ok {
			t.Fatalf("status=%#v ok=%v err=%v", status, ok, err)
		}
	}
}

func TestNormalizeStatusMapsIdentityReplyWarningAndMedia(t *testing.T) {
	createdAt := time.Unix(1_700_000_000, 0).UTC()
	status := Status{
		ID: "42", URI: "https://social.example/users/alice/statuses/42", URL: "https://social.example/@alice/42",
		Content: `<p>Hello <a href="https://example.com">world</a></p>`, SpoilerText: "Plot twist", CreatedAt: createdAt,
		Visibility: "public", InReplyToID: "21", InReplyToAccountID: "bob", Account: Account{URI: "https://social.example/users/alice"},
		MediaAttachments: []MediaAttachment{
			{Type: "image", URL: "https://cdn.example/photo.jpg", MIMEType: "image/jpeg", Description: "A cat", Blurhash: "U8ABC", Meta: MediaMeta{Original: MediaDimensions{Width: 1200, Height: 800}}},
			{Type: "video", URL: "https://cdn.example/movie.mp4", MIMEType: "video/mp4", Description: "A clip", Meta: MediaMeta{Original: MediaDimensions{Width: 1920, Height: 1080}}},
		},
	}
	post, ok, err := NormalizeStatus(status)
	if err != nil || !ok {
		t.Fatalf("NormalizeStatus() ok=%v err=%v", ok, err)
	}
	if post.ID != "mastodon:https://social.example/users/alice/statuses/42" || post.Author.Provider != "mastodon" || post.Author.ID != status.Account.URI {
		t.Errorf("identity = %#v / %q", post.Author, post.ID)
	}
	if post.SourceURL != status.URL || post.Text != "Plot twist\n\nHello world (https://example.com)" || post.ReplyToID != "mastodon:21" || post.ContentWarning != "Plot twist" || !post.CreatedAt.Equal(createdAt) {
		t.Errorf("post = %#v", post)
	}
	if len(post.Attachments) != 2 || post.Attachments[0].MIMEType != "image/jpeg" || post.Attachments[0].Description != "A cat" || post.Attachments[0].Blurhash != "U8ABC" || post.Attachments[0].Width != 1200 || post.Attachments[0].Height != 800 || post.Attachments[1].MIMEType != "video/mp4" {
		t.Errorf("attachments = %#v", post.Attachments)
	}
}

func TestNormalizeStatusPreservesCustomEmojiAlt(t *testing.T) {
	status := Status{URI: "https://social.example/users/alice/statuses/42", URL: "https://social.example/@alice/42", Visibility: "public", Account: Account{URI: "https://social.example/users/alice"}, Content: `<p>Hello <img src="https://social.example/emoji/blob.png" alt=":blobcat:"> friend</p>`}
	post, ok, err := NormalizeStatus(status)
	if err != nil || !ok {
		t.Fatalf("NormalizeStatus() ok=%v err=%v", ok, err)
	}
	if post.Text != "Hello :blobcat: friend" {
		t.Fatalf("text = %q", post.Text)
	}
}

func TestNormalizeStatusUsesCanonicalURIAndSensitiveMediaWarning(t *testing.T) {
	status := Status{ID: "42", URI: "https://remote.example/users/alice/statuses/42", URL: "", Visibility: "public", Sensitive: true, Account: Account{URI: "https://remote.example/users/alice"}, MediaAttachments: []MediaAttachment{{Type: "image", URL: "https://remote.example/media/a.png"}, {Type: "image", URL: "javascript:bad"}}}
	post, ok, err := NormalizeStatus(status)
	if err != nil || !ok {
		t.Fatalf("NormalizeStatus() ok=%v err=%v", ok, err)
	}
	if post.SourceURL != status.URI || post.ContentWarning == "" || post.Text == "" || post.Text != post.ContentWarning {
		t.Fatalf("sensitive post = %#v", post)
	}
	if len(post.Attachments) != 1 || post.Attachments[0].URL != "https://remote.example/media/a.png" || post.Attachments[0].MIMEType != "image/png" {
		t.Fatalf("attachments = %#v", post.Attachments)
	}
}

func TestNormalizeStatusRejectsMissingCanonicalIdentity(t *testing.T) {
	for _, status := range []Status{{Visibility: "public", Account: Account{URI: "actor"}}, {Visibility: "public", URI: "status"}} {
		if _, ok, err := NormalizeStatus(status); err == nil || ok {
			t.Fatalf("status=%#v ok=%v err=%v", status, ok, err)
		}
	}
}
