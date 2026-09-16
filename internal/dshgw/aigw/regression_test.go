package aigw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidationTimeoutIsNotInvalidKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := (&Client{BaseURL: server.URL}).ValidateKey(ctx, "sk-abcdefghijkl")
	if err == nil || errors.Is(err, ErrInvalidKey) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout misclassified: %v", err)
	}
}

func TestErrorBodyIsNotReturnedOrLogged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("echo sk-do-not-log-this"))
	}))
	defer server.Close()
	_, err := (&Client{BaseURL: server.URL}).ValidateKey(context.Background(), "sk-abcdefghijkl")
	if err == nil || strings.Contains(err.Error(), "sk-do-not-log-this") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestUnauthorizedDoesNotRequireReadableBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := (&Client{BaseURL: server.URL}).ValidateKey(ctx, "sk-abcdefghijkl")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("401 misclassified: %v", err)
	}
}
