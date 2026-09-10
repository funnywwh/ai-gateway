package pluginapi

import "fmt"

// Error is the plugin-side error payload carried by error frames.
type Error struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	// Kind classifies the failure for the router: retryable|quota_exhausted|fatal.
	Kind string `json:"kind,omitempty"`
	// ResetAt is the unix second at which an exhausted upstream quota recovers.
	ResetAt int64 `json:"reset_at,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// ErrKind values.
const (
	KindRetryable      = "retryable"
	KindQuotaExhausted = "quota_exhausted"
	KindFatal          = "fatal"
)

// Errors returned by plugins. The host maps them onto HTTP responses and routing decisions.
func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message, Kind: KindFatal}
}

// NewRetryableError marks a transient upstream failure (the router may fail over).
func NewRetryableError(code, message string, httpStatus int) *Error {
	return &Error{Code: code, Message: message, Retryable: true, HTTPStatus: httpStatus, Kind: KindRetryable}
}

// NewQuotaError marks exhausted upstream quota; resetAt is a unix second (0 = unknown).
func NewQuotaError(message string, resetAt int64) *Error {
	return &Error{
		Code: "quota_exhausted", Message: message, Retryable: true,
		HTTPStatus: 429, Kind: KindQuotaExhausted, ResetAt: resetAt,
	}
}

// IsError extracts a *Error from err.
func IsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	if e, ok := err.(*Error); ok {
		return e, true
	}
	return nil, false
}
