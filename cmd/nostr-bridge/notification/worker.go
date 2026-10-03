package notification

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
	"github.com/nakatanakatana/mytools/internal/webpush"
)

const (
	// DefaultEvaluationInterval is the default frequency for evaluating bridge health.
	DefaultEvaluationInterval = 15 * time.Second

	// DefaultRemindInterval is the default duration before repeating an active alert.
	DefaultRemindInterval = 24 * time.Hour

	maxConcurrentPushes = 5
	perPushTimeout      = 5 * time.Second
)

// Snapshot represents a point-in-time operational snapshot evaluated by NotificationWorker.
type Snapshot struct {
	DispatcherRunning bool
	OutboxAtLimit     bool
	Providers         map[string]ProviderStatus
}

// ProviderStatus represents health and sync metrics for a single source provider.
type ProviderStatus struct {
	ReauthRequired         bool
	AccessTokenExpired     bool
	StreamConnected        bool
	TargetCount            int
	Degraded               bool
	AuthorizationAvailable bool
}

// PushClient abstracts the Web Push delivery client.
type PushClient interface {
	Send(ctx context.Context, sub webpush.Subscription, payload []byte, opts webpush.Options) error
}

// SubscriptionStore abstracts subscription storage operations needed by the worker.
type SubscriptionStore interface {
	ListSubscriptions(ctx context.Context) ([]store.StoredSubscription, error)
	DeleteSubscription(ctx context.Context, endpoint string) error
}

// NotificationData represents the data payload included in a push notification.
type NotificationData struct {
	URL string `json:"url"`
}

// NotificationPayload represents the standard JSON body sent to web push clients.
type NotificationPayload struct {
	Title string           `json:"title"`
	Body  string           `json:"body"`
	Tag   string           `json:"tag"`
	Data  NotificationData `json:"data"`
}

// Issue represents a detected operational issue.
type Issue struct {
	ID    string
	Title string
	Body  string
	Tag   string
}

// WorkerOptions configures a NotificationWorker.
type WorkerOptions struct {
	Store              SubscriptionStore
	PushClient         PushClient
	SnapshotProvider   func(ctx context.Context) (Snapshot, error)
	RemindInterval     time.Duration
	EvaluationInterval time.Duration
	Now                func() time.Time
	Logf               func(format string, v ...any)
}

// NotificationWorker evaluates operational health and sends push notifications.
type NotificationWorker struct {
	evalMu             sync.Mutex
	mu                 sync.Mutex
	store              SubscriptionStore
	pushClient         PushClient
	snapshotProvider   func(ctx context.Context) (Snapshot, error)
	remindInterval     time.Duration
	evaluationInterval time.Duration
	now                func() time.Time
	logfFunc           func(format string, v ...any)
	activeIssues       map[string]map[string]endpointDeliveryState
	streamDownTicks    map[string]int
}

type endpointDeliveryState struct {
	lastSuccess           time.Time
	nextAttempt           time.Time
	failureCount          uint
	permanentFailure      bool
	subscriptionUpdatedAt time.Time
}

