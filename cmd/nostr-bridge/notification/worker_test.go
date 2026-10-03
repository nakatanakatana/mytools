package notification_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/notification"
	"github.com/nakatanakatana/mytools/cmd/nostr-bridge/store"
	"github.com/nakatanakatana/mytools/internal/webpush"
)

type mockStore struct {
	mu            sync.Mutex
	subscriptions []store.StoredSubscription
	deleted       []string
	listErr       error
	deleteErr     error
}

func (m *mockStore) ListSubscriptions(ctx context.Context) ([]store.StoredSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	res := make([]store.StoredSubscription, len(m.subscriptions))
	copy(res, m.subscriptions)
	return res, nil
}

func (m *mockStore) DeleteSubscription(ctx context.Context, endpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleted = append(m.deleted, endpoint)
	filtered := m.subscriptions[:0]
	for _, s := range m.subscriptions {
		if s.Endpoint != endpoint {
			filtered = append(filtered, s)
		}
	}
	m.subscriptions = filtered
	return nil
}

func (m *mockStore) DeletedEndpoints() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]string, len(m.deleted))
	copy(res, m.deleted)
	return res
}

type sentPush struct {
	sub     webpush.Subscription
	payload []byte
	opts    webpush.Options
}

type mockPushClient struct {
	mu      sync.Mutex
	sent    []sentPush
	sendErr func(sub webpush.Subscription) error
}

func (m *mockPushClient) Send(ctx context.Context, sub webpush.Subscription, payload []byte, opts webpush.Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		if err := m.sendErr(sub); err != nil {
			return err
		}
	}
	m.sent = append(m.sent, sentPush{sub: sub, payload: payload, opts: opts})
	return nil
}

func (m *mockPushClient) Sent() []sentPush {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]sentPush, len(m.sent))
	copy(res, m.sent)
	return res
}

func (m *mockPushClient) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = nil
}

func makeTestSubscription(endpoint string) store.StoredSubscription {
	return store.StoredSubscription{
		Endpoint:  endpoint,
		P256dh:    "dummy-p256dh",
		Auth:      "dummy-auth",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
}

func healthySnapshot() notification.Snapshot {
	return notification.Snapshot{
		DispatcherRunning: true,
		OutboxAtLimit:     false,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {
				AuthorizationAvailable: true,
				StreamConnected:        true,
				TargetCount:            2,
			},
		},
	}
}

