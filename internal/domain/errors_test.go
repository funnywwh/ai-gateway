package domain

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestErrorStatuses(t *testing.T) {
	cases := []struct {
		err    *APIError
		status int
		code   string
	}{
		{ErrInvalidRequest("bad"), http.StatusBadRequest, "invalid_request"},
		{ErrUnauthorized("no"), http.StatusUnauthorized, "invalid_api_key"},
		{ErrForbidden("nope"), http.StatusForbidden, "permission_denied"},
		{ErrModelNotFound("gpt-x"), http.StatusNotFound, "model_not_found"},
		{ErrRateLimited("slow"), http.StatusTooManyRequests, "rate_limit_exceeded"},
		{ErrInsufficientQuota("broke"), http.StatusPaymentRequired, "billing_hard_limit_reached"},
		{ErrUnsupported("nope"), http.StatusBadRequest, "unsupported_parameter"},
	}
	for _, tc := range cases {
		if tc.err.Status != tc.status {
			t.Errorf("%s status = %d, want %d", tc.err.Code, tc.err.Status, tc.status)
		}
		if tc.err.Code != tc.code {
			t.Errorf("code = %q, want %q", tc.err.Code, tc.code)
		}
	}
}

func TestUpstreamDefaultsTo502(t *testing.T) {
	if got := ErrUpstream(0, "boom").Status; got != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", got)
	}
	if got := ErrUpstream(http.StatusServiceUnavailable, "boom").Status; got != http.StatusServiceUnavailable {
		t.Fatalf("status pass-through failed: %d", got)
	}
}

func TestAsAPIError(t *testing.T) {
	wrapped := fmt.Errorf("context: %w", ErrInvalidRequest("bad input"))
	ae, ok := AsAPIError(wrapped)
	if !ok {
		t.Fatal("AsAPIError must unwrap")
	}
	if ae.Code != "invalid_request" {
		t.Fatalf("code = %q", ae.Code)
	}
	if _, ok := AsAPIError(errors.New("plain")); ok {
		t.Fatal("plain error must not match")
	}
}

func TestWithParamAndCause(t *testing.T) {
	cause := errors.New("root cause")
	e := ErrInvalidRequest("").WithParam("input").WithCause(cause)
	if e.Param != "input" {
		t.Errorf("param = %q", e.Param)
	}
	if !errors.Is(e, cause) {
		t.Errorf("cause not unwrappable")
	}
	if e.Error() == "" {
		t.Errorf("Error() must not be empty")
	}
}
