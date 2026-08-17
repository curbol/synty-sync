package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDoSucceedsWithoutRetry(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestDoRetriesThenSucceeds(t *testing.T) {
	calls := 0
	err := Do(context.Background(), 3, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestDoExhaustsAndReturnsLastError(t *testing.T) {
	calls := 0
	sentinel := errors.New("boom")
	err := Do(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the last error", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (all attempts used)", calls)
	}
}

func TestDoStopsEarlyOnNonRetryable(t *testing.T) {
	calls := 0
	permanent := errors.New("404")
	err := Do(context.Background(), 5, time.Millisecond, func() error {
		calls++
		return Stop(permanent)
	})
	if !errors.Is(err, permanent) {
		t.Errorf("err = %v, want the wrapped permanent error (unwrapped)", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (Stop must not retry)", calls)
	}
}

func TestDoHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Do(ctx, 5, time.Hour, func() error {
		calls++
		cancel() // cancel before the first backoff wait
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancellation aborts the backoff)", calls)
	}
}
