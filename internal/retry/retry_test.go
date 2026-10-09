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

// MaxDelay is what keeps a server's Retry-After from parking a run. A store that
// answers a rate limit with an implausible wait (a stray "86400", a date a year out)
// would otherwise leave sync sitting in time.After with no output for the rest of
// the day. Checked through nextDelay rather than Do, so the cap can be asserted
// without waiting one out.
func TestNextDelay(t *testing.T) {
	const base = 100 * time.Millisecond
	for _, tc := range []struct {
		name      string
		requested time.Duration
		want      func(time.Duration) bool
		describe  string
	}{
		{
			name:      "an implausible Retry-After is capped",
			requested: 24 * time.Hour,
			want:      func(d time.Duration) bool { return d == MaxDelay },
			describe:  "MaxDelay",
		},
		{
			name:      "a reasonable Retry-After is honored exactly",
			requested: 3 * time.Second,
			want:      func(d time.Duration) bool { return d == 3*time.Second },
			describe:  "the requested 3s",
		},
		{
			// A past-dated or zero Retry-After must not mean "retry instantly": that
			// spends the whole attempt budget inside the window the server asked for.
			name:      "no request falls back to backoff",
			requested: 0,
			want:      func(d time.Duration) bool { return d >= base && d <= 2*base },
			describe:  "one backoff interval",
		},
		{
			name:      "a negative request falls back to backoff",
			requested: -5 * time.Second,
			want:      func(d time.Duration) bool { return d >= base && d <= 2*base },
			describe:  "one backoff interval",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Repeated because backoff adds jitter, so one sample proves little.
			for i := 0; i < 50; i++ {
				if got := nextDelay(tc.requested, base, 1); !tc.want(got) {
					t.Fatalf("nextDelay(%v) = %v, want %s", tc.requested, got, tc.describe)
				}
			}
		})
	}
}

// After marks an error retryable at the server's pace. Do must see through the
// marker the way it does through Stop, or a rate limit that a caller classifies by
// status code stops being classifiable the moment it carries a Retry-After.
func TestAfterKeepsTheErrorMatchable(t *testing.T) {
	sentinel := errors.New("rate limited")
	wrapped := After(sentinel, time.Second)
	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is cannot see through the After marker")
	}
	if wrapped.Error() != sentinel.Error() {
		t.Errorf("After changed the message to %q", wrapped.Error())
	}
	if After(nil, time.Second) != nil {
		t.Error("After(nil) must stay nil")
	}

	// And the error Do returns when the attempts run out still matches.
	err := Do(context.Background(), 2, time.Millisecond, func() error {
		return After(sentinel, time.Millisecond)
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Do returned %v; the sentinel did not survive the retry loop", err)
	}
}

// Do floors attempts at one. Without the floor a zero or negative count runs the loop
// no times and returns its nil lastErr: success reported for an operation never tried,
// so a caller that left its attempt count unset would record a download that never ran.
func TestDoRunsOnceForANonPositiveAttemptCount(t *testing.T) {
	for _, attempts := range []int{0, -1} {
		calls := 0
		sentinel := errors.New("boom")
		err := Do(context.Background(), attempts, time.Millisecond, func() error {
			calls++
			return sentinel
		})
		if calls != 1 {
			t.Errorf("attempts=%d: calls = %d, want 1", attempts, calls)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("attempts=%d: err = %v, want the one attempt's error", attempts, err)
		}
	}
}
