package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// The route table promises a natural-month period ("或自然月如 2026-08") and the
// console offers current/previous; before M24 the handler understood only the three
// keywords, so the documented form came back as 400. These two cases keep the
// documented contract and the code together.

func TestAdminBuildInvoiceAcceptsNaturalMonth(t *testing.T) {
	f := newAdminFixture(t)
	cookie := f.login(t, adminUser, adminPassword)
	account, err := f.db.GetAccountByName(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/api/v1/accounts/%d/invoices", account.ID)

	for _, period := range []string{"2026-06", "2026-07"} {
		resp := f.call(t, http.MethodPost, path, `{"period":"`+period+`"}`, cookie)
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("period %s status = %d, want 200/201", period, resp.StatusCode)
		}
		payload := decodeJSONBody(t, resp)
		start, _ := payload["period_start"].(string)
		if start[:7] != period {
			t.Fatalf("period %s produced period_start %q", period, start)
		}
	}

	// Two months, two invoices — and the keyword form keeps working.
	resp := f.call(t, http.MethodPost, path, `{"period":"previous"}`, cookie)
	resp.Body.Close()
	listed := mustPage(t, f, cookie, fmt.Sprintf("/admin/api/v1/invoices?account_id=%d&limit=10", account.ID))
	if total, _ := listed["total"].(float64); total != 3 {
		t.Fatalf("invoices total = %v, want 3 (two months + previous)", listed["total"])
	}

	// A typo still fails loudly instead of silently billing the current period.
	bad := f.call(t, http.MethodPost, path, `{"period":"2026-6"}`, cookie)
	status, code := decodeError(t, bad)
	if status != http.StatusBadRequest || code != "invalid_request" {
		t.Fatalf("period=2026-6 status=%d code=%s, want 400 invalid_request", status, code)
	}
}
