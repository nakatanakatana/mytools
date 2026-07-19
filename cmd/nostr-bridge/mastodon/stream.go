package mastodon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type StreamConnection interface {
	Write(context.Context, []byte) error
	Read(context.Context) ([]byte, error)
	Close() error
}
type StreamConnector func(context.Context, string) (StreamConnection, error)
type wsStream struct{ conn *websocket.Conn }

func dialStream(ctx context.Context, url string) (StreamConnection, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	return wsStream{conn}, nil
}
func (w wsStream) Write(ctx context.Context, b []byte) error {
	return w.conn.Write(ctx, websocket.MessageText, b)
}
func (w wsStream) Read(ctx context.Context) ([]byte, error) {
	_, b, err := w.conn.Read(ctx)
	return b, err
}
func (w wsStream) Close() error { return w.conn.Close(websocket.StatusNormalClosure, "") }

type subscription struct {
	Type   string `json:"type"`
	Stream string `json:"stream"`
	List   string `json:"list,omitempty"`
}

func subscribe(ctx context.Context, conn StreamConnection, listIDs []string) error {
	values := []subscription{{Type: "subscribe", Stream: "user"}}
	for _, id := range uniqueStrings(listIDs) {
		values = append(values, subscription{Type: "subscribe", Stream: "list", List: id})
	}
	for _, v := range values {
		b, _ := json.Marshal(v)
		if err := conn.Write(ctx, b); err != nil {
			return err
		}
	}
	return nil
}

func decodeStreamEvent(message []byte) (StreamEvent, error) {
	var envelope struct {
		Event   string `json:"event"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return StreamEvent{}, fmt.Errorf("decode Mastodon stream event: %w", err)
	}
	result := StreamEvent{Event: envelope.Event}
	switch envelope.Event {
	case "update", "status.update":
		if err := json.Unmarshal([]byte(envelope.Payload), &result.Payload); err != nil {
			return StreamEvent{}, errors.New("decode Mastodon status payload")
		}
	case "delete":
		if err := json.Unmarshal([]byte(envelope.Payload), &result.DeleteID); err != nil {
			result.DeleteID = strings.Trim(envelope.Payload, "\"")
		}
	}
	return result, nil
}
func (s *Syncer) consume(ctx context.Context, conn StreamConnection) error {
	for {
		message, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		event, err := decodeStreamEvent(message)
		if err != nil {
			return err
		}
		if err := s.HandleEvent(ctx, event); err != nil {
			return err
		}
	}
}

// Run performs REST catch-up before every subscription and reconnects with bounded backoff.
func (s *Syncer) Run(ctx context.Context) error {
	if strings.TrimSpace(s.options.StreamURL) == "" {
		return s.Backfill(ctx)
	}
	attempt := 0
	for ctx.Err() == nil {
		if err := s.Backfill(ctx); err != nil {
			if waitErr := s.options.Sleep(ctx, attempt); waitErr != nil {
				return nil
			}
			attempt++
			continue
		}
		conn, err := s.options.Connect(ctx, s.options.StreamURL)
		if err == nil {
			if err = subscribe(ctx, conn, s.options.ListIDs); err == nil {
				attempt = 0
				err = s.consume(ctx, conn)
			}
			_ = conn.Close()
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if waitErr := s.options.Sleep(ctx, attempt); waitErr != nil {
				return nil
			}
			if attempt < 6 {
				attempt++
			}
		}
	}
	return nil
}
func backoffSleep(ctx context.Context, attempt int) error {
	delay := backoffDuration(attempt)
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func backoffDuration(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	if attempt < 0 {
		attempt = 0
	}
	return time.Duration(1<<attempt) * time.Second
}
