package relayclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/coder/websocket"
)

func TestPublisherSendsEventUnchangedAndAcceptsOK(t *testing.T) {
	event := signedPublisherEvent(t)
	server := publisherServer(t, func(conn *websocket.Conn) {
		_, message, err := conn.Read(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		envelope, err := nostr.ParseMessage(string(message))
		if err != nil {
			t.Error(err)
			return
		}
		got, ok := envelope.(*nostr.EventEnvelope)
		if !ok || got.SubscriptionID != nil || !reflect.DeepEqual(got.Event, event) {
			t.Errorf("EVENT = %#v, want unchanged %#v", envelope, event)
			return
		}
		writePublisherMessage(t, conn, &nostr.OKEnvelope{EventID: event.ID, OK: true})
	})
	defer server.Close()

	if err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

func TestPublisherAcceptsDuplicateResponse(t *testing.T) {
	event := signedPublisherEvent(t)
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "false", true: "true"}[accepted], func(t *testing.T) {
			server := publisherServer(t, func(conn *websocket.Conn) {
				_, _, _ = conn.Read(context.Background())
				writePublisherMessage(t, conn, &nostr.OKEnvelope{EventID: event.ID, OK: accepted, Reason: "duplicate: already stored"})
			})
			defer server.Close()
			if err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(context.Background(), event); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPublisherRejectsInvalidRelayResponses(t *testing.T) {
	event := signedPublisherEvent(t)
	other := signedPublisherEvent(t)
	tests := []struct {
		name    string
		message string
	}{
		{name: "rejection", message: nostr.OKEnvelope{EventID: event.ID, Reason: "blocked: policy"}.String()},
		{name: "other event", message: nostr.OKEnvelope{EventID: other.ID, OK: true}.String()},
		{name: "malformed", message: `["OK"]`},
		{name: "extra field", message: `["OK","` + event.ID.Hex() + `",false,"duplicate: already stored",true]`},
		{name: "null event ID", message: `["OK",null,true,""]`},
		{name: "null accepted", message: `["OK","` + event.ID.Hex() + `",null,"duplicate: already stored"]`},
		{name: "null reason", message: `["OK","` + event.ID.Hex() + `",true,null]`},
		{name: "unexpected", message: `["NOTICE","try later"]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := publisherServer(t, func(conn *websocket.Conn) {
				_, _, _ = conn.Read(context.Background())
				if err := conn.Write(context.Background(), websocket.MessageText, []byte(test.message)); err != nil {
					t.Error(err)
				}
			})
			defer server.Close()
			if err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(context.Background(), event); err == nil {
				t.Fatal("expected response error")
			}
		})
	}
}

func TestPublisherRejectsOversizedResponse(t *testing.T) {
	event := signedPublisherEvent(t)
	server := publisherServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background())
		message := []byte(`["NOTICE","` + strings.Repeat("x", publisherResponseLimit) + `"]`)
		_ = conn.Write(context.Background(), websocket.MessageText, message)
	})
	defer server.Close()
	if err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(context.Background(), event); err == nil {
		t.Fatal("expected oversized response error")
	}
}

func TestPublisherClassifiesPublisherAllowlistDrift(t *testing.T) {
	event := signedPublisherEvent(t)
	server := publisherServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background())
		_ = conn.Write(context.Background(), websocket.MessageText, []byte((&nostr.OKEnvelope{EventID: event.ID, Reason: "restricted: publisher is not allowed"}).String()))
	})
	defer server.Close()
	err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(context.Background(), event)
	var drift *PublisherNotAllowedError
	if !errors.As(err, &drift) {
		t.Fatalf("Publish() error = %T %v", err, err)
	}
}

func TestPublisherDoesNotClassifyOtherRestrictedReasonsAsPublisherDrift(t *testing.T) {
	for _, reason := range []string{"restricted: event kind not allowed", "restricted: reader is not allowed", "restricted: publisher is not allowed today"} {
		t.Run(reason, func(t *testing.T) {
			event := signedPublisherEvent(t)
			server := publisherServer(t, func(conn *websocket.Conn) {
				_, _, _ = conn.Read(context.Background())
				_ = conn.Write(context.Background(), websocket.MessageText, []byte((&nostr.OKEnvelope{EventID: event.ID, Reason: reason}).String()))
			})
			defer server.Close()
			err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(context.Background(), event)
			var drift *PublisherNotAllowedError
			if errors.As(err, &drift) {
				t.Fatalf("reason %q classified as drift", reason)
			}
		})
	}
}

func TestPublisherRejectsBinaryOK(t *testing.T) {
	event := signedPublisherEvent(t)
	server := publisherServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background())
		message := []byte((&nostr.OKEnvelope{EventID: event.ID, OK: true}).String())
		if err := conn.Write(context.Background(), websocket.MessageBinary, message); err != nil {
			t.Error(err)
		}
	})
	defer server.Close()
	if err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(context.Background(), event); err == nil {
		t.Fatal("expected binary response error")
	}
}

func TestPublisherReportsDisconnect(t *testing.T) {
	event := signedPublisherEvent(t)
	server := publisherServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background())
	})
	defer server.Close()
	if err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(context.Background(), event); err == nil {
		t.Fatal("expected disconnect error")
	}
}

func TestPublisherHonorsContextDeadline(t *testing.T) {
	event := signedPublisherEvent(t)
	server := publisherServer(t, func(conn *websocket.Conn) {
		_, _, _ = conn.Read(context.Background())
		time.Sleep(200 * time.Millisecond)
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := (&WebSocketPublisher{RelayURL: publisherURL(server.URL)}).Publish(ctx, event)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
}

func TestPublisherRejectsInvalidInputBeforeDial(t *testing.T) {
	event := signedPublisherEvent(t)
	badID := event
	badID.ID = nostr.ID{}
	badSignature := event
	badSignature.Sig = [64]byte{}
	for _, test := range []struct {
		name      string
		publisher *WebSocketPublisher
		event     nostr.Event
	}{
		{name: "nil publisher", publisher: nil, event: event},
		{name: "HTTP URL", publisher: &WebSocketPublisher{RelayURL: "https://relay.example"}, event: event},
		{name: "relative URL", publisher: &WebSocketPublisher{RelayURL: "/relay"}, event: event},
		{name: "bad ID", publisher: &WebSocketPublisher{RelayURL: "wss://relay.example"}, event: badID},
		{name: "bad signature", publisher: &WebSocketPublisher{RelayURL: "wss://relay.example"}, event: badSignature},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			if test.publisher != nil {
				test.publisher.Dial = func(context.Context, string) (Conn, error) {
					calls++
					return nil, errors.New("unexpected dial")
				}
			}
			if err := test.publisher.Publish(context.Background(), test.event); err == nil {
				t.Fatal("expected validation error")
			}
			if calls != 0 {
				t.Fatalf("dial calls = %d, want 0", calls)
			}
		})
	}
}

func TestPublisherValidatesRelayURL(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want bool
	}{
		{raw: "ws://relay.example", want: true},
		{raw: "wss://relay.example/path?x=1", want: true},
		{raw: "ws://user@relay.example", want: false},
		{raw: "wss://relay.example/#fragment", want: false},
	} {
		if got := validRelayURL(test.raw); got != test.want {
			t.Errorf("validRelayURL(%q) = %v, want %v", test.raw, got, test.want)
		}
	}
}

func TestPublisherClosesConnectionAfterWriteFailure(t *testing.T) {
	event := signedPublisherEvent(t)
	conn := &publisherTestConn{writeErr: errors.New("write failed")}
	publisher := &WebSocketPublisher{
		RelayURL: "wss://relay.example",
		Dial: func(context.Context, string) (Conn, error) {
			return conn, nil
		},
	}
	if err := publisher.Publish(context.Background(), event); err == nil {
		t.Fatal("expected write error")
	}
	if !conn.closed {
		t.Fatal("connection was not closed")
	}
}

func publisherServer(t *testing.T, handle func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.CloseNow() }()
		handle(conn)
	}))
}

func writePublisherMessage(t *testing.T, conn *websocket.Conn, envelope nostr.Envelope) {
	t.Helper()
	message, err := envelope.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(context.Background(), websocket.MessageText, message); err != nil {
		t.Error(err)
	}
}

func signedPublisherEvent(t *testing.T) nostr.Event {
	t.Helper()
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: 1, Tags: nostr.Tags{{"t", "publisher-test"}}, Content: strings.Repeat("event payload ", 2)}
	if err := event.Sign(nostr.Generate()); err != nil {
		t.Fatal(err)
	}
	return event
}

func publisherURL(serverURL string) string { return "ws" + strings.TrimPrefix(serverURL, "http") }

type publisherTestConn struct {
	writeErr error
	closed   bool
}

func (*publisherTestConn) Read(context.Context) (websocket.MessageType, []byte, error) {
	return 0, nil, errors.New("unexpected read")
}

func (conn *publisherTestConn) Write(context.Context, websocket.MessageType, []byte) error {
	return conn.writeErr
}

func (*publisherTestConn) SetReadLimit(int64) {}

func (conn *publisherTestConn) CloseNow() error {
	conn.closed = true
	return nil
}
