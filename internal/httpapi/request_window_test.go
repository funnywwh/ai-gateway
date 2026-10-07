package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/funnywwh/ai-gateway/internal/domain"
)

// The window of a request-log read has two spellings (M97): the rolling `days` window that
// has been there since M24, and an explicit RFC3339 `from`/`to` pair. The pair exists
// because a calendar window — 「当天」「本周」「the 1st to the 3rd」 — is not a number of
// days, and its boundaries belong to the timezone the operator reads the screen in. The
// console resolves them locally and sends absolute instants, so what the gateway has to get
// right is narrower and testable here: parsing, the closed interval, the precedence, and
// the echoed window (docs/design/m97-request-log-time-window.md).
//
// Every case reads the rows the endpoint actually returned rather than the resolved filter,
// because the filter is not part of the contract — a window that parses but never reaches
// the SQL would pass a filter-level test and still show the wrong rows.

// seedWindowRows writes one row per given age, all metered, in one account/key so a
// filtered read can only differ by its window.
func seedWindowRows(t *testing.T, f *adminFixture, labels []string, ages []time.Duration) {
	t.Helper()
	now := time.Now().UTC()
	for i, label := range labels {
		seedIdentityRow(t, f, &domain.RequestLogRecord{
			RequestID: "req_win" + label, AccountID: 1, APIKeyID: 1, Endpoint: "/v1/responses",
			Status: "completed", Client: "dsh", Model: "luna", CreatedAt: now.Add(-ages[i]),
		})
	}
}

// requestIDs lists the ids of a list-endpoint page.
func requestIDs(t *testing.T, payload map[string]any) []string {
	t.Helper()
	rows, _ := payload["data"].([]any)
	ids := make([]string, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		id, _ := row["request_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func windowHasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestAdminRequestLogExplicitWindowFiltersRows is the core of the feature: the rows come
// back for the interval that was asked for, boundaries included.
//
// The closed interval is asserted at the second, not the day: `to` is compared with `<=`,
// and the console expresses "through the 3rd" as 23:59:59 — if the endpoint used `<`, that
// last second of the day would be silently missing from the audit trail of exactly the
// window an operator asked about.
func TestAdminRequestLogExplicitWindowFiltersRows(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	now := time.Now().UTC().Truncate(time.Second)

	// Four rows: inside the interval, exactly on each boundary, and outside it.
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_inside0001", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", CreatedAt: now.Add(-2 * time.Hour)})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_onfrom0001", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", CreatedAt: now.Add(-3 * time.Hour)})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_onto000001", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", CreatedAt: now.Add(-time.Hour)})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_before0001", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", CreatedAt: now.Add(-5 * time.Hour)})

	from := now.Add(-3 * time.Hour).Format(time.RFC3339)
	to := now.Add(-time.Hour).Format(time.RFC3339)
	payload := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests?from="+from+"&to="+to+"&limit=50", "", cookie))
	ids := requestIDs(t, payload)
	for _, want := range []string{"req_inside0001", "req_onfrom0001", "req_onto000001"} {
		if !windowHasID(ids, want) {
			t.Errorf("%s must be inside the closed interval [from, to]: %v", want, ids)
		}
	}
	if windowHasID(ids, "req_before0001") {
		t.Errorf("a row before `from` must not be returned: %v", ids)
	}

	// from only: the right edge is the server's "now", so a row from an hour ago is in and
	// one from five hours ago is out. This is the shape 当天/本周/本月 use.
	payload = decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests?from="+from+"&limit=50", "", cookie))
	ids = requestIDs(t, payload)
	if !windowHasID(ids, "req_onto000001") || windowHasID(ids, "req_before0001") {
		t.Errorf("from without to must run up to now: %v", ids)
	}

	// The dimension breakdown reads the same window: one shared filter, so a bucket count
	// that ignored it would contradict the list it sits above.
	dimensions := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests/dimensions?group_by=client&from="+from+"&to="+to+"&limit=10", "", cookie))
	buckets, _ := dimensions["rows"].([]any)
	if len(buckets) != 1 {
		t.Fatalf("one bucket is in the window, got %v", buckets)
	}
	bucket, _ := buckets[0].(map[string]any)
	if got, _ := bucket["requests"].(float64); got != 3 {
		t.Errorf("the bucket must count the three in-window requests, got %v", bucket["requests"])
	}
}

