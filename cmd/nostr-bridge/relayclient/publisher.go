package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"
)

const publisherResponseLimit = 64 << 10

type Publisher interface {
	Publish(context.Context, nostr.Event) error
}

// PublisherNotAllowedError indicates that relay-side publisher authorization
// drifted from the bridge's durable registration state.
type PublisherNotAllowedError struct{ Reason string }

func (e *PublisherNotAllowedError) Error() string { return "relay publisher is not allowed" }

type Conn interface {
	Read(context.Context) (websocket.MessageType, []byte, error)
	Write(context.Context, websocket.MessageType, []byte) error
	SetReadLimit(int64)
	CloseNow() error
}

type WebSocketPublisher struct {
	RelayURL string
	Dial     func(context.Context, string) (Conn, error)
}

func (p *WebSocketPublisher) Publish(ctx context.Context, event nostr.Event) error {
	if p == nil || !validRelayURL(p.RelayURL) {
		return errors.New("invalid relay publisher configuration")
	}
	if !event.CheckID() || !event.VerifySignature() {
		return errors.New("invalid relay event")
	}

	dial := p.Dial
	if dial == nil {
		dial = func(ctx context.Context, relayURL string) (Conn, error) {
			conn, _, err := websocket.Dial(ctx, relayURL, nil)
			return conn, err
		}
	}
	conn, err := dial(ctx, p.RelayURL)
	if err != nil {
		return publisherTransportError(ctx, "connect to relay", err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(publisherResponseLimit)

	message, err := (&nostr.EventEnvelope{Event: event}).MarshalJSON()
	if err != nil {
		return errors.New("encode relay event")
	}
	if err := conn.Write(ctx, websocket.MessageText, message); err != nil {
		return publisherTransportError(ctx, "write relay event", err)
	}
	messageType, response, err := conn.Read(ctx)
	if err != nil {
		return publisherTransportError(ctx, "read relay response", err)
	}
	if messageType != websocket.MessageText {
		return errors.New("unexpected relay response")
	}
	if !validOKEnvelope(response) {
		return errors.New("decode relay response")
	}
	envelope, err := nostr.ParseMessage(string(response))
	if err != nil {
		return errors.New("decode relay response")
	}
	ok, valid := envelope.(*nostr.OKEnvelope)
	if !valid || ok.EventID != event.ID {
		return errors.New("unexpected relay response")
	}
	if ok.OK || strings.HasPrefix(ok.Reason, "duplicate:") {
		return nil
	}
	if ok.Reason == "restricted: publisher is not allowed" {
		return &PublisherNotAllowedError{Reason: ok.Reason}
	}
	return errors.New("relay rejected event")
}

func validOKEnvelope(message []byte) bool {
	var fields []json.RawMessage
	if err := json.Unmarshal(message, &fields); err != nil || len(fields) != 4 {
		return false
	}
	var label, eventID, reason *string
	var accepted *bool
	return json.Unmarshal(fields[0], &label) == nil && label != nil && *label == "OK" &&
		json.Unmarshal(fields[1], &eventID) == nil && eventID != nil &&
		json.Unmarshal(fields[2], &accepted) == nil && accepted != nil &&
		json.Unmarshal(fields[3], &reason) == nil && reason != nil
}

func validRelayURL(raw string) bool {
	relayURL, err := url.Parse(raw)
	return err == nil && (relayURL.Scheme == "ws" || relayURL.Scheme == "wss") && relayURL.Host != "" &&
		relayURL.User == nil && relayURL.Fragment == ""
}

func publisherTransportError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("%s failed", operation)
}
