package httpapi

import (
	"net/http"
	"strings"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/orgtree"
)

// The company registry of the Feishu organization sync (M92).
//
// Companies come from the configuration, so this file has no write path for the list itself —
// what it does is answer "which companies can I import, and where does each one's subtree hang".
// The two questions an operator asks before pressing 同步: is this company's node already there
// (or will it be created), and how much of the directory is already mapped.

// handleAdminListFeishuCompanies answers the console's company dropdown.
func (s *Server) handleAdminListFeishuCompanies(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return
	}
	companies := s.feishuCompanies()
	if len(companies) == 0 {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return
	}
	orgStore, ok := portReady(w, s.deps.Org, "organization management")
	if !ok {
		return
	}
	accounts, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	people, ok := portReady(w, s.deps.FeishuPeople, "feishu company person mapping")
	if !ok {
		return
	}
	ctx := r.Context()
	nodes, err := orgStore.ListOrgNodes(ctx)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	links, err := people.ListFeishuPersonLinks(ctx)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	allAccounts, err := accounts.ListAccounts(ctx)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	index := orgtree.NewIndex(nodes)
	nodesByApp := map[string]int{}
	for _, node := range nodes {
		if node.FeishuAppID != "" {
			nodesByApp[node.FeishuAppID]++
		}
	}
	// Distinct accounts per company: one account may map to people in several companies, and a
	// count of rows would then say "2 accounts" for one person.
	accountsByApp := map[string]map[int64]bool{}
	for _, link := range links {
		if accountsByApp[link.AppID] == nil {
			accountsByApp[link.AppID] = map[int64]bool{}
		}
		accountsByApp[link.AppID][link.AccountID] = true
	}
	identityLogins := 0
	for _, account := range allAccounts {
		if account.FeishuOpenID != "" {
			identityLogins++
		}
	}

	data := make([]map[string]any, 0, len(companies))
	for i := range companies {
		company := companies[i]
		root := planCompanyRoot(&company, nodes, index)
		linked := len(accountsByApp[company.AppID])
		if company.Identity {
			// The identity application's people are bound on the account itself (that column is
			// the portal login), so its mapping count comes from there.
			linked = identityLogins
		}
		data = append(data, map[string]any{
			"app_id":          company.AppID,
			"name":            company.Name,
			"identity":        company.Identity,
			"root_node_id":    jsonNilInt64(root.NodeID),
			"root_node_name":  root.Name,
			"root_matched":    root.Matched,
			"root_will_create": root.WillCreate,
			"company_nodes":   nodesByApp[company.AppID],
			"linked_accounts": linked,
		})
	}
	identityAppID := ""
	for i := range companies {
		if companies[i].Identity {
			identityAppID = companies[i].AppID
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":            data,
		"count":           len(data),
		"identity_app_id": identityAppID,
	})
}

// handleAdminPurgeFeishuCompanyLinks drops every person mapping of one company (offboarding a
// client): the links go, the org nodes, memberships, accounts and keys stay — those are separate
// decisions an operator makes with the node and account endpoints.
func (s *Server) handleAdminPurgeFeishuCompanyLinks(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return
	}
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	company, ok := s.resolveFeishuCompany(w, strings.TrimSpace(r.PathValue("app_id")))
	if !ok {
		return
	}
	if company.Identity {
		writeAPIError(w, domain.ErrInvalidRequest(
			"这是本部署的身份应用（本公司）：它的人员映射就是账号上的登录身份，请用 DELETE /org/feishu/users/{open_id}/account 逐人解绑"))
		return
	}
	people, ok := portReady(w, s.deps.FeishuPeople, "feishu company person mapping")
	if !ok {
		return
	}
	deleted, err := people.DeleteFeishuPersonLinksByApp(r.Context(), company.AppID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if deleted > 0 {
		s.audit(r.Context(), actor.Username, "purge_feishu_links", "feishu_company", company.AppID,
			map[string]any{"company": company.AppID, "deleted": deleted}, "ok")
	}
	s.invalidateFeishuDirectory()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "company": feishuCompanyJSON(company, nil), "deleted_links": deleted,
	})
}
