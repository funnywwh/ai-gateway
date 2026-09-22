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

// Model is one model aigw advertises for a key, with the facts it discloses about itself
// (M68). Every field but ID is optional: an older aigw answers only the id, and the zero
// value then means "not disclosed" rather than "unsupported", so a caller renders exactly
// what it was told and nothing more.
type Model struct {
	// ID is the model name a request names.
	ID string
	// Name is the operator-facing display name; "" when aigw disclosed none.
	Name string
	// ContextWindow is the declared context capacity; 0 when not disclosed, never a guess.
	ContextWindow int
	// MaxOutputTokens is the declared output cap; 0 when not disclosed.
	MaxOutputTokens int
	// Images reports that the model accepts image input.
	Images bool
	// ReasoningSupported is the disclosed reasoning capability: true when a route declares
	// it, false when the model's capability set was disclosed without it, and nil when no
	// capability set was disclosed at all. The three lead to different DSH settings — a
	// level table, an explicit non-reasoning declaration, or inheritance — so collapsing
	// them would turn "unknown" into a claim.
	ReasoningSupported *bool
	// ReasoningForced reports that the model's reasoning policy overrides whatever effort a
	// client asks for, which makes a selectable level list a lie.
	ReasoningForced bool
}

// modelRow is one entry of the aigw /v1/models reply. Only the id is decoded strictly:
// every other field is kept raw and read by the tolerant helpers below, so one malformed
// value cannot fail the key validation that gates a tenant's whole model list.
type modelRow struct {
	ID              string          `json:"id"`
	Name            json.RawMessage `json:"name"`
	ContextWindow   json.RawMessage `json:"context_window"`
	MaxOutputTokens json.RawMessage `json:"max_output_tokens"`
	InputModalities json.RawMessage `json:"input_modalities"`
	Capabilities    json.RawMessage `json:"capabilities"`
	Reasoning       json.RawMessage `json:"reasoning"`
}
type modelList struct {
	Data *[]modelRow `json:"data"`
}

// modalityImage is the modality that decides whether a model may be sent an image.
const modalityImage = "image"

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
func (c *Client) ValidateKey(ctx context.Context, key string) ([]Model, error) {
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
	out := make([]Model, 0, len(*payload.Data))
	seen := map[string]bool{}
	for _, m := range *payload.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, disclosedModel(m))
	}
	return out, nil
}

// disclosedModel turns one reply row into the facts a caller can render.
func disclosedModel(row modelRow) Model {
	model := Model{
		ID:              row.ID,
		Name:            disclosedString(row.Name),
		ContextWindow:   disclosedCount(row.ContextWindow),
		MaxOutputTokens: disclosedCount(row.MaxOutputTokens),
	}
	for _, modality := range disclosedStrings(row.InputModalities) {
		if modality == modalityImage {
			model.Images = true
		}
	}
	if capabilities, ok := disclosedCapabilities(row.Capabilities); ok {
		supported := capabilities["reasoning"]
		model.ReasoningSupported = &supported
		// The capability set is the declaration and `input_modalities` is its published
		// reading; either one saying yes is enough, so a consumer never has to know which of
		// the two an endpoint chose to fill in.
		if capabilities[modalityImage] {
			model.Images = true
		}
	}
	model.ReasoningForced = disclosedReasoningMode(row.Reasoning) == "force"
	return model
}

// disclosedString reads an optional string, answering "" for anything else. A display name
// is presentation only: a number where a string belongs must not fail the model list.
func disclosedString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// disclosedCount reads an optional token count, answering 0 (not disclosed) for anything
// that is not a positive whole number. Negative and fractional values are upstream nonsense
// rather than a capacity, and 0 is already this package's word for "unknown".
func disclosedCount(raw json.RawMessage) int {
	var value int
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value < 1 {
		return 0
	}
	return value
}

// disclosedStrings reads an optional array of strings, keeping only entries a caller can act
// on and answering nil for anything else.
func disclosedStrings(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// disclosedCapabilities reads the optional capability set. The second result separates
// "declared nothing supported" (an empty or all-false object: known) from "not disclosed"
// (absent or unreadable), which is the distinction the reasoning level list turns on.
func disclosedCapabilities(raw json.RawMessage) (map[string]bool, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var caps map[string]bool
	if err := json.Unmarshal(raw, &caps); err != nil {
		return nil, false
	}
	return caps, true
}

// disclosedReasoningMode reads the mode of the model's disclosed reasoning policy.
func disclosedReasoningMode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var policy struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		return ""
	}
	return strings.TrimSpace(policy.Mode)
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
	// AccountID is the account the key belongs to (M72). The portal uses it to match a key-pick
	// ticket's account against the tenants it serves; an older aigw leaves it zero, which the
	// picker treats as "cannot resolve".
	AccountID int64 `json:"account_id"`
	// Keys are the account's usable keys (M72): when there is more than one, the portal asks
	// which one this session should be recorded against. An older aigw does not answer the
	// field at all, which the picker treats as "no choice to offer" rather than an error.
	Keys []KeyRef `json:"keys"`
}

// KeyRef is one of an account's keys as the portal may show it: an id, a name and a prefix.
// It never carries key material — the picker only needs something a person recognises, and the
// portal validates the submitted id against the freshly fetched list rather than trusting it.
type KeyRef struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	KeyPrefix  string `json:"key_prefix"`
	LastUsedAt string `json:"last_used_at"`
}

// Identity is who aigw says a key belongs to: the tenant it may enter, the account and names
// the tenant's interface shows (M67), and the keys a login may be recorded against (M72).
type Identity struct {
	Tenant     string
	Account    string
	FeishuName string
	AccountID  int64
	Keys       []KeyRef
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
		AccountID:  payload.AccountID,
		Keys:       payload.Keys,
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
