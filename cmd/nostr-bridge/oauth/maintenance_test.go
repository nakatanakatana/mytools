package oauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type maintenanceClientFake struct {
	results       []RefreshResult
	refreshErrs   []error
	status        Status
	statusErr     error
	refreshCalls  int
	statusCalls   int
	accounts      []string
	periods       []time.Duration
	beforeRefresh func()
}

func (c *maintenanceClientFake) RefreshIfDue(_ context.Context, accountDID string, period time.Duration) (RefreshResult, error) {
	if c.beforeRefresh != nil {
		c.beforeRefresh()
	}
	index := c.refreshCalls
	c.refreshCalls++
	c.accounts = append(c.accounts, accountDID)
	c.periods = append(c.periods, period)
	var result RefreshResult
	if index < len(c.results) {
		result = c.results[index]
	}
	var err error
	if index < len(c.refreshErrs) {
		err = c.refreshErrs[index]
	}
	return result, err
}

func (c *maintenanceClientFake) AuthorizationStatus(_ context.Context, accountDID string, period time.Duration) (Status, error) {
	c.statusCalls++
	c.accounts = append(c.accounts, accountDID)
	c.periods = append(c.periods, period)
	return c.status, c.statusErr
}

type maintenanceObserverRecorder struct {
	cancel        context.CancelFunc
	cancelChecked int
	started       []time.Time
	stopped       []time.Time
	checked       []Status
	succeeded     []RefreshReason
	failedReasons []RefreshReason
	failedClasses []RefreshErrorClass
	retryReasons  []RefreshReason
	retryClasses  []RefreshErrorClass
	retryDelays   []time.Duration
	cancelRetries int
}

func (o *maintenanceObserverRecorder) Started(at time.Time) {
	o.started = append(o.started, at)
}

func (o *maintenanceObserverRecorder) Stopped(at time.Time) {
	o.stopped = append(o.stopped, at)
}

func (o *maintenanceObserverRecorder) Checked(_ time.Time, status Status) {
	o.checked = append(o.checked, status)
	if o.cancel != nil && o.cancelChecked > 0 && len(o.checked) == o.cancelChecked {
		o.cancel()
	}
}

func (o *maintenanceObserverRecorder) RefreshSucceeded(_ time.Time, reason RefreshReason) {
	o.succeeded = append(o.succeeded, reason)
}

func (o *maintenanceObserverRecorder) RefreshFailed(_ time.Time, reason RefreshReason, class RefreshErrorClass) {
	o.failedReasons = append(o.failedReasons, reason)
	o.failedClasses = append(o.failedClasses, class)
}

func (o *maintenanceObserverRecorder) RetryScheduled(_ time.Time, reason RefreshReason, class RefreshErrorClass, delay time.Duration) {
	o.retryReasons = append(o.retryReasons, reason)
	o.retryClasses = append(o.retryClasses, class)
	o.retryDelays = append(o.retryDelays, delay)
	if o.cancel != nil && o.cancelRetries > 0 && len(o.retryDelays) == o.cancelRetries {
		o.cancel()
	}
}

func immediateMaintenanceTimer(delays *[]time.Duration) func(time.Duration) <-chan time.Time {
	return func(delay time.Duration) <-chan time.Time {
		*delays = append(*delays, delay)
		ready := make(chan time.Time, 1)
		ready <- time.Unix(1, 0)
		return ready
	}
}

