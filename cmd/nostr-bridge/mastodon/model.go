package mastodon

import "time"

type Account struct {
	ID          string `json:"id"`
	URI         string `json:"uri"`
	URL         string `json:"url"`
	DisplayName string `json:"display_name"`
	Note        string `json:"note"`
	Avatar      string `json:"avatar"`
}

type List struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type MediaDimensions struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type MediaMeta struct {
	Original MediaDimensions `json:"original"`
}

type MediaAttachment struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	URL         string    `json:"url"`
	PreviewURL  string    `json:"preview_url"`
	MIMEType    string    `json:"mime_type"`
	Description string    `json:"description"`
	Blurhash    string    `json:"blurhash"`
	Meta        MediaMeta `json:"meta"`
}

type Status struct {
	ID                 string            `json:"id"`
	URI                string            `json:"uri"`
	URL                string            `json:"url"`
	Content            string            `json:"content"`
	SpoilerText        string            `json:"spoiler_text"`
	CreatedAt          time.Time         `json:"created_at"`
	Visibility         string            `json:"visibility"`
	Sensitive          bool              `json:"sensitive"`
	InReplyToID        string            `json:"in_reply_to_id"`
	InReplyToAccountID string            `json:"in_reply_to_account_id"`
	Account            Account           `json:"account"`
	MediaAttachments   []MediaAttachment `json:"media_attachments"`
	Reblog             *Status           `json:"reblog"`
}
