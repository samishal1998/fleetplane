package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrorClass tells the operation engine how to schedule around a failure
// (docs/03 §7).
type ErrorClass string

const (
	ErrRetryable   ErrorClass = "retryable"
	ErrRateLimited ErrorClass = "rate_limited"
	ErrConflict    ErrorClass = "conflict"
	ErrNotFound    ErrorClass = "not_found"
	ErrInvalid     ErrorClass = "invalid"
	ErrQuota       ErrorClass = "quota"
	ErrTerminal    ErrorClass = "terminal"
)

// SideEffect tells the engine whether re-executing a failed mutation is safe
// (invariant 7 as a type, plan R7): EffectNone means the request provably
// never reached the provider (blind retry is safe); EffectMaybe means the
// outcome is uncertain and the resolution procedure must run first.
type SideEffect string

const (
	EffectNone  SideEffect = "none"
	EffectMaybe SideEffect = "maybe"
)

// Error is the typed provider error (docs/03 §7). Message must be safe to
// log; never embed credentials or request bodies.
type Error struct {
	Class      ErrorClass
	SideEffect SideEffect // meaningful on mutation paths; EffectMaybe is the safe default
	Code       string     // provider-native error code
	Message    string
	Provider   string        // instance name
	RequestID  string        // provider correlation ID
	RetryAfter time.Duration // 0 = unknown
	Cause      error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s: %s", e.Class, e.Message)
	if e.Provider != "" {
		s = e.Provider + ": " + s
	}
	if e.Code != "" {
		s += " (code=" + e.Code
		if e.RequestID != "" {
			s += ", request_id=" + e.RequestID
		}
		s += ")"
	} else if e.RequestID != "" {
		s += " (request_id=" + e.RequestID + ")"
	}
	return s
}

func (e *Error) Unwrap() error { return e.Cause }

// Classify maps any error to an ErrorClass. Unknown errors are ErrRetryable
// — never silently terminal (conservative: give the engine's capped backoff
// a chance rather than freezing a resource on a transient blip).
func Classify(err error) ErrorClass {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Class
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ErrRetryable
	}
	return ErrRetryable
}

// IsClass reports whether err carries the given class.
func IsClass(err error, c ErrorClass) bool { return Classify(err) == c }

// Effect reports whether a failed mutation may have had a side effect.
// Anything that is not a *Error explicitly marked EffectNone is EffectMaybe
// (conservative default: transport errors after send are uncertain).
func Effect(err error) SideEffect {
	var pe *Error
	if errors.As(err, &pe) && pe.SideEffect == EffectNone {
		return EffectNone
	}
	return EffectMaybe
}

// RetryAfterOf extracts an explicit retry-after hint, if any.
func RetryAfterOf(err error) (time.Duration, bool) {
	var pe *Error
	if errors.As(err, &pe) && pe.RetryAfter > 0 {
		return pe.RetryAfter, true
	}
	return 0, false
}
