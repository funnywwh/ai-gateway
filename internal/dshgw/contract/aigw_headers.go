package contract

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
)

func checkAPIKeyHeader(ctx context.Context, baseURL, key string) error {
	client := &aigw.Client{BaseURL: baseURL}
	bearer, err := client.ValidateKey(ctx, key)
	if err != nil {
		return err
	}
	target, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return errors.New("invalid aigw contract URL")
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", key)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := httpClient.Do(req)
	if err != nil {
		return errors.New("aigw X-API-Key contract request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("X-API-Key did not authenticate like Bearer")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return errors.New("invalid X-API-Key model-list response")
	}
	var payload struct {
		Data *[]struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Data == nil {
		return errors.New("invalid X-API-Key model-list data array")
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(*payload.Data))
	for _, row := range *payload.Data {
		if row.ID != "" && !seen[row.ID] {
			seen[row.ID] = true
			ids = append(ids, row.ID)
		}
	}
	bearerIDs := make([]string, 0, len(bearer))
	for _, model := range bearer {
		bearerIDs = append(bearerIDs, model.ID)
	}
	sort.Strings(ids)
	sort.Strings(bearerIDs)
	if !reflect.DeepEqual(ids, bearerIDs) {
		return errors.New("Bearer and X-API-Key produced different model lists; retry if model availability changed")
	}
	return nil
}