// NewWorker creates a new NotificationWorker.
func NewWorker(opts WorkerOptions) *NotificationWorker {
	evalInterval := opts.EvaluationInterval
	if evalInterval <= 0 {
		evalInterval = DefaultEvaluationInterval
	}

	remindInterval := opts.RemindInterval
	if remindInterval <= 0 {
		remindInterval = DefaultRemindInterval
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	logf := opts.Logf
	if logf == nil {
		logf = log.Printf
	}

	return &NotificationWorker{
		store:              opts.Store,
		pushClient:         opts.PushClient,
		snapshotProvider:   opts.SnapshotProvider,
		remindInterval:     remindInterval,
		evaluationInterval: evalInterval,
		now:                now,
		logfFunc:           logf,
		activeIssues:       make(map[string]map[string]endpointDeliveryState),
		streamDownTicks:    make(map[string]int),
	}
}

func (w *NotificationWorker) logf(format string, v ...any) {
	if w.logfFunc != nil {
		w.logfFunc(format, v...)
	}
}

func formatProviderName(name string) string {
	switch strings.ToLower(name) {
	case "bluesky":
		return "Bluesky"
	case "mastodon":
		return "Mastodon"
	default:
		if len(name) == 0 {
			return ""
		}
		runes := []rune(name)
		return string(unicode.ToUpper(runes[0])) + string(runes[1:])
	}
}

func newIssue(id, title, body string) Issue {
	return Issue{ID: id, Title: title, Body: body, Tag: id}
}

func detectIssues(snapshot Snapshot) []Issue {
	var issues []Issue

	// Provider issues
	keys := slices.Sorted(maps.Keys(snapshot.Providers))

	for _, name := range keys {
		p := snapshot.Providers[name]
		lowerName := strings.ToLower(name)
		displayName := formatProviderName(name)

		if p.ReauthRequired {
			issues = append(issues, newIssue(
				fmt.Sprintf("provider:%s:reauth_required", lowerName),
				fmt.Sprintf("nostr-bridge: %s の再認証が必要です", displayName),
				"同期を再開するにはダッシュボードから再ログインしてください。",
			))
		}
		if p.AccessTokenExpired {
			issues = append(issues, newIssue(
				fmt.Sprintf("provider:%s:token_expired", lowerName),
				fmt.Sprintf("nostr-bridge: %s のトークン期限切れ", displayName),
				"アクセストークンが失効しました。",
			))
		}
		if p.TargetCount > 0 && !p.StreamConnected {
			issues = append(issues, newIssue(
				fmt.Sprintf("provider:%s:stream_disconnected", lowerName),
				fmt.Sprintf("nostr-bridge: %s のストリーム切断", displayName),
				"リアルタイムストリームが切断されています。",
			))
		}
		if p.Degraded {
			issues = append(issues, newIssue(
				fmt.Sprintf("provider:%s:degraded", lowerName),
				fmt.Sprintf("nostr-bridge: %s の同期異常", displayName),
				"同期処理でエラーが発生しています。",
			))
		}
	}

	// System issues
	if !snapshot.DispatcherRunning {
		issues = append(issues, newIssue(
			"system:dispatcher_stopped",
			"nostr-bridge: ディスパッチャー停止",
			"リレー送信ディスパッチャーが停止しています。",
		))
	}
	if snapshot.OutboxAtLimit {
		issues = append(issues, newIssue(
			"system:outbox_limit",
			"nostr-bridge: 送信待ち上限到達",
			"送信キューが上限に達しました。",
		))
	}

	return issues
}

// Evaluate performs a single health evaluation using the configured SnapshotProvider.
func (w *NotificationWorker) Evaluate(ctx context.Context) error {
	if w.snapshotProvider == nil {
		return errors.New("snapshot provider is required")
	}
	snapshot, err := w.snapshotProvider(ctx)
	if err != nil {
		return fmt.Errorf("get snapshot: %w", err)
	}
	return w.EvaluateSnapshot(ctx, snapshot)
}

// SendNotification sends an ad-hoc push notification to all stored subscriptions.
func (w *NotificationWorker) SendNotification(ctx context.Context, title, body, tag string) error {
	if w.store == nil {
		return errors.New("subscription store is required")
	}
	if w.pushClient == nil {
		return errors.New("push client is required")
	}

	payload, err := marshalNotificationPayload(title, body, tag)
	if err != nil {
		return fmt.Errorf("marshal notification payload: %w", err)
	}

	return w.sendPayloadToSubscriptions(ctx, payload, tag)
}

func (w *NotificationWorker) sendPayloadToSubscriptions(ctx context.Context, payload []byte, tag string) error {
	subs, err := w.store.ListSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}

	opts := webpush.Options{
		Topic:   webpush.NormalizeTopic(tag),
		Urgency: webpush.UrgencyHigh,
	}

	results := w.deliverToSubscriptions(ctx, subs, payload, opts)
	for _, err := range results {
		if err != nil && !webpush.IsSubscriptionExpired(err) {
			return errors.New("one or more push notifications could not be delivered")
		}
	}
	return nil
}

