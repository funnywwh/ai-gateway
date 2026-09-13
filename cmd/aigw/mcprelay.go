package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const mcpRelayMaxBytes = 10 * 1024 * 1024

func validateMCPEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(endpoint, "#") {
		return errors.New("MCP endpoint must be an absolute HTTP(S) URL without credentials, query or fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return errors.New("MCP endpoint requires HTTPS except for loopback HTTP")
}

// serveMCPRelay forwards each message once. Never retry a management operation:
// a lost response does not mean the server failed to commit the write.
func serveMCPRelay(ctx context.Context, input io.Reader, output io.Writer, endpoint, token string, client *http.Client) error {
	if err := validateMCPEndpoint(endpoint); err != nil {
		return err
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return errors.New("missing or invalid MCP token")
	}
	if client == nil {
		client = http.DefaultClient
	}
	relayClient := *client
	relayClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if relayClient.Timeout == 0 {
		relayClient.Timeout = 2 * time.Minute
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), mcpRelayMaxBytes+2)
	writer := bufio.NewWriter(output)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(scanner.Bytes()) > mcpRelayMaxBytes {
			return errors.New("MCP input exceeds 10 MiB")
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if json.Unmarshal(line, &request) != nil || request.JSONRPC != "2.0" || request.Method == "" {
			return errors.New("invalid MCP JSON-RPC request")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(line))
		if err != nil {
			return errors.New("cannot construct MCP request")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := relayClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("MCP HTTP request failed; operation was not retried")
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, mcpRelayMaxBytes+1))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("MCP endpoint returned HTTP %d; operation was not retried", resp.StatusCode)
		}
		if readErr != nil {
			return errors.New("cannot read MCP response; operation was not retried")
		}
		if len(body) > mcpRelayMaxBytes {
			return errors.New("MCP response exceeds 10 MiB")
		}
		if len(bytes.TrimSpace(body)) == 0 && len(request.ID) == 0 {
			continue
		}
		var response struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal(body, &response) != nil || response.JSONRPC != "2.0" || (len(response.Result) == 0) == (len(response.Error) == 0) || !bytes.Equal(bytes.TrimSpace(request.ID), bytes.TrimSpace(response.ID)) {
			return errors.New("invalid MCP JSON-RPC response")
		}
		// Notifications never produce a stdio response, even if an older gateway replies.
		if len(request.ID) == 0 {
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, body); err != nil {
			return errors.New("invalid MCP JSON response")
		}
		if _, err := writer.Write(compact.Bytes()); err != nil {
			return errors.New("cannot write MCP response")
		}
		if err := writer.WriteByte('\n'); err != nil {
			return errors.New("cannot write MCP response")
		}
		if err := writer.Flush(); err != nil {
			return errors.New("cannot flush MCP response")
		}
	}
	if scanner.Err() != nil {
		return errors.New("cannot read MCP input or input exceeds 10 MiB")
	}
	return ctx.Err()
}
