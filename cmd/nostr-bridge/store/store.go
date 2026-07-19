// Package store persists nostr-bridge state.
package store

import (
	"context"
	"errors"
	"time"
)

// EventMapping is durable source metadata. Event JSON belongs in an incomplete
// publish outbox payload, never in this final mapping.
type EventMapping struct {
	Source       SourceRef
	NostrEventID string
	SourceKind   string
	AuthorPubKey string
	UpdatedAt    int64
}

type SourceScope struct {
	Provider string
	Account  string
}

type SourceRef struct {
	Scope SourceScope
	URI   string
}

var ErrClaimLost = errors.New("outbox claim lost")
var ErrInvalidLease = errors.New("outbox claim lease must be positive")
var ErrOutboxFull = errors.New("outbox limit reached")
var ErrPurgePending = errors.New("publisher purge pending or complete")
var ErrPurgeConflict = errors.New("publisher purge conflicts with existing unallow")
var ErrAuthorMismatch = errors.New("outbox author identity mismatch")
var ErrInvalidOutboxPayload = errors.New("invalid outbox payload")
var ErrSourceScopeMismatch = errors.New("source scope mismatch")

type OutboxOperation string

const (
	OutboxAllowPublisher   OutboxOperation = "allow-publisher"
	OutboxPublishEvent     OutboxOperation = "publish-event"
	OutboxUnallowPublisher OutboxOperation = "unallow-publisher"
)

type OutboxRequest struct {
	AggregateKey string
	Operation    OutboxOperation
	PubKey       string
	Payload      string
	AvailableAt  time.Time
}

type OutboxItem struct {
	ID           int64
	AggregateKey string
	Sequence     int64
	Operation    OutboxOperation
	PubKey       string
	Payload      string
	Attempts     int
	AvailableAt  time.Time
	LastError    string
	ClaimToken   string
	ClaimedUntil time.Time
}

type CursorUpdate struct {
	Name  string
	Value string
}
type EventEnqueueRequest struct {
	Mapping         EventMapping
	Event           OutboxRequest
	Limit           int64
	Cursor          *CursorUpdate
	SourceOperation string
}
type ReconciliationRequest struct {
	Scope   SourceScope
	Targets []string
	Events  []EventEnqueueRequest
	Limit   int64
}
type DeleteEnqueueRequest struct {
	Source SourceRef
	Event  OutboxRequest
	Limit  int64
	Cursor *CursorUpdate
}
type UpdateEnqueueRequest struct {
	Mapping         EventMapping
	Deletion        OutboxRequest
	Replacement     OutboxRequest
	SourceOperation string
	Limit           int64
	Cursor          *CursorUpdate
}
type PurgeRequest struct {
	PubKey, Payload string
	AvailableAt     time.Time
	Limit           int64
}

// OAuthSession is the encrypted, short-lived state needed to complete an OAuth callback.
type OAuthSession struct {
	State            string
	EncryptedPayload []byte
	ExpiresAt        int64
}

// OAuthToken is the encrypted OAuth token payload for one Bluesky account.
type OAuthToken struct {
	AccountDID       string
	EncryptedPayload []byte
	UpdatedAt        int64
}

type OAuthStore interface {
	SaveOAuthSession(context.Context, SourceScope, OAuthSession) error
	OAuthSessionByState(context.Context, SourceScope, string) (OAuthSession, error)
	DeleteOAuthSession(context.Context, SourceScope, string) error
	SaveOAuthToken(context.Context, SourceScope, OAuthToken) error
	OAuthTokenByAccountDID(context.Context, SourceScope, string) (OAuthToken, error)
}

type SyncDeliveryStore interface {
	EnqueueEvent(context.Context, EventEnqueueRequest) error
	EnqueueDelete(context.Context, DeleteEnqueueRequest) error
	EnqueueUpdate(context.Context, UpdateEnqueueRequest) error
	EventMappingBySourceURI(context.Context, SourceRef) (EventMapping, error)
	SourceOperationBySourceURI(context.Context, SourceRef) (string, error)
	SaveCursor(context.Context, SourceScope, string, string) error
	Cursor(context.Context, SourceScope, string) (string, error)
}

type TargetStore interface {
	SyncTargets(context.Context, SourceScope) ([]string, error)
}

type ReconciliationStore interface {
	TargetStore
	Reconcile(context.Context, ReconciliationRequest) error
}

// OutboxStore is the production-facing bridge delivery contract. It deliberately
// excludes legacy SaveEvent so mapping writes must be atomic with enqueue.
type OutboxStore interface {
	EnqueueEvent(context.Context, EventEnqueueRequest) error
	EnqueueDelete(context.Context, DeleteEnqueueRequest) error
	EnqueueUpdate(context.Context, UpdateEnqueueRequest) error
	EnqueuePurge(context.Context, PurgeRequest) error
	EventIDsByAuthor(context.Context, string) ([]string, error)
	SaveEventAndEnqueue(context.Context, EventMapping, OutboxRequest) error
	EventMappingBySourceURI(context.Context, SourceRef) (EventMapping, error)
	DeleteEventBySourceURI(context.Context, SourceRef) error
	SaveSourceOperation(context.Context, SourceRef, string) error
	SourceOperationBySourceURI(context.Context, SourceRef) (string, error)
	SaveCursor(context.Context, SourceScope, string, string) error
	Cursor(context.Context, SourceScope, string) (string, error)
	EnqueueOutbox(context.Context, OutboxRequest) error
	ClaimOutbox(context.Context, time.Time, time.Duration, int) ([]OutboxItem, error)
	CompleteOutbox(context.Context, int64, string, time.Time) error
	CompletePublisherRegistration(context.Context, int64, string, string, time.Time) error
	CompletePublisherUnregistration(context.Context, int64, string, time.Time, string) error
	RecoverPublisherRegistration(context.Context, int64, string, string, time.Time) error
	RetryOutbox(context.Context, int64, string, time.Time, time.Time, string) error
	OutboxCount(context.Context) (int64, error)
	SetPublisherRegistered(context.Context, string, time.Time) error
	ClearPublisherRegistration(context.Context, string) error
	PublisherRegistered(context.Context, string) (bool, error)
	ReplaceSyncTargets(context.Context, SourceScope, []string) error
	SyncTargets(context.Context, SourceScope) ([]string, error)
}

type DurableStore interface {
	OAuthStore
	OutboxStore
}
