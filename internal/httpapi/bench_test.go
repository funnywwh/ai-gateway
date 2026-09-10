package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The benchmarks run the whole HTTP surface in process against the testecho provider,
// so they measure the gateway itself (auth, routing, metering, settlement) without a
// network stack in the way. Numbers are environment-dependent; the shape is what
// matters: the cached-auth path must not be dominated by anything below it.

func BenchmarkResponsesNonStreaming(b *testing.B) {
	f := newFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		resp := f.do(b, http.MethodPost, "/v1/responses", nonStreamBody, nil)
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status = %d", resp.StatusCode)
		}
		_, _ = resp.Body.Read(make([]byte, 0))
		resp.Body.Close()
	}
}

func BenchmarkResponsesStreaming(b *testing.B) {
	f := newFixture(b)
	body := `{"model":"echo-model","input":"ping","stream":true}`
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		resp := f.do(b, http.MethodPost, "/v1/responses", body, nil)
		body, _ := readAllBody(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), "response.completed") {
			b.Fatalf("stream did not complete: %s", body)
		}
	}
}

// BenchmarkParallelResponses is the concurrency check: many requests at once through
// the same server, which is where the single-writer settlement and the sharded
// limiter have to hold up.
func BenchmarkParallelResponses(b *testing.B) {
	f := newFixture(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp := f.do(b, http.MethodPost, "/v1/responses", nonStreamBody, nil)
			if resp.StatusCode != http.StatusOK {
				b.Errorf("status = %d", resp.StatusCode)
				return
			}
			_, _ = readAllBody(resp.Body)
			resp.Body.Close()
		}
	})
}

func BenchmarkListModels(b *testing.B) {
	f := newFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		resp := f.do(b, http.MethodGet, "/v1/models", "", nil)
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status = %d", resp.StatusCode)
		}
		_, _ = readAllBody(resp.Body)
		resp.Body.Close()
	}
}

func readAllBody(body io.ReadCloser) ([]byte, error) {
	return io.ReadAll(body)
}
