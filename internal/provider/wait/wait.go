// Package wait polls a resource until it reaches a terminal state.
//
// Two rules shape the loop and both are load-bearing:
//
//   - The status is matched on EVERY probe including the first. A
//     sleep-then-check loop makes the fastest possible operation take a full
//     backoff interval, and hides the case where the resource was already
//     terminal when the wait began.
//   - A Warning status is TERMINAL, not a state to keep polling through. A
//     cluster that comes up degraded has finished coming up; the caller is told
//     via Result.Warned so it can raise a warning diagnostic instead of hanging
//     until the timeout and then reporting a failure that did not happen.
package wait

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrTimeout is wrapped by every timeout error, so callers can use errors.Is
// while still reading the last-seen status out of the message.
var ErrTimeout = errors.New("wait: timeout")

// FailedError is returned when a probe reports a status in Options.Failed. It is
// always returned as a POINTER, so the errors.As target is a **FailedError:
//
//	var fe *wait.FailedError
//	if errors.As(err, &fe) { ... fe.Status, fe.Message ... }
type FailedError struct {
	Status  string
	Message string
}

func (e *FailedError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("reached %s", e.Status)
	}
	return fmt.Sprintf("reached %s: %s", e.Status, e.Message)
}

// Probe reads the current status of a resource. The message is whatever the API
// offers as a human explanation (a failure reason, a degraded-condition note).
type Probe func(ctx context.Context) (status, message string, err error)

// Options configures a wait. Now and Sleep exist so tests can drive the loop
// with a fake clock; production leaves them nil.
type Options struct {
	Timeout                  time.Duration
	Success, Warning, Failed []string
	Now                      func() time.Time
	Sleep                    func(context.Context, time.Duration) error
}

// Result is the terminal state a wait ended in.
type Result struct {
	Status  string
	Message string
	// Warned is true when Status matched Options.Warning: the operation
	// finished, but not cleanly.
	Warned bool
}

func (o *Options) defaults() {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
}

// sleep is the production Sleep. It honours ctx cancellation: a practitioner
// pressing Ctrl-C must not wait out the remaining backoff.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func in(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// backoff is 1s, 2s, 4s, 8s, then 15s forever. The cap is applied AFTER the
// doubling, so the sequence never jumps from 8s to 16s.
func backoff(attempt int) time.Duration {
	if attempt >= 4 { // 1<<4 already exceeds the cap; also avoids shifting far
		return 15 * time.Second
	}
	d := time.Second << attempt
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	return d
}

// nextSleep is backoff clamped to the time left before the deadline. Sleeping
// PAST the deadline turns a 30s timeout into a 45s one and, worse, reports the
// status seen 15s before the deadline as "the last status" - the caller waits
// longer than it asked and is then told about a staler observation than the
// clock allowed for. The unclamped sequence is preserved whenever there is
// room for it, which is every ordinary case.
func nextSleep(attempt int, now, deadline time.Time) time.Duration {
	d := backoff(attempt)
	if remaining := deadline.Sub(now); remaining < d {
		return remaining
	}
	return d
}

// ForStatus polls until the status is in Success, Warning or Failed, the
// timeout expires, or the probe fails.
func ForStatus(ctx context.Context, probe Probe, opts Options) (Result, error) {
	opts.defaults()
	deadline := opts.Now().Add(opts.Timeout)
	last := ""
	for attempt := 0; ; attempt++ {
		status, msg, err := probe(ctx)
		if err != nil {
			// Short-circuit: a probe that cannot read the resource is not a
			// "not ready yet" signal. Wrapped, so errors.Is/As still reach the
			// underlying cause.
			return Result{}, fmt.Errorf("wait: probe failed: %w", err)
		}
		last = status
		switch {
		case in(opts.Success, status):
			return Result{Status: status, Message: msg}, nil
		case in(opts.Warning, status):
			return Result{Status: status, Message: msg, Warned: true}, nil
		case in(opts.Failed, status):
			return Result{}, &FailedError{Status: status, Message: msg}
		}
		if !opts.Now().Before(deadline) {
			return Result{}, fmt.Errorf("%w after %s; last status %q", ErrTimeout, opts.Timeout, last)
		}
		if err := opts.Sleep(ctx, nextSleep(attempt, opts.Now(), deadline)); err != nil {
			return Result{}, err
		}
	}
}

// ForGone polls until the resource is gone (a Read that 404s), used by Delete.
func ForGone(ctx context.Context, probe func(ctx context.Context) (bool, error), opts Options) error {
	opts.defaults()
	deadline := opts.Now().Add(opts.Timeout)
	for attempt := 0; ; attempt++ {
		gone, err := probe(ctx)
		if err != nil {
			return fmt.Errorf("wait: probe failed: %w", err)
		}
		if gone {
			return nil
		}
		if !opts.Now().Before(deadline) {
			return fmt.Errorf("%w after %s; resource still present", ErrTimeout, opts.Timeout)
		}
		if err := opts.Sleep(ctx, nextSleep(attempt, opts.Now(), deadline)); err != nil {
			return err
		}
	}
}
