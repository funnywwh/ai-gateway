// Package aigw contains dshgw's only integration with ai-gateway. It uses
// public HTTP APIs and intentionally imports no aigw implementation package.
package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrInvalidKey = errors.New("dshgw: invalid api key")

type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("aigw returned HTTP %d", e.Status)
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
}
type modelRow struct {
	ID string `json:"id"`
}
type modelList struct {
	Data *[]modelRow `json:"data"`
}

var directTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

func (c *Client) httpClient() *http.Client {
	var client http.Client
	if c.HTTP != nil {
		client = *c.HTTP
	}
	if client.Transport == nil {
		client.Transport = directTransport
	}
	if client.Timeout == 0 {
		client.Timeout = 5 * time.Second
	}
	client.Jar = nil
	return &client
}

// ValidateKey treats HTTP 200, including an empty data list, as authenticated.
// Only HTTP 401 means the key itself is invalid.
func (c *Client) ValidateKey(ctx context.Context, key string) ([]string, error) {
	if strings.TrimSpace(key) == "" {
		return nil, ErrInvalidKey
	}
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || base.Scheme != "http" && base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("invalid aigw base URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1/models"
	base.RawQuery = ""
	base.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	baseClient := c.httpClient()
	client := *baseClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("validate key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrInvalidKey
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 1<<20 {
		return nil, errors.New("aigw /v1/models response exceeds 1 MiB")
	}
	var payload modelList
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode /v1/models: %w", err)
	}
	if payload.Data == nil {
		return nil, errors.New("decode /v1/models: missing data array")
	}
	out := make([]string, 0, len(*payload.Data))
	seen := map[string]bool{}
	for _, m := range *payload.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m.ID)
	}
	return out, nil
}

// DSHDenial is aigw's definitive 403 answer: the key and its account are valid, but the
// account is not admitted to the dsh gateway. Reason mirrors aigw's machine-readable cause
// ("dsh_disabled" = the console toggle, "account_status" = suspended/closed account) so the
// portal can show an accurate message instead of guessing.
type DSHDenial struct{ Reason string }

func (e *DSHDenial) Error() string {
	return "dshgw: dsh access denied (" + e.Reason + ")"
}

type dshAuthorize struct {
	Allowed *bool  `json:"allowed"`
	Reason  string `json:"reason"`
	Tenant  string `json:"tenant"`
	// Account and FeishuName name the person this key belongs to (M67). aigw answers them
	// with the admitted decision; both are display data, so an older aigw simply leaves them
	// empty and nothing here fails.
	Account    string `json:"account"`
	FeishuName string `json:"feishu_name"`
}

// Identity is who aigw says a key belongs to: the tenant it may enter, and the names the
// tenant's interface shows for that person (M67).
type Identity struct {
	Tenant     string
	Account    string
	FeishuName string
}

// Authorize asks aigw whether the key's account is opted in to the dsh gateway (M52).
// HTTP 200 with allowed=true answers the account's tenant name (empty on aigw versions
// without tenant mapping — the caller falls back to prefix binding). HTTP 403 becomes
// *DSHDenial. HTTP 401 becomes ErrInvalidKey. Everything else — timeouts, 5xx, malformed
// bodies — returns an error the caller must treat as "authorization unavailable" and fail
// closed.
func (c *Client) Authorize(ctx context.Context, key string) (string, error) {
	identity, err := c.Identity(ctx, key)
	return identity.Tenant, err
}

// Identity is Authorize plus the names (M67). One HTTP call serves both, so a caller that
// wants to show who is signed in does not pay for a second check.
func (c *Client) Identity(ctx context.Context, key string) (Identity, error) {
	if strings.TrimSpace(key) == "" {
		return Identity{}, ErrInvalidKey
	}
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || base.Scheme != "http" && base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return Identity{}, errors.New("invalid aigw base URL")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1/dshgw/authorize"
	base.RawQuery = ""
	base.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String(), nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	baseClient := c.httpClient()
	client := *baseClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("dsh authorize: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return Identity{}, ErrInvalidKey
	}
	if resp.StatusCode == http.StatusForbidden {
		denial := &DSHDenial{Reason: "dsh_disabled"}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
		if err == nil && len(body) <= 64<<10 {
			var payload dshAuthorize
			if json.Unmarshal(body, &payload) == nil && payload.Reason != "" {
				denial.Reason = payload.Reason
			}
		}
		return Identity{}, denial
	}
	if resp.StatusCode != http.StatusOK {
		return Identity{}, &StatusError{Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil {
		return Identity{}, err
	}
	if len(body) > 64<<10 {
		return Identity{}, errors.New("aigw /v1/dshgw/authorize response exceeds 64 KiB")
	}
	var payload dshAuthorize
	if err := json.Unmarshal(body, &payload); err != nil {
		return Identity{}, fmt.Errorf("decode /v1/dshgw/authorize: %w", err)
	}
	if payload.Allowed == nil || !*payload.Allowed {
		return Identity{}, errors.New("decode /v1/dshgw/authorize: allowed is not true")
	}
	return Identity{
		Tenant:     strings.TrimSpace(payload.Tenant),
		Account:    strings.TrimSpace(payload.Account),
		FeishuName: strings.TrimSpace(payload.FeishuName),
	}, nil
}

func NormalizeKey(input string) (string, error) {
	key := strings.TrimSpace(input)
	if len(key) >= 7 && strings.EqualFold(key[:7], "Bearer ") {
		key = strings.TrimSpace(key[7:])
	}
	if len(key) < 12 || len(key) > 8192 {
		return "", errors.New("API key must contain 12 to 8192 bytes")
	}
	for _, b := range []byte(key) {
		if b < 0x21 || b > 0x7e {
			return "", errors.New("API key must contain printable ASCII without whitespace")
		}
	}
	return key, nil
}

func KeyPrefix(input string) (string, error) {
	key, err := NormalizeKey(input)
	if err != nil {
		return "", err
	}
	return key[:12], nil
}