func TestInitialDegradationTriggersImmediatePushNotification(t *testing.T) {
	testCases := []struct {
		name          string
		snapshot      notification.Snapshot
		expectedTag   string
		expectedTitle string
		expectedBody  string
	}{
		{
			name: "provider reauth required",
			snapshot: notification.Snapshot{
				DispatcherRunning: true,
				Providers: map[string]notification.ProviderStatus{
					"bluesky": {ReauthRequired: true},
				},
			},
			expectedTag:   "provider:bluesky:reauth_required",
			expectedTitle: "nostr-bridge: Bluesky の再認証が必要です",
			expectedBody:  "同期を再開するにはダッシュボードから再ログインしてください。",
		},
		{
			name: "provider token expired",
			snapshot: notification.Snapshot{
				DispatcherRunning: true,
				Providers: map[string]notification.ProviderStatus{
					"bluesky": {AccessTokenExpired: true},
				},
			},
			expectedTag:   "provider:bluesky:token_expired",
			expectedTitle: "nostr-bridge: Bluesky のトークン期限切れ",
			expectedBody:  "アクセストークンが失効しました。",
		},
		{
			name: "provider stream disconnected",
			snapshot: notification.Snapshot{
				DispatcherRunning: true,
				Providers: map[string]notification.ProviderStatus{
					"bluesky": {TargetCount: 3, StreamConnected: false},
				},
			},
			expectedTag:   "provider:bluesky:stream_disconnected",
			expectedTitle: "nostr-bridge: Bluesky のストリーム切断",
			expectedBody:  "リアルタイムストリームが切断されています。",
		},
		{
			name: "provider degraded",
			snapshot: notification.Snapshot{
				DispatcherRunning: true,
				Providers: map[string]notification.ProviderStatus{
					"bluesky": {Degraded: true},
				},
			},
			expectedTag:   "provider:bluesky:degraded",
			expectedTitle: "nostr-bridge: Bluesky の同期異常",
			expectedBody:  "同期処理でエラーが発生しています。",
		},
		{
			name: "system dispatcher stopped",
			snapshot: notification.Snapshot{
				DispatcherRunning: false,
			},
			expectedTag:   "system:dispatcher_stopped",
			expectedTitle: "nostr-bridge: ディスパッチャー停止",
			expectedBody:  "リレー送信ディスパッチャーが停止しています。",
		},
		{
			name: "system outbox limit",
			snapshot: notification.Snapshot{
				DispatcherRunning: true,
				OutboxAtLimit:     true,
			},
			expectedTag:   "system:outbox_limit",
			expectedTitle: "nostr-bridge: 送信待ち上限到達",
			expectedBody:  "送信キューが上限に達しました。",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			st := &mockStore{
				subscriptions: []store.StoredSubscription{
					makeTestSubscription("https://push.example.com/sub1"),
				},
			}
			push := &mockPushClient{}
			now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

			worker := notification.NewWorker(notification.WorkerOptions{
				Store:      st,
				PushClient: push,
				Now:        func() time.Time { return now },
			})

			ctx := context.Background()
			if err := worker.EvaluateSnapshot(ctx, tc.snapshot); err != nil {
				t.Fatalf("EvaluateSnapshot failed: %v", err)
			}
			if tc.name == "provider stream disconnected" {
				if err := worker.EvaluateSnapshot(ctx, tc.snapshot); err != nil {
					t.Fatalf("second disconnected evaluation failed: %v", err)
				}
			}

			sent := push.Sent()
			if len(sent) != 1 {
				t.Fatalf("expected 1 push sent, got %d", len(sent))
			}

			if sent[0].sub.Endpoint != "https://push.example.com/sub1" {
				t.Errorf("endpoint = %q, want %q", sent[0].sub.Endpoint, "https://push.example.com/sub1")
			}

			var payload struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Tag   string `json:"tag"`
				Data  struct {
					URL string `json:"url"`
				} `json:"data"`
			}
			if err := json.Unmarshal(sent[0].payload, &payload); err != nil {
				t.Fatalf("failed to unmarshal payload: %v", err)
			}

			if payload.Tag != tc.expectedTag {
				t.Errorf("tag = %q, want %q", payload.Tag, tc.expectedTag)
			}
			if payload.Title != tc.expectedTitle {
				t.Errorf("title = %q, want %q", payload.Title, tc.expectedTitle)
			}
			if payload.Body != tc.expectedBody {
				t.Errorf("body = %q, want %q", payload.Body, tc.expectedBody)
			}
			if payload.Data.URL != "/" {
				t.Errorf("data.url = %q, want %q", payload.Data.URL, "/")
			}
		})
	}
}

func TestContinuingDegradationDoesNotTriggerDuplicateNotificationBeforeRemindInterval(t *testing.T) {
	st := &mockStore{
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/sub1"),
		},
	}
	push := &mockPushClient{}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:          st,
		PushClient:     push,
		RemindInterval: 24 * time.Hour,
		Now:            func() time.Time { return now },
	})

	ctx := context.Background()
	degraded := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {Degraded: true},
		},
	}

	// 1st evaluation at t=0
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 1 {
		t.Fatalf("expected 1 push sent initially, got %d", len(push.Sent()))
	}

	// 2nd evaluation at t=1h (before 24h remind interval)
	now = now.Add(1 * time.Hour)
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 1 {
		t.Fatalf("expected still 1 push sent after 1h, got %d", len(push.Sent()))
	}

	// 3rd evaluation at t=23h (still before 24h remind interval)
	now = now.Add(22 * time.Hour)
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 1 {
		t.Fatalf("expected still 1 push sent after 23h, got %d", len(push.Sent()))
	}
}

func TestReminderTriggersAfterRemindInterval(t *testing.T) {
	st := &mockStore{
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/sub1"),
		},
	}
	push := &mockPushClient{}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:          st,
		PushClient:     push,
		RemindInterval: 24 * time.Hour,
		Now:            func() time.Time { return now },
	})

	ctx := context.Background()
	degraded := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {Degraded: true},
		},
	}

	// Initial alert at t=0
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 1 {
		t.Fatalf("expected 1 push sent initially, got %d", len(push.Sent()))
	}

	// Advance time past remind interval (24 hours)
	now = now.Add(24 * time.Hour)
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 2 {
		t.Fatalf("expected 2 pushes sent after 24h reminder, got %d", len(push.Sent()))
	}

	// Advance another 24 hours
	now = now.Add(24 * time.Hour)
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 3 {
		t.Fatalf("expected 3 pushes sent after 48h reminder, got %d", len(push.Sent()))
	}
}