func (w *NotificationWorker) deliverToSubscriptions(
	ctx context.Context,
	subs []store.StoredSubscription,
	payload []byte,
	opts webpush.Options,
) map[string]error {
	if len(subs) == 0 {
		return nil
	}

	var (
		wg        sync.WaitGroup
		sem       = make(chan struct{}, maxConcurrentPushes)
		results   = make(map[string]error, len(subs))
		resultsMu sync.Mutex
	)

deliverLoop:
	for _, sub := range subs {
		select {
		case <-ctx.Done():
			break deliverLoop
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(s store.StoredSubscription) {
			defer func() {
				<-sem
				wg.Done()
			}()

			sendCtx, cancel := context.WithTimeout(ctx, perPushTimeout)
			defer cancel()

			ws := webpush.Subscription{
				Endpoint: s.Endpoint,
				Keys: webpush.SubscriptionKeys{
					P256dh: s.P256dh,
					Auth:   s.Auth,
				},
			}
			sendErr := w.pushClient.Send(sendCtx, ws, payload, opts)
			if sendErr != nil {
				if webpush.IsSubscriptionExpired(sendErr) {
					w.logf("subscription expired for endpoint %s: %v", maskEndpoint(s.Endpoint), sendErr)
					if delErr := w.store.DeleteSubscription(ctx, s.Endpoint); delErr != nil {
						w.logf("failed to delete expired subscription %s: %v", maskEndpoint(s.Endpoint), delErr)
					}
				} else {
					w.logf("failed to send push notification to %s: %v", maskEndpoint(s.Endpoint), sendErr)
				}
			}

			resultsMu.Lock()
			results[s.Endpoint] = sendErr
			resultsMu.Unlock()
		}(sub)
	}

	wg.Wait()
	return results
}

// EvaluateSnapshot evaluates a specific snapshot against active issues and sends notifications.
func (w *NotificationWorker) EvaluateSnapshot(ctx context.Context, snapshot Snapshot) error {
	w.evalMu.Lock()
	defer w.evalMu.Unlock()

	w.mu.Lock()
	detected := w.filterTransientStreamIssues(snapshot, detectIssues(snapshot))
	currentIDs := make(map[string]bool, len(detected))
	for _, issue := range detected {
		currentIDs[issue.ID] = true
	}

	// Recovery: remove issues no longer detected
	maps.DeleteFunc(w.activeIssues, func(id string, _ map[string]endpointDeliveryState) bool {
		return !currentIDs[id]
	})
	w.mu.Unlock()
	return w.sendNotifications(ctx, detected)
}

func (w *NotificationWorker) filterTransientStreamIssues(snapshot Snapshot, issues []Issue) []Issue {
	suppressed := make(map[string]struct{})
	for name, provider := range snapshot.Providers {
		name = strings.ToLower(name)
		issueID := fmt.Sprintf("provider:%s:stream_disconnected", name)
		if provider.TargetCount == 0 || provider.StreamConnected {
			delete(w.streamDownTicks, name)
			continue
		}
		w.streamDownTicks[name]++
		if w.streamDownTicks[name] < 2 {
			suppressed[issueID] = struct{}{}
		}
	}

	return slices.DeleteFunc(issues, func(issue Issue) bool {
		_, skip := suppressed[issue.ID]
		return skip
	})
}

func (w *NotificationWorker) sendNotifications(ctx context.Context, issues []Issue) error {
	if len(issues) == 0 || w.store == nil || w.pushClient == nil {
		return nil
	}

	subs, err := w.store.ListSubscriptions(ctx)
	if err != nil {
		return fmt.Errorf("list subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}

	expiredEndpoints := make(map[string]bool)
	currentSubscriptions := make(map[string]time.Time, len(subs))
	for _, sub := range subs {
		currentSubscriptions[sub.Endpoint] = sub.UpdatedAt
	}
	now := w.now()

	for _, issue := range issues {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		payload, err := marshalNotificationPayload(issue.Title, issue.Body, issue.Tag)
		if err != nil {
			w.logf("failed to marshal notification payload: %v", err)
			continue
		}

		opts := webpush.Options{
			Topic:   webpush.NormalizeTopic(issue.Tag),
			Urgency: webpush.UrgencyHigh,
		}

		w.mu.Lock()
		states := w.activeIssues[issue.ID]
		if states == nil {
			states = make(map[string]endpointDeliveryState)
			w.activeIssues[issue.ID] = states
		}
		maps.DeleteFunc(states, func(endpoint string, state endpointDeliveryState) bool {
			updatedAt, exists := currentSubscriptions[endpoint]
			return !exists || !updatedAt.Equal(state.subscriptionUpdatedAt)
		})
		eligible := make([]store.StoredSubscription, 0, len(subs))
		for _, sub := range subs {
			if expiredEndpoints[sub.Endpoint] {
				continue
			}
			state, exists := states[sub.Endpoint]
			if isEligible(state, exists, now, w.remindInterval) {
				eligible = append(eligible, sub)
			}
		}
		w.mu.Unlock()

		results := w.deliverToSubscriptions(ctx, eligible, payload, opts)
		w.mu.Lock()
		for endpoint, sendErr := range results {
			if webpush.IsSubscriptionExpired(sendErr) {
				expiredEndpoints[endpoint] = true
				delete(states, endpoint)
				continue
			}
			state := states[endpoint]
			if sendErr == nil {
				state.lastSuccess = now
				state.nextAttempt = time.Time{}
				state.failureCount = 0
			} else {
				state.failureCount++
				state.nextAttempt = now.Add(w.retryDelay(state.failureCount, sendErr))
				state.permanentFailure = webpush.IsPermanentPushError(sendErr)
			}
			state.subscriptionUpdatedAt = currentSubscriptions[endpoint]
			states[endpoint] = state
		}
		w.mu.Unlock()
	}

	return nil
}

func (w *NotificationWorker) retryDelay(failureCount uint, err error) time.Duration {
	delay := w.evaluationInterval
	for attempt := uint(1); attempt < failureCount && delay < 15*time.Minute; attempt++ {
		delay *= 2
	}
	delay = min(delay, 15*time.Minute)

	if retryAfter, ok := errors.AsType[interface {
		error
		RetryAfter() time.Duration
	}](err); ok {
		delay = max(delay, retryAfter.RetryAfter())
	}
	return delay
}


func marshalNotificationPayload(title, body, tag string) ([]byte, error) {
	return json.Marshal(NotificationPayload{
		Title: title,
		Body:  body,
		Tag:   tag,
		Data: NotificationData{
			URL: "/",
		},
	})
}

func isEligible(state endpointDeliveryState, exists bool, now time.Time, remindInterval time.Duration) bool {
	if !exists {
		return true
	}
	if state.permanentFailure {
		return false
	}
	if !state.nextAttempt.IsZero() {
		return !now.Before(state.nextAttempt)
	}
	return state.lastSuccess.IsZero() || now.Sub(state.lastSuccess) >= remindInterval
}

func maskEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "push endpoint [invalid]"
	}
	hash := sha256.Sum256([]byte(endpoint))
	return fmt.Sprintf("push endpoint [%x]", hash[:4])
}

// Run runs the evaluation loop until ctx is canceled.
func (w *NotificationWorker) Run(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}

	interval := w.evaluationInterval
	if interval <= 0 {
		interval = DefaultEvaluationInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	evaluate := func() bool {
		if err := w.Evaluate(ctx); err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				return false
			}
			w.logf("notification evaluation failed: %v", err)
		}
		return true
	}

	if !evaluate() {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !evaluate() {
				return nil
			}
		}
	}
}
