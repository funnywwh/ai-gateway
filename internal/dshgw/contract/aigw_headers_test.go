package contract

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAigwHeaderContracts(t *testing.T) {
	const key = "sk-contract-valid-key"
	for _, empty := range []bool{true, false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+key && r.Header.Get("X-API-Key") != key {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if empty {
				_, _ = w.Write([]byte(`{"data":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"data":[{"id":"z"},{"id":"a"}]}`))
			}
		}))
		report := Run(context.Background(), AigwChecks(server.URL, key))
		server.Close()
		if !report.OK || len(report.Results) != 3 {
			t.Fatalf("empty=%v report=%+v", empty, report)
		}
	}
}

func TestAigwHeaderContractRejectsUnequalAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	if err := checkAPIKeyHeader(context.Background(), server.URL, "sk-contract-valid-key"); err == nil {
		t.Fatal("unequal header authentication accepted")
	}
}
