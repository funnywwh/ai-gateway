package httpapi

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/winger/ai-gateway/internal/domain"
)

// This file is the pagination contract of the management surface: every list
// endpoint takes limit + offset and answers with the same envelope, so the console
// (and an MCP agent calling the same handler) can page any list the same way.
//
// Two shapes of list share the contract. History that grows without bound (request
// logs, audit trail, ledger, invoices, codes, reconciliations) is windowed in SQL
// and counted with the same WHERE clause; bounded configuration tables (accounts,
// keys, models, routes, …) are still read whole — the registry reads them whole on
// every reload anyway — and sliced here. See docs/design/m24-console-pagination.md.

// pageParams is one requested window over a list.
type pageParams struct {
	Limit  int
	Offset int
}

// pageSpec is the page-size contract of one list family: the default when limit is
// absent and the ceiling a caller cannot exceed. Both the handler (which parses) and
// the route table (which documents for MCP clients) go through it, so the number an
// agent reads in admin_describe cannot drift from the one the handler enforces.
type pageSpec struct{ Def, Max int }

var (
	// History tables stream from SQL. Their defaults keep the pre-pagination behaviour,
	// so a caller that never passes limit sees exactly the rows it used to.
	pageRequests        = pageSpec{Def: 50, Max: 500}
	pageAudit           = pageSpec{Def: 100, Max: 500}
	pageLedger          = pageSpec{Def: 100, Max: 1000}
	pageInvoices        = pageSpec{Def: 50, Max: 200}
	pageCodes           = pageSpec{Def: 100, Max: 500}
	pageReconciliations = pageSpec{Def: 30, Max: 200}
	pageBackups         = pageSpec{Def: 100, Max: 500}

	// Configuration tables are read whole and sliced in memory: they are bounded by the
	// number of configured objects, which the registry reads wholesale on every reload.
	pageConfig = pageSpec{Def: 200, Max: 1000}
)

// params parses the request's limit/offset against this spec.
func (s pageSpec) params(r *http.Request) (pageParams, error) { return adminPage(r, s.Def, s.Max) }

// fields documents the window in the management route table, which is also what MCP
// clients read through admin_endpoints/admin_describe.
func (s pageSpec) fields() []adminField {
	return []adminField{
		queryParam("limit", "integer", fmt.Sprintf("返回条数上限，默认 %d，最大 %d", s.Def, s.Max)),
		offsetParam(),
	}
}

// offsetParam documents the offset half of the window.
func offsetParam() adminField {
	return queryParam("offset", "integer",
		"偏移量，从 0 开始；与 limit 一起构成分页窗口，省略等于 0，越界返回空页")
}

// adminPage parses limit and offset.
//
// limit keeps adminLimit's forgiving behaviour (absent, malformed or <= 0 means the
// endpoint default; above the cap means the cap). offset is new, and silently
// treating a malformed offset as 0 would answer a request for page 5 with page 1,
// which is the worst possible failure mode for a pager, so it is rejected instead.
func adminPage(r *http.Request, def, max int) (pageParams, error) {
	params := pageParams{Limit: adminLimit(r, def, max)}
	raw := r.URL.Query().Get("offset")
	if raw == "" {
		return params, nil
	}
	offset, err := strconv.Atoi(raw)
	if err != nil || offset < 0 {
		return params, domain.ErrInvalidRequest("offset must be an integer >= 0")
	}
	params.Offset = offset
	return params, nil
}

// sliceWindow takes one window out of an already loaded list. It is the in-memory half
// of the contract: it never reorders rows, so the store's ORDER BY stays the single
// source of the listing order. has_more is not returned because the envelope derives it
// from total, which for these lists is the length of the whole slice.
func sliceWindow[T any](rows []T, p pageParams) []T {
	if p.Offset >= len(rows) {
		return []T{}
	}
	end := p.Offset + p.Limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[p.Offset:end]
}

// writeList renders the one envelope every list endpoint answers with.
func writeList(w http.ResponseWriter, data []map[string]any, total int, p pageParams) {
	writeJSON(w, http.StatusOK, listPayload(data, total, p))
}

// listPayload renders that envelope as a map, for endpoints that add fields of their own
// (/backups carries the directory and the footprint of every backup, not of the page).
//
// count stays the number of rows in this page — existing console code and tests read it —
// while total is the number of rows the filters matched, which is what a pager needs to
// show "共 N 条 / 第 X/Y 页".
func listPayload(data []map[string]any, total int, p pageParams) map[string]any {
	if data == nil {
		data = []map[string]any{}
	}
	return map[string]any{
		"data": data, "count": len(data),
		"total": total, "limit": p.Limit, "offset": p.Offset,
		"has_more": p.Offset+len(data) < total,
	}
}