// The explicit pair wins over `days`, and the response says so. Without the echo an
// operator (or an agent) reading `days=1` back cannot tell why ten days of rows came back.
func TestAdminRequestLogWindowEchoAndPrecedence(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	now := time.Now().UTC().Truncate(time.Second)
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_old00000001", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", CreatedAt: now.AddDate(0, 0, -20)})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_recent00001", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", CreatedAt: now.Add(-time.Minute)})

	from := now.AddDate(0, 0, -30).Format(time.RFC3339)
	// days=1 would exclude the 20-day-old row; the explicit 30-day window must not.
	payload := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests?days=1&from="+from+"&limit=50", "", cookie))
	ids := requestIDs(t, payload)
	if !windowHasID(ids, "req_old00000001") {
		t.Errorf("an explicit from must win over days: %v", ids)
	}
	window, _ := payload["window"].(map[string]any)
	if window == nil {
		t.Fatal("the list response must echo the window it used")
	}
	if got, _ := window["from"].(string); got != from {
		t.Errorf("echoed from = %q, want %q", got, from)
	}
	if to, _ := window["to"].(string); to == "" {
		t.Error("a from-only window must still report the right edge it applied")
	}
	// The echoed window is a real instant, not the request text: it round-trips.
	if _, err := time.Parse(time.RFC3339, window["to"].(string)); err != nil {
		t.Errorf("echoed to must be RFC3339: %v", err)
	}

	// The rolling window still echoes a window of its own, so a caller has one shape to read.
	payload = decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests?days=1&limit=50", "", cookie))
	window, _ = payload["window"].(map[string]any)
	fromAt, _ := time.Parse(time.RFC3339, window["from"].(string))
	toAt, _ := time.Parse(time.RFC3339, window["to"].(string))
	if span := toAt.Sub(fromAt); span < 23*time.Hour || span > 25*time.Hour {
		t.Errorf("days=1 must span about a day, got %v", span)
	}
	if windowHasID(requestIDs(t, payload), "req_old00000001") {
		t.Error("days=1 must not include a 20-day-old row")
	}

	// The breakdown echoes the same window and keeps its own `days` parameter echo (M31):
	// the two answer different questions ("what was asked" vs "what was applied").
	dimensions := decodeJSONBody(t, f.call(t, http.MethodGet,
		"/admin/api/v1/requests/dimensions?days=1&group_by=client&limit=10", "", cookie))
	if dimensions["window"] == nil {
		t.Error("the dimensions response must echo the window it used")
	}
	if days, _ := dimensions["days"].(float64); days != 1 {
		t.Errorf("the dimensions response must keep echoing the days parameter, got %v", dimensions["days"])
	}
}

// A malformed window is rejected instead of ignored: answering "everything" (or "the last
// 7 days") to a question about a specific interval is the failure mode account_id was
// already fixed for, and it is worse here because the answer looks like data.
func TestAdminRequestLogWindowRejectsBadInput(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	now := time.Now().UTC()
	valid := now.Add(-time.Hour).Format(time.RFC3339)

	for _, tc := range []struct {
		name   string
		query  string
		param  string
		reason string
	}{
		{"from is not a time", "from=yesterday", "from", "text is not an instant"},
		{"to is not a time", "to=2026-10-07", "to", "a bare date is not an instant"},
		{"from is empty-looking", "from=+&to=" + valid, "from", "a stray character is not an instant"},
		{"from after to", "from=" + now.Format(time.RFC3339) + "&to=" + valid, "from", "an inverted interval is empty"},
		{"span too wide", "from=" + now.AddDate(0, 0, -400).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339),
			"from", "a 400-day window is a full scan"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{
				"/admin/api/v1/requests?" + tc.query,
				"/admin/api/v1/requests/dimensions?group_by=client&" + tc.query,
			} {
				resp := f.call(t, http.MethodGet, path, "", cookie)
				if resp.StatusCode != http.StatusBadRequest {
					resp.Body.Close()
					t.Fatalf("%s: status = %d, want 400 (%s)", path, resp.StatusCode, tc.reason)
				}
				payload := decodeJSONBody(t, resp)
				envelope, _ := payload["error"].(map[string]any)
				if got, _ := envelope["param"].(string); got != tc.param {
					t.Errorf("%s: error.param = %q, want %q", path, got, tc.param)
				}
			}
		})
	}

	// An empty value is "not given" rather than an error: api.js never sends one, but a
	// hand-written URL or a form round-trip can, and dropping a filter is not the same
	// mistake as a typo in a date.
	payload := decodeJSONBody(t, f.call(t, http.MethodGet, "/admin/api/v1/requests?from=&to=&limit=5", "", cookie))
	window, _ := payload["window"].(map[string]any)
	if window == nil {
		t.Fatal("empty from/to must fall back to the default window")
	}
}

// The rolling window keeps its own contract unchanged: this is the endpoint every existing
// caller (and the console's default) still uses, and M97 must not have moved it.
func TestAdminRequestLogDaysWindowUnchanged(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	now := time.Now().UTC()
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_today000001", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", CreatedAt: now.Add(-time.Hour)})
	seedIdentityRow(t, f, &domain.RequestLogRecord{
		RequestID: "req_tendays0001", AccountID: 1, APIKeyID: 1, Status: "completed",
		Client: "dsh", CreatedAt: now.AddDate(0, 0, -10)})

	for _, tc := range []struct {
		days    string
		wantOld bool
	}{
		{"1", false},
		{"30", true},
		// The bounds are the documented ones: an out-of-range value has always fallen back
		// to 7 rather than erroring, and that behaviour is what an existing link relies on.
		// (999 is therefore a 7-day window, not a 999-day one.)
		{"0", false},
		{"999", false},
	} {
		t.Run("days="+tc.days, func(t *testing.T) {
			payload := decodeJSONBody(t, f.call(t, http.MethodGet,
				"/admin/api/v1/requests?days="+tc.days+"&limit=50", "", cookie))
			ids := requestIDs(t, payload)
			if !windowHasID(ids, "req_today000001") {
				t.Errorf("the recent row must always be in window: %v", ids)
			}
			if got := windowHasID(ids, "req_tendays0001"); got != tc.wantOld {
				t.Errorf("days=%s includes the 10-day-old row = %v, want %v", tc.days, got, tc.wantOld)
			}
		})
	}
}
