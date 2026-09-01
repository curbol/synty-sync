// Package retry runs an operation with bounded exponential backoff, honoring
// context cancellation and stopping early on an error marked non-retryable. It is
// the shared mechanism behind the portal's page fetches and the syncer's file
// downloads; each caller keeps its own policy for which errors are retryable.
package retry

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// MaxDelay caps the wait an After-marked error can ask for, so a server that answers
// a rate limit with an implausible Retry-After cannot park a run for the rest of the
// day. Past it the caller gives up and reports, which a person can act on.
const MaxDelay = 60 * time.Second

// Do calls fn up to attempts times. Between tries it waits base<<(i-1) plus jitter,
// or the delay an After-marked error asked for, returning ctx.Err() if the context is
// cancelled during a wait. It returns nil on the first success, the unwrapped error
// immediately when fn returns one wrapped with Stop, and otherwise the last error
// once all attempts are exhausted.
func Do(ctx context.Context, attempts int, base time.Duration, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	var requested time.Duration
	for i := 0; i < attempts; i++ {
		if i > 0 {
			wait := requested
			if wait <= 0 {
				wait = backoff(base, i)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		requested = 0
		err := fn()
		if err == nil {
			return nil
		}
		var s *stop
		if errors.As(err, &s) {
			return s.err
		}
		var d *delayed
		if errors.As(err, &d) {
			requested = min(d.d, MaxDelay)
		}
		lastErr = err
	}
	return lastErr
}

// backoff doubles with each attempt and adds up to as much again at random. Callers
// fan out — the syncer reads several item pages at once — so a shared rate limit trips
// them together, and without the jitter they would come back in lockstep and trip it
// again.
func backoff(base time.Duration, i int) time.Duration {
	d := base << (i - 1)
	return d + time.Duration(rand.Int64N(int64(d)+1))
}

// Stop marks err as non-retryable so Do returns it without further attempts. The
// result unwraps to err, so errors.Is/As see through the marker.
func Stop(err error) error {
	if err == nil {
		return nil
	}
	return &stop{err}
}

type stop struct{ err error }

func (s *stop) Error() string { return s.err.Error() }
func (s *stop) Unwrap() error { return s.err }

// After marks err retryable after the delay the server asked for, so a rate limit
// clears by waiting the stated time rather than by exhausting a budget far shorter
// than it. The result unwraps to err, so errors.Is/As see through the marker.
func After(err error, d time.Duration) error {
	if err == nil {
		return nil
	}
	return &delayed{err, d}
}

type delayed struct {
	err error
	d   time.Duration
}

func (d *delayed) Error() string { return d.err.Error() }
func (d *delayed) Unwrap() error { return d.err }
