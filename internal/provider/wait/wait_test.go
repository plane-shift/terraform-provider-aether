package wait

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time                                 { return f.now }
func (f *fakeClock) Sleep(_ context.Context, d time.Duration) error { f.now = f.now.Add(d); return nil }

func opts(fc *fakeClock, timeout time.Duration) Options {
	return Options{Timeout: timeout, Success: []string{"running"}, Warning: []string{"degraded"}, Failed: []string{"failed"}, Now: fc.Now, Sleep: fc.Sleep}
}

func TestForStatus_ReachesSuccess(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	seq := []string{"provisioning", "provisioning", "running"}
	i := 0
	res, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { s := seq[i]; i++; return s, "", nil }, opts(fc, time.Hour))
	if err != nil || res.Status != "running" || res.Warned {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestForStatus_WarningIsTerminalAndFlagged(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	res, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "degraded", "one node NotReady", nil }, opts(fc, time.Hour))
	if err != nil || !res.Warned || res.Message != "one node NotReady" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestForStatus_FailedCarriesMessage(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	_, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "failed", "quota exceeded", nil }, opts(fc, time.Hour))
	var fe *FailedError
	if !errors.As(err, &fe) || fe.Message != "quota exceeded" {
		t.Fatalf("err = %v", err)
	}
}

func TestForStatus_TimeoutNamesLastStatus(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	_, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "provisioning", "", nil }, opts(fc, 2*time.Minute))
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), "provisioning") {
		t.Fatalf("err = %v", err)
	}
}

func TestForStatus_BackoffCapsAt15s(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	var sleeps []time.Duration
	o := opts(fc, time.Hour)
	o.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		fc.now = fc.now.Add(d)
		return nil
	}
	n := 0
	_, _ = ForStatus(context.Background(), func(context.Context) (string, string, error) {
		n++
		if n > 8 {
			return "running", "", nil
		}
		return "provisioning", "", nil
	}, o)
	if sleeps[0] != time.Second || sleeps[len(sleeps)-1] != 15*time.Second {
		t.Fatalf("sleeps = %v", sleeps)
	}
}

func TestForStatus_ProbeErrorIsReturned(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	boom := errors.New("boom")
	_, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "", "", boom }, opts(fc, time.Hour))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

// TestForStatus_CtxCancelledDuringSleepReturnsTheCtxError exercises the
// production Sleep contract (a caller-injected Sleep can itself cancel ctx,
// e.g. from an unrelated shutdown signal) rather than the fake clock's Sleep,
// which never returns an error.
func TestForStatus_CtxCancelledDuringSleepReturnsTheCtxError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fc := &fakeClock{now: time.Unix(0, 0)}
	o := opts(fc, time.Hour)
	o.Sleep = func(sctx context.Context, _ time.Duration) error {
		cancel()
		return sctx.Err()
	}
	_, err := ForStatus(ctx, func(context.Context) (string, string, error) { return "provisioning", "", nil }, o)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestForGone_TimeoutWrapsErrTimeout(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	err := ForGone(context.Background(), func(context.Context) (bool, error) { return false, nil }, opts(fc, 2*time.Minute))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout wrapped", err)
	}
}

func TestForGone_ProbeErrorIsWrapped(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	boom := errors.New("boom")
	err := ForGone(context.Background(), func(context.Context) (bool, error) { return false, boom }, opts(fc, time.Hour))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v wrapped", err, boom)
	}
}

// TestForStatus_BackoffNeverOvershootsTheDeadline pins a review-confirmed
// defect: backoff(attempt) is added to the clock unconditionally, with no
// check against how much time is actually left before the deadline. With
// Timeout=10s the real sequence is 1s,2s,4s,8s (sum 15s) BEFORE the loop even
// re-checks the deadline - so the timeout fires at clock=15s, 50% past the
// 10s a caller asked for. The last sleep must be clamped to the remaining
// time instead.
func TestForStatus_BackoffNeverOvershootsTheDeadline(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	deadline := fc.now.Add(10 * time.Second)
	o := opts(fc, 10*time.Second)
	_, err := ForStatus(context.Background(), func(context.Context) (string, string, error) { return "provisioning", "", nil }, o)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !strings.Contains(err.Error(), "provisioning") {
		t.Fatalf("err = %v, want the last status named", err)
	}
	if fc.now.After(deadline) {
		t.Fatalf("clock ended at %s, %s past the %s deadline - the last sleep must be clamped to the remaining time, never overshoot it",
			fc.now, fc.now.Sub(deadline), o.Timeout)
	}
}

// TestForGone_BackoffNeverOvershootsTheDeadline is ForStatus's sibling: same
// unclamped backoff.(attempt) call, same overshoot.
func TestForGone_BackoffNeverOvershootsTheDeadline(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	deadline := fc.now.Add(10 * time.Second)
	o := opts(fc, 10*time.Second)
	err := ForGone(context.Background(), func(context.Context) (bool, error) { return false, nil }, o)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if fc.now.After(deadline) {
		t.Fatalf("clock ended at %s, %s past the %s deadline - the last sleep must be clamped to the remaining time, never overshoot it",
			fc.now, fc.now.Sub(deadline), o.Timeout)
	}
}

func TestForGone(t *testing.T) {
	fc := &fakeClock{now: time.Unix(0, 0)}
	n := 0
	err := ForGone(context.Background(), func(context.Context) (bool, error) { n++; return n >= 3, nil }, opts(fc, time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}
