package domain

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrorType mirrors the OpenAI error "type" field.
type ErrorType string

const (
	ErrTypeInvalidRequest ErrorType = "invalid_request_error"
	ErrTypeAuthentication ErrorType = "authentication_error"
	ErrTypePermission     ErrorType = "permission_error"
	ErrTypeNotFound       ErrorType = "not_found_error"
	ErrTypeRateLimit      ErrorType = "rate_limit_error"
	ErrTypeUnsupported    ErrorType = "unsupported_error"
	ErrTypeAPI            ErrorType = "api_error"
)

// APIError is an error that can be rendered as an OpenAI-compatible error envelope.
type APIError struct {
	Status  int
	Type    ErrorType
	Code    string
	Message string
	Param   string
	Err     error
}

func (e *APIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("%s (%s): %s", e.Type, e.Code, msg)
}

func (e *APIError) Unwrap() error { return e.Err }

// WithParam attaches the offending parameter name.
func (e *APIError) WithParam(p string) *APIError { e.Param = p; return e }

// WithCause attaches an underlying error.
func (e *APIError) WithCause(err error) *APIError { e.Err = err; return e }

func newErr(status int, t ErrorType, code, msg string) *APIError {
	return &APIError{Status: status, Type: t, Code: code, Message: msg}
}

// Constructors for the error shapes the gateway returns.
func ErrInvalidRequest(msg string) *APIError {
	return newErr(http.StatusBadRequest, ErrTypeInvalidRequest, "invalid_request", msg)
}
func ErrUnauthorized(msg string) *APIError {
	return newErr(http.StatusUnauthorized, ErrTypeAuthentication, "invalid_api_key", msg)
}
func ErrForbidden(msg string) *APIError {
	return newErr(http.StatusForbidden, ErrTypePermission, "permission_denied", msg)
}

// ErrConflict reports a uniqueness or reference-integrity violation: the request
// is well formed but contradicts the current configuration.
func ErrConflict(msg string) *APIError {
	return newErr(http.StatusConflict, ErrTypeInvalidRequest, "conflict", msg)
}
func ErrNotFound(msg string) *APIError {
	return newErr(http.StatusNotFound, ErrTypeNotFound, "not_found", msg)
}
func ErrModelNotFound(model string) *APIError {
	return newErr(http.StatusNotFound, ErrTypeNotFound, "model_not_found", "model not found: "+model)
}
func ErrRateLimited(msg string) *APIError {
	return newErr(http.StatusTooManyRequests, ErrTypeRateLimit, "rate_limit_exceeded", msg)
}

// ErrProviderBusy reports that every candidate was at its provider concurrency limit
// (providers.max_inflight) when this attempt needed a slot. It is a rate-limit shape so
// clients back off, with a code of its own: an operator (and a client) must be able to
// tell gateway capacity from the account's own rpm/tpm/concurrency quota, which is the
// only thing rate_limit_exceeded ever meant.
func ErrProviderBusy(msg string) *APIError {
	return newErr(http.StatusTooManyRequests, ErrTypeRateLimit, "provider_busy", msg)
}
func ErrInsufficientQuota(msg string) *APIError {
	return newErr(http.StatusPaymentRequired, ErrTypeRateLimit, "billing_hard_limit_reached", msg)
}
func ErrUnsupported(msg string) *APIError {
	return newErr(http.StatusBadRequest, ErrTypeUnsupported, "unsupported_parameter", msg)
}
func ErrInternal(msg string) *APIError {
	return newErr(http.StatusInternalServerError, ErrTypeAPI, "internal_error", msg)
}

func ErrGatewayTimeout(msg string) *APIError {
	return newErr(http.StatusGatewayTimeout, ErrTypeAPI, "upstream_timeout", msg)
}

func ErrUpstream(status int, msg string) *APIError {
	if status == 0 {
		status = http.StatusBadGateway
	}
	return newErr(status, ErrTypeAPI, "upstream_error", msg)
}

// HasStatus reports whether err carries the given HTTP status.
func HasStatus(err error, status int) bool {
	ae, ok := AsAPIError(err)
	return ok && ae.Status == status
}

// IsNotFound reports whether err is a not-found error.
func IsNotFound(err error) bool { return HasStatus(err, http.StatusNotFound) }

// IsUnauthorized reports whether err is an authentication error.
func IsUnauthorized(err error) bool { return HasStatus(err, http.StatusUnauthorized) }

// IsForbidden reports whether err is a permission error.
func IsForbidden(err error) bool { return HasStatus(err, http.StatusForbidden) }

// IsConflict reports whether err is a conflict (409): the request was understood but the
// state already stored forbids it — a name taken within its scope, or a delete that would
// remove more than it says.
func IsConflict(err error) bool { return HasStatus(err, http.StatusConflict) }

// IsInvalidRequest reports whether err is a refused request (400): the caller asked for
// something the gateway will not do, as opposed to something it cannot find.
func IsInvalidRequest(err error) bool { return HasStatus(err, http.StatusBadRequest) }

// IsRateLimited reports whether err indicates rate limiting.
func IsRateLimited(err error) bool { return HasStatus(err, http.StatusTooManyRequests) }

// IsInsufficientQuota reports whether err indicates exhausted balance or credit.
func IsInsufficientQuota(err error) bool { return HasStatus(err, http.StatusPaymentRequired) }

// AsAPIError extracts an *APIError from err (if any).
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}
