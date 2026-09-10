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
func ErrNotFound(msg string) *APIError {
	return newErr(http.StatusNotFound, ErrTypeNotFound, "not_found", msg)
}
func ErrModelNotFound(model string) *APIError {
	return newErr(http.StatusNotFound, ErrTypeNotFound, "model_not_found", "model not found: "+model)
}
func ErrRateLimited(msg string) *APIError {
	return newErr(http.StatusTooManyRequests, ErrTypeRateLimit, "rate_limit_exceeded", msg)
}
func ErrInsufficientQuota(msg string) *APIError {
	return newErr(http.StatusPaymentRequired, ErrTypeRateLimit, "billing_hard_limit_reached", msg)
}
func ErrUnsupported(msg string) *APIError {
	return newErr(http.StatusBadRequest, ErrTypeUnsupported, "unsupported_parameter", msg)
}
func ErrUpstream(status int, msg string) *APIError {
	if status == 0 {
		status = http.StatusBadGateway
	}
	return newErr(status, ErrTypeAPI, "upstream_error", msg)
}

// AsAPIError extracts an *APIError from err (if any).
func AsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}