func TestRecoveryClearsIssueSoSubsequentDegradationTriggersImmediately(t *testing.T) {
	st := &mockStore{
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/sub1"),
		},
	}
	push := &mockPushClient{}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:          st,
		PushClient:     push,
		RemindInterval: 24 * time.Hour,
		Now:            func() time.Time { return now },
	})

	ctx := context.Background()
	degraded := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {Degraded: true},
		},
	}

	// Initial alert at t=0
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 1 {
		t.Fatalf("expected 1 push sent initially, got %d", len(push.Sent()))
	}

	// Healthy snapshot at t=1h (recovers)
	now = now.Add(1 * time.Hour)
	healthy := healthySnapshot()
	if err := worker.EvaluateSnapshot(ctx, healthy); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 1 {
		t.Fatalf("expected still 1 push sent on recovery, got %d", len(push.Sent()))
	}

	// Subsequent degradation at t=2h (well before 24h)
	now = now.Add(1 * time.Hour)
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 2 {
		t.Fatalf("expected 2 pushes sent because recovered issue re-triggered immediately, got %d", len(push.Sent()))
	}
}

func TestExpiredSubscriptionCausesDeleteSubscription(t *testing.T) {
	st := &mockStore{
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/expired-sub"),
		},
	}
	push := &mockPushClient{
		sendErr: func(sub webpush.Subscription) error {
			return webpush.ErrSubscriptionExpired
		},
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:      st,
		PushClient: push,
		Now:        func() time.Time { return now },
	})

	ctx := context.Background()
	degraded := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {ReauthRequired: true},
		},
	}

	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}

	deleted := st.DeletedEndpoints()
	if len(deleted) != 1 || deleted[0] != "https://push.example.com/expired-sub" {
		t.Fatalf("expected endpoint %q to be deleted, got %v", "https://push.example.com/expired-sub", deleted)
	}
}

func TestMultipleSubscriptionsReceiveNotificationsAndSingleFailureDoesNotAbort(t *testing.T) {
	st := &mockStore{
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/sub-fail"),
			makeTestSubscription("https://push.example.com/sub-ok-1"),
			makeTestSubscription("https://push.example.com/sub-ok-2"),
		},
	}
	push := &mockPushClient{
		sendErr: func(sub webpush.Subscription) error {
			if sub.Endpoint == "https://push.example.com/sub-fail" {
				return errors.New("network error or service unavailable")
			}
			return nil
		},
	}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:      st,
		PushClient: push,
		Now:        func() time.Time { return now },
	})

	ctx := context.Background()
	degraded := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {Degraded: true},
		},
	}

	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}

	sent := push.Sent()
	if len(sent) != 2 {
		t.Fatalf("expected 2 successful pushes sent, got %d", len(sent))
	}
	endpoints := map[string]bool{}
	for _, s := range sent {
		endpoints[s.sub.Endpoint] = true
	}
	if !endpoints["https://push.example.com/sub-ok-1"] || !endpoints["https://push.example.com/sub-ok-2"] {
		t.Fatalf("unexpected endpoints received push: %v", endpoints)
	}
}

func TestPartialDeliveryRetriesOnlyFailedSubscription(t *testing.T) {
	failedEndpoint := "https://push.example.com/sub-fail"
	successfulEndpoint := "https://push.example.com/sub-ok"
	st := &mockStore{subscriptions: []store.StoredSubscription{
		makeTestSubscription(failedEndpoint),
		makeTestSubscription(successfulEndpoint),
	}}
	attempts := make(map[string]int)
	push := &mockPushClient{sendErr: func(sub webpush.Subscription) error {
		attempts[sub.Endpoint]++
		if sub.Endpoint == failedEndpoint && attempts[sub.Endpoint] == 1 {
			return errors.New("temporary push service failure")
		}
		return nil
	}}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	worker := notification.NewWorker(notification.WorkerOptions{
		Store:              st,
		PushClient:         push,
		Now:                func() time.Time { return now },
		EvaluationInterval: 15 * time.Second,
	})
	snapshot := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {Degraded: true},
		},
	}

	if err := worker.EvaluateSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("first EvaluateSnapshot failed: %v", err)
	}
	now = now.Add(15 * time.Second)
	if err := worker.EvaluateSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("second EvaluateSnapshot failed: %v", err)
	}

	if attempts[failedEndpoint] != 2 {
		t.Fatalf("failed endpoint attempts = %d, want 2", attempts[failedEndpoint])
	}
	if attempts[successfulEndpoint] != 1 {
		t.Fatalf("successful endpoint attempts = %d, want 1", attempts[successfulEndpoint])
	}
	if got := len(push.Sent()); got != 2 {
		t.Fatalf("successful pushes = %d, want 2", got)
	}
}

