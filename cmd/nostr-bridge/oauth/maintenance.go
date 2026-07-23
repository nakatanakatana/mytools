package oauth

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

const maxMaintenanceStartupJitter = time.Minute

// MaintenanceClient is the OAuth client surface needed by Maintenance.
type MaintenanceClient interface {
	RefreshIfDue(context.Context, string, time.Duration) (RefreshResult, error)
	AuthorizationStatus(context.Context, string, time.Duration) (Status, error)
}

// MaintenanceObserver receives bounded, secret-free maintenance events.
type MaintenanceObserver interface {
	Started(time.Time)
	Stopped(time.Time)
	Checked(time.Time, Status)
	RefreshSucceeded(time.Time, RefreshReason)
	RefreshFailed(time.Time, RefreshReason, RefreshErrorClass)
	RetryScheduled(time.Time, RefreshReason, RefreshErrorClass, time.Duration)
}

// NopMaintenanceObserver provides no-op default observer methods.
type NopMaintenanceObserver struct{}

func (NopMaintenanceObserver) Started(time.Time) {}

func (NopMaintenanceObserver) Stopped(time.Time) {}

func (NopMaintenanceObserver) Checked(time.Time, Status) {}

func (NopMaintenanceObserver) RefreshSucceeded(time.Time, RefreshReason) {}

func (NopMaintenanceObserver) RefreshFailed(time.Time, RefreshReason, RefreshErrorClass) {}

func (NopMaintenanceObserver) RetryScheduled(time.Time, RefreshReason, RefreshErrorClass, time.Duration) {
}

// Maintenance periodically refreshes the configured Bluesky authorization.
type Maintenance struct {
	Client        MaintenanceClient
	AccountDID    string
	RefreshPeriod time.Duration
	CheckInterval time.Duration
	Now           func() time.Time
	Jitter        func(time.Duration) time.Duration
	Timer         func(time.Duration) <-chan time.Time
	Observer      MaintenanceObserver
}

// Run checks the durable authorization after bounded startup jitter and then
// on the configured interval. Transient refresh failures use bounded
// exponential retries; permanent failures wait for the next periodic check.
// Unexpected failures and refresh-failure persistence errors stop the worker.
func (m Maintenance) Run(ctx context.Context) error {
	if m.Client == nil {
		return errors.New("OAuth maintenance client is required")
	}
	if strings.TrimSpace(m.AccountDID) == "" {
		return errors.New("OAuth maintenance account DID is required")
	}
	if m.RefreshPeriod <= 0 {
		return errors.New("OAuth maintenance refresh period must be positive")
	}
	if m.CheckInterval <= 0 {
		return errors.New("OAuth maintenance check interval must be positive")
	}

	observer := m.Observer
	if observer == nil {
		observer = NopMaintenanceObserver{}
	}
	now := m.Now
	if now == nil {
		now = time.Now
	}
	timer := m.Timer
	if timer == nil {
		timer = time.After
	}
	jitter := m.Jitter
	if jitter == nil {
		jitter = func(bound time.Duration) time.Duration {
			return time.Duration(rand.Int64N(int64(bound) + 1))
		}
	}

	observer.Started(now())
	defer func() {
		observer.Stopped(now())
	}()

	jitterBound := min(m.CheckInterval, maxMaintenanceStartupJitter)
	startupDelay := jitter(jitterBound)
	if startupDelay < 0 {
		startupDelay = 0
	}
	if startupDelay > jitterBound {
		startupDelay = jitterBound
	}
	if err := waitForMaintenance(ctx, timer, startupDelay); err != nil {
		return err
	}

	retryAttempt := 0
	for {
		result, refreshErr := m.Client.RefreshIfDue(ctx, m.AccountDID, m.RefreshPeriod)
		status, statusErr := m.Client.AuthorizationStatus(ctx, m.AccountDID, m.RefreshPeriod)
		if statusErr != nil {
			return fmt.Errorf("inspect OAuth authorization after maintenance check: %w", statusErr)
		}
		if status.LastRefreshErrorClass != "" {
			status.LastRefreshErrorClass = boundedRefreshErrorClass(status.LastRefreshErrorClass)
		}
		observer.Checked(now(), status)

		nextDelay := m.CheckInterval
		if refreshErr != nil {
			class, retryable, operationalErr := maintenanceFailure(refreshErr)
			observer.RefreshFailed(now(), RefreshReasonMaintenance, class)
			if operationalErr != nil {
				return operationalErr
			}
			if retryable {
				nextDelay = maintenanceRetryDelay(retryAttempt, m.CheckInterval)
				retryAttempt++
				observer.RetryScheduled(now(), RefreshReasonMaintenance, class, nextDelay)
			} else {
				retryAttempt = 0
			}
		} else {
			retryAttempt = 0
			if result.Refreshed {
				observer.RefreshSucceeded(now(), RefreshReasonMaintenance)
			}
		}

		if err := waitForMaintenance(ctx, timer, nextDelay); err != nil {
			return err
		}
	}
}

func maintenanceFailure(err error) (RefreshErrorClass, bool, error) {
	var refreshErr *RefreshError
	if !errors.As(err, &refreshErr) || refreshErr == nil {
		class := RefreshErrorProtocol
		return class, false, maintenanceOperationalError(class, false)
	}
	class := boundedRefreshErrorClass(refreshErr.Class)
	if refreshErr.PersistenceFailed {
		return class, false, maintenanceOperationalError(class, true)
	}
	return class, !refreshErr.ReauthRequired, nil
}

func maintenanceOperationalError(class RefreshErrorClass, persistenceFailed bool) error {
	return fmt.Errorf(
		"OAuth maintenance refresh failed: class=%s persistence_failed=%t",
		boundedRefreshErrorClass(class),
		persistenceFailed,
	)
}

func maintenanceRetryDelay(attempt int, checkInterval time.Duration) time.Duration {
	maxDelay := checkInterval / 2
	if maxDelay <= 0 {
		return 0
	}
	delay := min(time.Second, maxDelay)
	for range attempt {
		if delay >= maxDelay {
			return maxDelay
		}
		if delay > maxDelay/2 {
			return maxDelay
		}
		delay *= 2
	}
	return min(delay, maxDelay)
}

func waitForMaintenance(
	ctx context.Context,
	timer func(time.Duration) <-chan time.Time,
	delay time.Duration,
) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if delay <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer(delay):
		return nil
	}
}
