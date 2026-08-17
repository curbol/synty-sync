// Package retry runs an operation with bounded exponential backoff, honoring
// context cancellation and stopping early on an error marked non-retryable. It is
// the shared mechanism behind the portal's page fetches and the syncer's file
// downloads; each caller keeps its own policy for which errors are retryable.
package retry

import (
	"context"
	"errors"
	"time"
)

// Do calls fn up to attempts times. Between tries it waits base<<(i-1), returning
// ctx.Err() if the context is cancelled during a wait. It returns nil on the first
// success, the unwrapped error immediately when fn returns one wrapped with Stop,
// and otherwise the last error once all attempts are exhausted.
func Do(ctx context.Context, attempts int, base time.Duration, fn func() error) error {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(base << (i - 1)):
			}
		}
		err := fn()
		if err == nil {
			return nil
		}
		var s *stop
		if errors.As(err, &s) {
			return s.err
		}
		lastErr = err
	}
	return lastErr
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