type permanentPushError struct{}

func (permanentPushError) Error() string   { return "permanent push request failure" }
func (permanentPushError) Permanent() bool { return true }

func TestPermanentPushFailureIsNotRetriedUntilSubscriptionChanges(t *testing.T) {
	endpoint := "https://push.example.com/sub-permanent-error"
	st := &mockStore{subscriptions: []store.StoredSubscription{makeTestSubscription(endpoint)}}
	attempts := 0
	push := &mockPushClient{sendErr: func(webpush.Subscription) error {
		attempts++
		return permanentPushError{}
	}}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	worker := notification.NewWorker(notification.WorkerOptions{
		Store:              st,
		PushClient:         push,
		Now:                func() time.Time { return now },
		EvaluationInterval: 15 * time.Second,
	})
	snapshot := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {Degraded: true},
		},
	}

	if err := worker.EvaluateSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("first EvaluateSnapshot: %v", err)
	}
	now = now.Add(24 * time.Hour)
	if err := worker.EvaluateSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("second EvaluateSnapshot: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("permanent failure attempts = %d, want 1 while issue remains active", attempts)
	}

	st.mu.Lock()
	st.subscriptions[0].UpdatedAt = now
	st.mu.Unlock()
	if err := worker.EvaluateSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("EvaluateSnapshot after subscription change: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("permanent failure attempts after subscription update = %d, want 2", attempts)
	}
}

func TestSendNotificationTreatsDeletedExpiredSubscriptionAsSuccess(t *testing.T) {
	endpoint := "https://push.example.com/expired"
	st := &mockStore{subscriptions: []store.StoredSubscription{makeTestSubscription(endpoint)}}
	push := &mockPushClient{sendErr: func(webpush.Subscription) error {
		return webpush.ErrSubscriptionExpired
	}}
	worker := notification.NewWorker(notification.WorkerOptions{
		Store:      st,
		PushClient: push,
	})

	if err := worker.SendNotification(context.Background(), "title", "body", "tag"); err != nil {
		t.Fatalf("SendNotification returned error after removing expired subscription: %v", err)
	}
	if got := st.DeletedEndpoints(); len(got) != 1 || got[0] != endpoint {
		t.Fatalf("deleted endpoints = %v, want [%q]", got, endpoint)
	}
}

func TestStreamDisconnectDoesNotNotifyOnFirstDetection(t *testing.T) {
	st := &mockStore{subscriptions: []store.StoredSubscription{makeTestSubscription("https://push.example.com/sub1")}}
	push := &mockPushClient{}
	worker := notification.NewWorker(notification.WorkerOptions{Store: st, PushClient: push})
	snapshot := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {TargetCount: 1, StreamConnected: false},
		},
	}

	if err := worker.EvaluateSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("EvaluateSnapshot: %v", err)
	}
	if got := len(push.Sent()); got != 0 {
		t.Fatalf("pushes after first disconnected snapshot = %d, want 0", got)
	}

	if err := worker.EvaluateSnapshot(context.Background(), snapshot); err != nil {
		t.Fatalf("second EvaluateSnapshot while bootstrap is incomplete: %v", err)
	}
	if got := len(push.Sent()); got != 1 {
		t.Fatalf("pushes after second unbootstrapped disconnected snapshot = %d, want 1", got)
	}
}

func TestCleanWorkerExitOnContextCancellation(t *testing.T) {
	st := &mockStore{}
	push := &mockPushClient{}

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:              st,
		PushClient:         push,
		EvaluationInterval: 50 * time.Millisecond,
		SnapshotProvider: func(ctx context.Context) (notification.Snapshot, error) {
			return healthySnapshot(), nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- worker.Run(ctx)
	}()

	// Wait briefly for worker to start, then cancel context
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("worker returned unexpected error on context cancel: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("worker did not exit in a timely manner on context cancellation")
	}
}