func TestMaintenanceWaitsForBoundedStartupJitterThenRefreshesOverdueAuthorization(t *testing.T) {
	now := time.Unix(100, 0)
	status := Status{AuthorizationAvailable: true, LastRefreshSucceededAt: now}
	client := &maintenanceClientFake{
		results: []RefreshResult{{Reason: RefreshReasonMaintenance, Refreshed: true}},
		status:  status,
	}
	var timerDelays []time.Duration
	var jitterBound time.Duration
	client.beforeRefresh = func() {
		if len(timerDelays) == 0 {
			t.Fatal("RefreshIfDue called before startup jitter timer")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	observer := &maintenanceObserverRecorder{cancel: cancel, cancelChecked: 1}
	maintenance := Maintenance{
		Client:        client,
		AccountDID:    "did:plc:owner",
		RefreshPeriod: 30 * 24 * time.Hour,
		CheckInterval: time.Hour,
		Now:           func() time.Time { return now },
		Jitter: func(bound time.Duration) time.Duration {
			jitterBound = bound
			return 7 * time.Second
		},
		Timer:    immediateMaintenanceTimer(&timerDelays),
		Observer: observer,
	}

	err := maintenance.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if jitterBound <= 0 || jitterBound > maintenance.CheckInterval {
		t.Fatalf("startup jitter bound = %s, want within (0, %s]", jitterBound, maintenance.CheckInterval)
	}
	if len(timerDelays) != 1 || timerDelays[0] != 7*time.Second {
		t.Fatalf("timer delays = %v, want [7s]", timerDelays)
	}
	if client.refreshCalls != 1 || client.statusCalls != 1 {
		t.Fatalf("client calls = refresh %d status %d, want 1/1", client.refreshCalls, client.statusCalls)
	}
	if len(client.accounts) != 2 || client.accounts[0] != maintenance.AccountDID || client.accounts[1] != maintenance.AccountDID {
		t.Fatalf("accounts = %v, want configured account", client.accounts)
	}
	if len(client.periods) != 2 || client.periods[0] != maintenance.RefreshPeriod || client.periods[1] != maintenance.RefreshPeriod {
		t.Fatalf("periods = %v, want %s", client.periods, maintenance.RefreshPeriod)
	}
	if len(observer.started) != 1 || !observer.started[0].Equal(now) || len(observer.stopped) != 1 || !observer.stopped[0].Equal(now) {
		t.Fatalf("lifecycle events = started %v stopped %v, want fixed now", observer.started, observer.stopped)
	}
	if len(observer.checked) != 1 || observer.checked[0] != status {
		t.Fatalf("checked status = %#v, want %#v", observer.checked, status)
	}
	if len(observer.succeeded) != 1 || observer.succeeded[0] != RefreshReasonMaintenance {
		t.Fatalf("success reasons = %v, want maintenance", observer.succeeded)
	}
}

func TestMaintenanceSkipsFreshAuthorization(t *testing.T) {
	client := &maintenanceClientFake{
		results: []RefreshResult{{Reason: RefreshReasonMaintenance}},
		status:  Status{AuthorizationAvailable: true},
	}
	var timerDelays []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	observer := &maintenanceObserverRecorder{cancel: cancel, cancelChecked: 1}
	maintenance := Maintenance{
		Client:        client,
		AccountDID:    "did:plc:owner",
		RefreshPeriod: time.Hour,
		CheckInterval: time.Minute,
		Jitter:        func(time.Duration) time.Duration { return 0 },
		Timer:         immediateMaintenanceTimer(&timerDelays),
		Observer:      observer,
	}

	err := maintenance.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if client.refreshCalls != 1 || len(observer.succeeded) != 0 || len(observer.failedClasses) != 0 || len(observer.retryDelays) != 0 {
		t.Fatalf(
			"fresh check = calls %d success %v failures %v retries %v",
			client.refreshCalls, observer.succeeded, observer.failedClasses, observer.retryDelays,
		)
	}
}

func TestMaintenanceRetriesTransientFailuresWithIncreasingBoundedDelays(t *testing.T) {
	const failures = 5
	client := &maintenanceClientFake{
		refreshErrs: []error{
			&RefreshError{Class: RefreshErrorServer},
			&RefreshError{Class: RefreshErrorServer},
			&RefreshError{Class: RefreshErrorServer},
			&RefreshError{Class: RefreshErrorServer},
			&RefreshError{Class: RefreshErrorServer},
		},
		status: Status{AuthorizationAvailable: true, LastRefreshErrorClass: RefreshErrorServer},
	}
	var timerDelays []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	observer := &maintenanceObserverRecorder{cancel: cancel, cancelRetries: failures}
	maintenance := Maintenance{
		Client:        client,
		AccountDID:    "did:plc:owner",
		RefreshPeriod: time.Hour,
		CheckInterval: 10 * time.Second,
		Jitter:        func(time.Duration) time.Duration { return 0 },
		Timer:         immediateMaintenanceTimer(&timerDelays),
		Observer:      observer,
	}

	err := maintenance.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	wantDelays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	if len(observer.retryDelays) != len(wantDelays) {
		t.Fatalf("retry delays = %v, want %v", observer.retryDelays, wantDelays)
	}
	for i, want := range wantDelays {
		if got := observer.retryDelays[i]; got != want {
			t.Fatalf("retry delay %d = %s, want %s", i, got, want)
		}
		if got := observer.retryDelays[i]; got >= maintenance.CheckInterval {
			t.Fatalf("retry delay %d = %s, want below %s", i, got, maintenance.CheckInterval)
		}
		if observer.retryReasons[i] != RefreshReasonMaintenance || observer.retryClasses[i] != RefreshErrorServer {
			t.Fatalf("retry event %d = %q/%q, want maintenance/server", i, observer.retryReasons[i], observer.retryClasses[i])
		}
		if observer.failedReasons[i] != RefreshReasonMaintenance || observer.failedClasses[i] != RefreshErrorServer {
			t.Fatalf("failure event %d = %q/%q, want maintenance/server", i, observer.failedReasons[i], observer.failedClasses[i])
		}
	}
	if client.refreshCalls != failures {
		t.Fatalf("refresh calls = %d, want %d", client.refreshCalls, failures)
	}
}

func TestMaintenanceReturnsBoundedOperationalErrorForUnknownRefreshFailure(t *testing.T) {
	const secret = "rotated-token-save-secret"
	client := &maintenanceClientFake{
		refreshErrs: []error{errors.New("save refreshed OAuth token: " + secret)},
		status:      Status{AuthorizationAvailable: true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	observer := &maintenanceObserverRecorder{}
	var timerDelays []time.Duration
	maintenance := Maintenance{
		Client:        client,
		AccountDID:    "did:plc:owner",
		RefreshPeriod: time.Hour,
		CheckInterval: 10 * time.Minute,
		Jitter:        func(time.Duration) time.Duration { return 0 },
		Timer: func(delay time.Duration) <-chan time.Time {
			timerDelays = append(timerDelays, delay)
			cancel()
			return make(chan time.Time)
		},
		Observer: observer,
	}

	err := maintenance.Run(ctx)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want bounded operational error", err)
	}
	if got := err.Error(); got != "OAuth maintenance refresh failed: class=protocol persistence_failed=false" {
		t.Fatalf("Run() error = %q, want bounded operational error", got)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Run() error leaked raw secret: %q", err)
	}
	if len(observer.retryDelays) != 0 || len(timerDelays) != 0 {
		t.Fatalf("scheduled retries = observer %v timer %v, want none", observer.retryDelays, timerDelays)
	}
	if len(observer.failedReasons) != 1 || observer.failedReasons[0] != RefreshReasonMaintenance ||
		len(observer.failedClasses) != 1 || observer.failedClasses[0] != RefreshErrorProtocol {
		t.Fatalf("failure events = %v/%v, want bounded maintenance/protocol", observer.failedReasons, observer.failedClasses)
	}
}

func TestMaintenanceReturnsBoundedOperationalErrorWhenFailurePersistenceFails(t *testing.T) {
	const secret = "persistence-secret-class"
	client := &maintenanceClientFake{
		refreshErrs: []error{&RefreshError{
			Class:             RefreshErrorClass(secret),
			ReauthRequired:    true,
			PersistenceFailed: true,
		}},
		status: Status{ReauthRequired: true, LastRefreshErrorClass: RefreshErrorClass(secret)},
	}
	ctx, cancel := context.WithCancel(context.Background())
	observer := &maintenanceObserverRecorder{}
	var timerDelays []time.Duration
	maintenance := Maintenance{
		Client:        client,
		AccountDID:    "did:plc:owner",
		RefreshPeriod: time.Hour,
		CheckInterval: 10 * time.Minute,
		Jitter:        func(time.Duration) time.Duration { return 0 },
		Timer: func(delay time.Duration) <-chan time.Time {
			timerDelays = append(timerDelays, delay)
			cancel()
			return make(chan time.Time)
		},
		Observer: observer,
	}

	err := maintenance.Run(ctx)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want bounded operational error", err)
	}
	if got := err.Error(); got != "OAuth maintenance refresh failed: class=protocol persistence_failed=true" {
		t.Fatalf("Run() error = %q, want bounded operational error", got)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("Run() error leaked raw secret: %q", err)
	}
	if len(observer.retryDelays) != 0 || len(timerDelays) != 0 {
		t.Fatalf("scheduled retries = observer %v timer %v, want none", observer.retryDelays, timerDelays)
	}
	if len(observer.failedReasons) != 1 || observer.failedReasons[0] != RefreshReasonMaintenance ||
		len(observer.failedClasses) != 1 || observer.failedClasses[0] != RefreshErrorProtocol {
		t.Fatalf("failure events = %v/%v, want bounded maintenance/protocol", observer.failedReasons, observer.failedClasses)
	}
}

func TestMaintenanceRunsPeriodicCheckAfterPermanentFailureWithoutRetry(t *testing.T) {
	client := &maintenanceClientFake{
		refreshErrs: []error{&RefreshError{Class: RefreshErrorInvalidGrant, ReauthRequired: true}},
		status: Status{
			ReauthRequired:        true,
			LastRefreshErrorClass: RefreshErrorInvalidGrant,
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	var timerDelays []time.Duration
	timer := func(delay time.Duration) <-chan time.Time {
		timerDelays = append(timerDelays, delay)
		ready := make(chan time.Time, 1)
		if len(timerDelays) <= 2 {
			ready <- time.Unix(1, 0)
		}
		return ready
	}
	observer := &maintenanceObserverRecorder{cancel: cancel, cancelChecked: 2}
	maintenance := Maintenance{
		Client:        client,
		AccountDID:    "did:plc:owner",
		RefreshPeriod: time.Hour,
		CheckInterval: 10 * time.Minute,
		Jitter:        func(time.Duration) time.Duration { return time.Second },
		Timer:         timer,
		Observer:      observer,
	}

	err := maintenance.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if client.refreshCalls != 2 || client.statusCalls != 2 {
		t.Fatalf("client calls = refresh %d status %d, want periodic second check", client.refreshCalls, client.statusCalls)
	}
	if len(observer.retryDelays) != 0 {
		t.Fatalf("retry delays = %v, want none", observer.retryDelays)
	}
	if len(timerDelays) != 2 || timerDelays[1] != maintenance.CheckInterval {
		t.Fatalf("timer delays = %v, want startup then periodic %s", timerDelays, maintenance.CheckInterval)
	}
	if len(observer.failedReasons) != 1 || observer.failedReasons[0] != RefreshReasonMaintenance ||
		len(observer.failedClasses) != 1 || observer.failedClasses[0] != RefreshErrorInvalidGrant {
		t.Fatalf("failure events = %v/%v, want maintenance/invalid_grant", observer.failedReasons, observer.failedClasses)
	}
}

func TestMaintenanceCancellationExitsWithoutWaitingForTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &maintenanceClientFake{}
	timerCalls := 0
	maintenance := Maintenance{
		Client:        client,
		AccountDID:    "did:plc:owner",
		RefreshPeriod: time.Hour,
		CheckInterval: time.Minute,
		Jitter:        func(time.Duration) time.Duration { return time.Second },
		Timer: func(time.Duration) <-chan time.Time {
			timerCalls++
			return make(chan time.Time)
		},
	}

	err := maintenance.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if client.refreshCalls != 0 || client.statusCalls != 0 {
		t.Fatalf("client calls after cancellation = refresh %d status %d", client.refreshCalls, client.statusCalls)
	}
	if timerCalls > 1 {
		t.Fatalf("timer calls = %d, want at most startup timer", timerCalls)
	}
}
