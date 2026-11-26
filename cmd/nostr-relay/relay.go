package main

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"strconv"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
	relaystore "github.com/nakatanakatana/mytools/cmd/nostr-relay/store"
	appotel "github.com/nakatanakatana/mytools/internal/otel"
	"go.opentelemetry.io/otel/trace"
)

const softwareURL = "https://github.com/nakatanakatana/mytools"

type RelayResources struct {
	Relay             *khatru.Relay
	Handler           http.Handler // combined handler retained for in-process compatibility
	ProtocolHandler   http.Handler
	ManagementHandler http.Handler
	Close             func() error
}

func NewRelay(ctx context.Context, cfg Config) (RelayResources, error) {
	switch cfg.Mode {
	case "", ModePublicMemory:
		return newPublicMemoryRelay(cfg)
	case ModePrivateSQLite:
		return newPrivateSQLiteRelay(cfg)
	default:
		return RelayResources{}, fmt.Errorf("unsupported relay mode %q", cfg.Mode)
	}
}

func newPublicMemoryRelay(cfg Config) (RelayResources, error) {
	store := &slicestore.SliceStore{}
	if err := store.Init(); err != nil {
		return RelayResources{}, err
	}
	relay := configuredRelay(cfg, []int{1, 11})
	relay.UseEventstore(store, cfg.MaxQueryLimit)
	instrumented := appotel.NewHTTPHandler(relay, "nostr-relay")
	return RelayResources{Relay: relay, Handler: instrumented, ProtocolHandler: instrumented, Close: func() error { store.Close(); return nil }}, nil
}

func newPrivateSQLiteRelay(cfg Config) (RelayResources, error) {
	store, err := relaystore.Open(cfg.DatabasePath)
	if err != nil {
		return RelayResources{}, err
	}
	relay := configuredRelay(cfg, []int{1, 9, 11, 42, 86, 98})
	relay.ServiceURL = cfg.ServiceURL

	readers := make(map[nostr.PubKey]struct{}, len(cfg.ReaderPubkeys))
	for _, encoded := range cfg.ReaderPubkeys {
		pubkey, err := nostr.PubKeyFromHex(encoded)
		if err != nil {
			_ = store.Close()
			return RelayResources{}, fmt.Errorf("decode reader pubkey: %w", err)
		}
		readers[pubkey] = struct{}{}
	}
	relay.OnRequest = func(ctx context.Context, _ nostr.Filter) (bool, string) {
		pubkey, ok := khatru.GetAuthed(ctx)
		if !ok {
			return true, "auth-required: authentication required"
		}
		if _, ok := readers[pubkey]; !ok {
			return true, "restricted: reader is not allowed"
		}
		return false, ""
	}
	relay.OnEvent = func(ctx context.Context, event nostr.Event) (bool, string) {
		ctx = withConnectionTraceContext(ctx)
		if !event.CheckID() || !event.VerifySignature() {
			return true, "invalid: event signature is invalid"
		}
		allowed, err := store.PublisherAllowed(ctx, event.PubKey)
		if err != nil {
			return true, "error: publisher authorization failed"
		}
		if !allowed {
			return true, "restricted: publisher is not allowed"
		}
		return false, ""
	}
	relay.StoreEvent = func(ctx context.Context, event nostr.Event) error {
		return persistEventAndApplyDeletion(withConnectionTraceContext(ctx), store, event)
	}
	relay.ReplaceEvent = func(ctx context.Context, event nostr.Event) error {
		return store.SaveEvent(withConnectionTraceContext(ctx), event)
	}
	relay.QueryStored = func(ctx context.Context, filter nostr.Filter) iter.Seq[nostr.Event] {
		ctx = withConnectionTraceContext(ctx)
		if cfg.MaxQueryLimit > 0 && (filter.Limit == 0 || filter.Limit > cfg.MaxQueryLimit) {
			filter.Limit = cfg.MaxQueryLimit
		}
		events, err := store.QueryEvents(ctx, filter)
		if err != nil {
			slog.ErrorContext(ctx, "query stored events", "error", err)
			return func(func(nostr.Event) bool) {}
		}
		return events
	}
	protocolHandler := appotel.NewHTTPHandler(relay, "nostr-relay")
	if cfg.AdminPubkey == "" {
		return RelayResources{Relay: relay, Handler: protocolHandler, ProtocolHandler: protocolHandler, Close: store.Close}, nil
	}
	admin, err := nostr.PubKeyFromHex(cfg.AdminPubkey)
	if err != nil {
		_ = store.Close()
		return RelayResources{}, fmt.Errorf("decode admin pubkey: %w", err)
	}
	auth := NIP98Validator{AdminPubKey: admin, ExpectedURL: cfg.ServiceURL, ReplayStore: store}
	handler := appotel.NewHTTPHandler(NewManagementHandler(relay, store, auth), "nostr-relay")
	management := appotel.NewHTTPHandler(NewManagementHandler(http.NotFoundHandler(), store, auth), "nostr-relay-management")
	return RelayResources{Relay: relay, Handler: handler, ProtocolHandler: protocolHandler, ManagementHandler: management, Close: store.Close}, nil
}

func configuredRelay(cfg Config, nips []int) *khatru.Relay {
	relay := khatru.NewRelay()
	relay.Info.Name = cfg.Name
	relay.Info.Description = cfg.Description
	relay.Info.Software = softwareURL
	relay.Info.Version = "dev"
	relay.Info.SupportedNIPs = nil
	strNIPs := make([]string, len(nips))
	for i, nip := range nips {
		strNIPs[i] = strconv.Itoa(nip)
	}
	relay.Info.AddSupportedNIPs(strNIPs)
	return relay
}

func withConnectionTraceContext(ctx context.Context) context.Context {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	if conn := khatru.GetConnection(ctx); conn != nil && conn.Request != nil {
		if spanCtx := trace.SpanContextFromContext(conn.Request.Context()); spanCtx.IsValid() {
			return trace.ContextWithSpanContext(ctx, spanCtx)
		}
	}
	return ctx
}
