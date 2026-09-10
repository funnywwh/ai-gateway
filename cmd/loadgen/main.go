// Command loadgen is a dependency-free load generator for the gateway. It exists
// because the environment has no hey/wrk/ab, and because a reproducible load test
// belongs next to the code it measures.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/v1/responses", "endpoint to call")
	key := flag.String("key", "", "API key (sk-gw-...)")
	model := flag.String("model", "echo", "model to request")
	concurrency := flag.Int("concurrency", 32, "parallel workers")
	duration := flag.Duration("duration", 20*time.Second, "how long to run")
	stream := flag.Bool("stream", false, "use streaming and read until response.completed")
	input := flag.String("input", "ping", "input text")
	maxTokens := flag.Int("max-output-tokens", 0, "max_output_tokens (0 omits it)")
	flag.Parse()

	if *key == "" {
		fmt.Fprintln(os.Stderr, "loadgen: -key is required")
		os.Exit(2)
	}
	body := map[string]any{"model": *model, "input": *input}
	if *stream {
		body["stream"] = true
	}
	if *maxTokens > 0 {
		body["max_output_tokens"] = *maxTokens
	}
	payload, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			MaxConnsPerHost:     *concurrency * 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	var (
		requests  atomic.Int64
		succeeded atomic.Int64
		failed    atomic.Int64
		totalUS   atomic.Int64
		latencies = make([][]time.Duration, *concurrency)
		statuses  sync.Map
	)

	var wait sync.WaitGroup
	for worker := 0; worker < *concurrency; worker++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			samples := make([]time.Duration, 0, 4096)
			for ctx.Err() == nil {
				started := time.Now()
				status, err := one(ctx, client, *url, *key, payload, *stream)
				elapsed := time.Since(started)
				samples = append(samples, elapsed)
				requests.Add(1)
				totalUS.Add(elapsed.Microseconds())
				if err != nil || status >= 400 {
					failed.Add(1)
					key := fmt.Sprintf("%d", status)
					if err != nil {
						key = "transport_error"
					}
					value, _ := statuses.LoadOrStore(key, new(atomic.Int64))
					value.(*atomic.Int64).Add(1)
					continue
				}
				succeeded.Add(1)
			}
			latencies[index] = samples
		}(worker)
	}
	wait.Wait()

	all := []time.Duration{}
	for _, samples := range latencies {
		all = append(all, samples...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	if len(all) == 0 {
		fmt.Println("loadgen: no requests completed")
		return
	}

	total := requests.Load()
	fmt.Printf("requests=%d ok=%d failed=%d\n", total, succeeded.Load(), failed.Load())
	fmt.Printf("rps=%.1f\n", float64(total)/duration.Seconds())
	fmt.Printf("latency avg=%s p50=%s p90=%s p95=%s p99=%s max=%s\n",
		time.Duration(totalUS.Load()/total)*time.Microsecond,
		percentile(all, 0.50), percentile(all, 0.90), percentile(all, 0.95), percentile(all, 0.99), all[len(all)-1])
	statuses.Range(func(key, value any) bool {
		fmt.Printf("status[%v]=%d\n", key, value.(*atomic.Int64).Load())
		return true
	})
}

// one performs a single request and, for streams, reads until the terminal event.
func one(ctx context.Context, client *http.Client, url, key string, payload []byte, stream bool) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if !stream {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "response.completed") || strings.Contains(line, "response.failed") {
			break
		}
	}
	return resp.StatusCode, scanner.Err()
}

func percentile(sorted []time.Duration, fraction float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1) * fraction)
	if index < 0 {
		index = 0
	}
	return sorted[index]
}