func TestFailedListSubscriptionsDoesNotSuppressNextEvaluation(t *testing.T) {
	st := &mockStore{
		listErr: errors.New("database locked"),
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/sub1"),
		},
	}
	push := &mockPushClient{}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:          st,
		PushClient:     push,
		RemindInterval: 24 * time.Hour,
		Now:            func() time.Time { return now },
	})

	ctx := context.Background()
	degraded := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {Degraded: true},
		},
	}

	// 1st evaluation fails because ListSubscriptions returns error
	err := worker.EvaluateSnapshot(ctx, degraded)
	if err == nil {
		t.Fatal("expected error from EvaluateSnapshot when ListSubscriptions fails, got nil")
	}
	if len(push.Sent()) != 0 {
		t.Fatalf("expected 0 pushes sent, got %d", len(push.Sent()))
	}

	// Store recovers from error
	st.mu.Lock()
	st.listErr = nil
	st.mu.Unlock()

	// 2nd evaluation at t=1m (well before 24h remind interval)
	now = now.Add(1 * time.Minute)
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}

	// Push should be sent immediately on the retry, not suppressed for 24 hours
	if len(push.Sent()) != 1 {
		t.Fatalf("expected 1 push sent on retry after store recovered, got %d", len(push.Sent()))
	}
}

func TestZeroSubscribersDoesNotSuppressWhenSubscriberJoins(t *testing.T) {
	st := &mockStore{
		subscriptions: nil, // 0 subscribers
	}
	push := &mockPushClient{}
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:          st,
		PushClient:     push,
		RemindInterval: 24 * time.Hour,
		Now:            func() time.Time { return now },
	})

	ctx := context.Background()
	degraded := notification.Snapshot{
		DispatcherRunning: true,
		Providers: map[string]notification.ProviderStatus{
			"bluesky": {Degraded: true},
		},
	}

	// 1st evaluation: 0 subscribers
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 0 {
		t.Fatalf("expected 0 pushes sent, got %d", len(push.Sent()))
	}

	// User subscribes 5 minutes later (well before 24h remind interval)
	st.mu.Lock()
	st.subscriptions = []store.StoredSubscription{
		makeTestSubscription("https://push.example.com/new-user"),
	}
	st.mu.Unlock()

	// 2nd evaluation at t=5m
	now = now.Add(5 * time.Minute)
	if err := worker.EvaluateSnapshot(ctx, degraded); err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}

	// Push should be sent to the newly subscribed user immediately, not suppressed for 24 hours
	if len(push.Sent()) != 1 {
		t.Fatalf("expected 1 push sent to new subscriber, got %d", len(push.Sent()))
	}
	if push.Sent()[0].sub.Endpoint != "https://push.example.com/new-user" {
		t.Fatalf("endpoint = %q, want https://push.example.com/new-user", push.Sent()[0].sub.Endpoint)
	}
}

func TestSendNotification(t *testing.T) {
	st := &mockStore{
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/sub-1"),
			makeTestSubscription("https://push.example.com/sub-2"),
		},
	}
	push := &mockPushClient{}

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:      st,
		PushClient: push,
	})

	ctx := context.Background()
	err := worker.SendNotification(ctx, "Test Push Title", "Test Push Body", "custom-tag")
	if err != nil {
		t.Fatalf("SendNotification failed: %v", err)
	}

	sent := push.Sent()
	if len(sent) != 2 {
		t.Fatalf("expected 2 pushes sent, got %d", len(sent))
	}

	for _, s := range sent {
		var payload struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Tag   string `json:"tag"`
			Data  struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		if err := json.Unmarshal(s.payload, &payload); err != nil {
			t.Fatalf("failed to unmarshal payload: %v", err)
		}
		if payload.Title != "Test Push Title" {
			t.Errorf("title = %q, want %q", payload.Title, "Test Push Title")
		}
		if payload.Body != "Test Push Body" {
			t.Errorf("body = %q, want %q", payload.Body, "Test Push Body")
		}
		if payload.Tag != "custom-tag" {
			t.Errorf("tag = %q, want %q", payload.Tag, "custom-tag")
		}
		if payload.Data.URL != "/" {
			t.Errorf("data.url = %q, want %q", payload.Data.URL, "/")
		}
	}
}

