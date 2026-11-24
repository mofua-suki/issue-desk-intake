package ai

import "errors"

// IdleTimeoutError preserves the original failure while identifying an idle diagnostic.
type IdleTimeoutError struct{ Err error }

func (e *IdleTimeoutError) Error() string { return e.Err.Error() }
func (e *IdleTimeoutError) Unwrap() error { return e.Err }

func IsIdleTimeout(err error) bool {
	var idle *IdleTimeoutError
	return errors.As(err, &idle)
}
