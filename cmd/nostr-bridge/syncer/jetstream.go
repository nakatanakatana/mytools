package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

// Connection is the minimal Jetstream websocket used by Syncer.
type Connection interface {
	Read(context.Context) ([]byte, error)
	Close() error
}

// Connector establishes a Jetstream connection.
type Connector func(context.Context, string) (Connection, error)

type websocketConnection struct{ conn *websocket.Conn }

func dialJetstream(ctx context.Context, endpoint string) (Connection, error) {
	conn, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return websocketConnection{conn}, nil
}
func (c websocketConnection) Read(ctx context.Context) ([]byte, error) {
	_, message, err := c.conn.Read(ctx)
	return message, err
}
func (c websocketConnection) Close() error { return c.conn.Close(websocket.StatusNormalClosure, "") }

func (s *Syncer) consume(ctx context.Context, connection Connection) error {
	for {
		message, err := connection.Read(ctx)
		if err != nil {
			return err
		}
		event, err := Decode(message)
		if err != nil {
			return err
		}
		if err := s.Handle(ctx, event); err != nil {
			return err
		}
		if s.options.Observer != nil {
			s.options.Observer.JetstreamEvent(time.Now())
		}
	}
}

// Decode converts a Jetstream commit message into a normalized Event.
func Decode(message []byte) (Event, error) {
	var envelope struct {
		DID    string `json:"did"`
		TimeUS int64  `json:"time_us"`
		Commit struct {
			Operation  Operation       `json:"operation"`
			Collection string          `json:"collection"`
			RKey       string          `json:"rkey"`
			CID        string          `json:"cid"`
			Record     json.RawMessage `json:"record"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		return Event{}, fmt.Errorf("decode Jetstream message: %w", err)
	}
	if envelope.DID == "" || envelope.Commit.Collection == "" || envelope.Commit.RKey == "" {
		return Event{}, fmt.Errorf("decode Jetstream message: missing commit identity")
	}
	return Event{DID: envelope.DID, TimeUS: envelope.TimeUS, Collection: envelope.Commit.Collection, RKey: envelope.Commit.RKey, CID: envelope.Commit.CID, Operation: envelope.Commit.Operation, Record: envelope.Commit.Record}, nil
}