func TestSendNotifications_AllDeliveriesFailed_RetriesNextTime(t *testing.T) {
	st := &mockStore{
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/sub-1"),
		},
	}
	push := &mockPushClient{
		sendErr: func(sub webpush.Subscription) error {
			return errors.New("500 internal server error")
		},
	}

	snap := notification.Snapshot{
		DispatcherRunning: false, // system:dispatcher_stopped
	}
	now := time.Now()

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:      st,
		PushClient: push,
		Now:        func() time.Time { return now },
		SnapshotProvider: func(ctx context.Context) (notification.Snapshot, error) {
			return snap, nil
		},
		RemindInterval: 24 * time.Hour,
	})

	ctx := context.Background()

	// First evaluation: delivery fails
	err := worker.EvaluateSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("EvaluateSnapshot failed: %v", err)
	}
	if len(push.Sent()) != 0 {
		t.Fatalf("expected 0 successful sends, got %d", len(push.Sent()))
	}

	// Server recovers: delivery now succeeds
	push.sendErr = nil
	now = now.Add(notification.DefaultEvaluationInterval)
	err = worker.EvaluateSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("EvaluateSnapshot 2nd failed: %v", err)
	}

	// Should have retried and sent immediately, NOT suppressed for 24h
	if len(push.Sent()) != 1 {
		t.Fatalf("expected 1 push sent on retry, got %d", len(push.Sent()))
	}
}

type concurrentTrackingPushClient struct {
	mu        sync.Mutex
	current   int32
	peak      int32
	delay     time.Duration
	sentCount int
}

func (c *concurrentTrackingPushClient) Send(ctx context.Context, sub webpush.Subscription, payload []byte, opts webpush.Options) error {
	c.mu.Lock()
	c.current++
	if c.current > c.peak {
		c.peak = c.current
	}
	c.sentCount++
	c.mu.Unlock()

	time.Sleep(c.delay)

	c.mu.Lock()
	c.current--
	c.mu.Unlock()
	return nil
}

func TestNotificationWorker_MaxConcurrentPushes(t *testing.T) {
	const numSubs = 12
	var subs []store.StoredSubscription
	for i := range numSubs {
		subs = append(subs, makeTestSubscription(fmt.Sprintf("https://push.example.com/sub-%d", i)))
	}
	st := &mockStore{subscriptions: subs}
	tracker := &concurrentTrackingPushClient{delay: 20 * time.Millisecond}

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:      st,
		PushClient: tracker,
	})

	ctx := context.Background()
	err := worker.SendNotification(ctx, "Alert", "Concurrent test", "test:tag")
	if err != nil {
		t.Fatalf("SendNotification failed: %v", err)
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if tracker.sentCount != numSubs {
		t.Errorf("sentCount = %d, want %d", tracker.sentCount, numSubs)
	}
	// maxConcurrentPushes is 5, peak must not exceed 5
	if tracker.peak > 5 {
		t.Errorf("peak concurrent pushes = %d, want <= 5", tracker.peak)
	}
	if tracker.peak < 2 {
		t.Errorf("peak concurrent pushes = %d, expected concurrent execution (>= 2)", tracker.peak)
	}
}

func TestSendNotification_NormalizesTag(t *testing.T) {
	st := &mockStore{
		subscriptions: []store.StoredSubscription{
			makeTestSubscription("https://push.example.com/sub-1"),
		},
	}
	push := &mockPushClient{}

	worker := notification.NewWorker(notification.WorkerOptions{
		Store:      st,
		PushClient: push,
	})

	ctx := context.Background()
	err := worker.SendNotification(ctx, "Title", "Body", "provider:test:extremely:long:tag:that:exceeds:thirty:two:bytes")
	if err != nil {
		t.Fatalf("SendNotification failed: %v", err)
	}

	sent := push.Sent()
	if len(sent) != 1 {
		t.Fatalf("expected 1 push sent, got %d", len(sent))
	}

	topic := sent[0].opts.Topic
	if len(topic) > 32 {
		t.Errorf("opts.Topic length = %d > 32: %q", len(topic), topic)
	}
	if strings.Contains(topic, ":") {
		t.Errorf("opts.Topic %q should not contain colons", topic)
	}
}
