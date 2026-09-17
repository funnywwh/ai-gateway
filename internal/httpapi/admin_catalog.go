package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/ids"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/orgtree"
	"github.com/winger/ai-gateway/internal/pricing"
	"github.com/winger/ai-gateway/internal/secret"
)

// ---------------------------------------------------------------------------
// accounts
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	list, err := store.ListAccounts(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// The organization columns and the organization filter both need the tree and the
	// memberships. They are read once here rather than per row, and only when an organization
	// port exists: a deployment without one answers exactly as it did before.
	members := map[int64][]int64{}
	index := orgtree.NewIndex(nil)
	if orgStore, ready := portReadyNoWrite(s.deps.Org); ready {
		nodes, err := orgStore.ListOrgNodes(r.Context())
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		index = orgtree.NewIndex(nodes)
		all, err := orgStore.ListOrgMemberships(r.Context())
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		members = orgMembershipsByAccount(all)
	}

	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		if !accountInOrgFilter(r, index, members[a.ID]) {
			continue
		}
		nodeIDs, refs := orgRefsForAccounts(index, members[a.ID])
		out = append(out, accountJSON(a, nodeIDs, refs))
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	// total counts what the filters matched, not the whole table: a pager that ignored the
	// organization filter would offer pages that are empty.
	writeList(w, window, len(out), page)
}

// accountInOrgFilter applies the optional org_node_id / include_descendants query pair.
//
// The default is to include descendants, because "show me this division" almost always means
// the whole division; include_descendants=false is how a caller asks for exactly the accounts
// attached to that one node.
func accountInOrgFilter(r *http.Request, index *orgtree.Index, nodeIDs []int64) bool {
	raw := strings.TrimSpace(r.URL.Query().Get("org_node_id"))
	if raw == "" {
		return true
	}
	wanted, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || wanted <= 0 {
		return true
	}
	if includeDescendants(r) {
		// Computed once per row: the subtree of the requested node does not change while a
		// single response is rendered.
		subtree := index.Descendants(wanted)
		for _, nodeID := range nodeIDs {
			if slices.Contains(subtree, nodeID) {
				return true
			}
		}
		return false
	}
	return slices.Contains(nodeIDs, wanted)
}

// includeDescendants reads the flag that decides whether an organization filter covers the
// subtree. It defaults to true, so only an explicit false narrows the query.
func includeDescendants(r *http.Request) bool {
	raw := strings.TrimSpace(r.URL.Query().Get("include_descendants"))
	if raw == "" {
		return true
	}
	value, err := strconv.ParseBool(raw)
	return err != nil || value
}

func (s *Server) handleAdminCreateAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	var body struct {
		Name                      string   `json:"name"`
		BillingMode               string   `json:"billing_mode"`
		Note                      string   `json:"note"`
		Tags                      []string `json:"tags"`
		OrgNodeIDs                []int64  `json:"org_node_ids"`
		CreditLimitMicros         *int64   `json:"credit_limit_micros"`
		LowBalanceThresholdMicros *int64   `json:"low_balance_threshold_micros"`
		OverdraftLimitMicros      *int64   `json:"overdraft_limit_micros"`
		MarkupOverrideBP          *int     `json:"markup_override_bp"`
		AutoSuspend               *bool    `json:"auto_suspend"`
		AutoResume                *bool    `json:"auto_resume"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	// An account name is a human-facing label: it may be an email address, Chinese
	// text or any other Unicode, and only a blank or overlong value is refused.
	name, err := domain.NormalizeAccountName(body.Name)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	mode := body.BillingMode
	if mode == "" {
		mode = string(domain.BillingPrepaid)
	}
	if !validBillingModes[mode] {
		writeAPIError(w, domain.ErrInvalidRequest("billing_mode must be prepaid or postpaid"))
		return
	}
	// New accounts always start empty: balances only move through the ledger (M11).
	a := &domain.Account{
		Name: name, BillingMode: domain.BillingMode(mode), Note: body.Note,
		TagsJSON: marshalOrEmpty(body.Tags),
		Status:   "active", AutoSuspend: true, AutoResume: false,
	}
	if body.CreditLimitMicros != nil {
		a.CreditLimitMicros = *body.CreditLimitMicros
	}
	if body.LowBalanceThresholdMicros != nil {
		a.LowBalanceThresholdMicros = *body.LowBalanceThresholdMicros
	}
	if body.OverdraftLimitMicros != nil {
		a.OverdraftLimitMicros = *body.OverdraftLimitMicros
	}
	if body.MarkupOverrideBP != nil {
		a.MarkupOverrideBP = *body.MarkupOverrideBP
		// 0 is a meaningful override (free usage), so record that it was chosen.
		a.MarkupOverrideSet = true
	}
	if body.AutoSuspend != nil {
		a.AutoSuspend = *body.AutoSuspend
	}
	if body.AutoResume != nil {
		a.AutoResume = *body.AutoResume
	}
	id, err := store.UpsertAccount(r.Context(), a)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	// The account exists now, so its organization memberships can be attached. Doing it after
	// the insert is what makes them set in one place (the account side owns the relation) and
	// keeps a failed membership write from leaving a half-created account.
	nodeIDs, refs, apiErr := s.applyAccountOrgNodes(r, id, body.OrgNodeIDs)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	s.audit(r.Context(), actor.Username, "create", "account", strconv.FormatInt(id, 10),
		map[string]any{"name": a.Name, "billing_mode": mode, "tags_set": body.Tags != nil,
			"org_node_ids": nodeIDs}, "ok")
	s.reload(r.Context(), "account created", true)
	writeJSON(w, http.StatusCreated, accountJSON(a, nodeIDs, refs))
}

// applyAccountOrgNodes replaces an account's organization memberships and returns the stored
// ids plus their rendered references.
//
// A nil list means "the caller did not mention organizations", which leaves the memberships
// alone; an empty (non-nil) list means "remove it from every organization". Those two are
// different requests and conflating them would silently drop a placement on any unrelated
// field update.
func (s *Server) applyAccountOrgNodes(r *http.Request, accountID int64, nodeIDs []int64) ([]int64, []map[string]any, *domain.APIError) {
	orgStore, ready := portReadyNoWrite(s.deps.Org)
	if !ready {
		if len(nodeIDs) > 0 {
			return nil, nil, domain.ErrUnsupported("organization management is disabled in this deployment")
		}
		return []int64{}, []map[string]any{}, nil
	}
	if nodeIDs != nil {
		if err := orgStore.SetAccountOrgNodes(r.Context(), accountID, nodeIDs); err != nil {
			return nil, nil, toAPIError(err)
		}
	}
	stored, err := orgStore.ListOrgMemberships(r.Context())
	if err != nil {
		return nil, nil, toAPIError(err)
	}
	byAccount := orgMembershipsByAccount(stored)
	nodes, err := orgStore.ListOrgNodes(r.Context())
	if err != nil {
		return nil, nil, toAPIError(err)
	}
	index := orgtree.NewIndex(nodes)
	ids, refs := orgRefsForAccounts(index, byAccount[accountID])
	return ids, refs, nil
}

func (s *Server) handleAdminPatchAccount(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	a, err := store.GetAccount(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var body struct {
		BillingMode               *string         `json:"billing_mode"`
		Status                    *string         `json:"status"`
		Note                      *string         `json:"note"`
		CreditLimitMicros         *int64          `json:"credit_limit_micros"`
		LowBalanceThresholdMicros *int64          `json:"low_balance_threshold_micros"`
		OverdraftLimitMicros      *int64          `json:"overdraft_limit_micros"`
		MarkupOverrideBP          *int            `json:"markup_override_bp"`
		AutoSuspend               *bool           `json:"auto_suspend"`
		AutoResume                *bool           `json:"auto_resume"`
		Tags                      *[]string       `json:"tags"`
		OrgNodeIDs                *[]int64        `json:"org_node_ids"`
		InflightPolicyOverride    *string         `json:"inflight_policy_override"`
		PriceOverrides            json.RawMessage `json:"price_overrides"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	status := a.Status
	if body.BillingMode != nil {
		if !validBillingModes[*body.BillingMode] {
			writeAPIError(w, domain.ErrInvalidRequest("billing_mode must be prepaid or postpaid"))
			return
		}
		a.BillingMode = domain.BillingMode(*body.BillingMode)
	}
	if body.Status != nil {
		switch *body.Status {
		case "active", "suspended", "closed":
			status = *body.Status
		default:
			writeAPIError(w, domain.ErrInvalidRequest("status must be active, suspended or closed"))
			return
		}
	}
	if body.Note != nil {
		a.Note = *body.Note
	}
	if body.CreditLimitMicros != nil {
		a.CreditLimitMicros = *body.CreditLimitMicros
	}
	if body.LowBalanceThresholdMicros != nil {
		a.LowBalanceThresholdMicros = *body.LowBalanceThresholdMicros
	}
	if body.OverdraftLimitMicros != nil {
		a.OverdraftLimitMicros = *body.OverdraftLimitMicros
	}
	if body.MarkupOverrideBP != nil {
		a.MarkupOverrideBP = *body.MarkupOverrideBP
		a.MarkupOverrideSet = true
	}
	if body.AutoSuspend != nil {
		a.AutoSuspend = *body.AutoSuspend
	}
	if body.AutoResume != nil {
		a.AutoResume = *body.AutoResume
	}
	if body.Tags != nil {
		a.TagsJSON = marshalOrEmpty(*body.Tags)
	}
	if body.InflightPolicyOverride != nil {
		a.InflightPolicyOverride = *body.InflightPolicyOverride
	}
	if body.PriceOverrides != nil {
		raw, err := jsonObjectString(body.PriceOverrides, "price_overrides")
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		a.PriceOverridesJSON = raw
	}

	if _, err := store.UpsertAccount(r.Context(), a); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if status != a.Status {
		if err := store.SetAccountStatus(r.Context(), id, status); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	a.Status = status
	// The organization memberships are written after the account row, so a rejected node id
	// (404) cannot leave the account's own fields half-updated.
	orgNodeIDs := []int64(nil)
	if body.OrgNodeIDs != nil {
		orgNodeIDs = *body.OrgNodeIDs
	}
	nodeIDs, refs, apiErr := s.applyAccountOrgNodes(r, id, orgNodeIDs)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	s.audit(r.Context(), actor.Username, "update", "account", strconv.FormatInt(id, 10),
		map[string]any{"status": status, "billing_mode": string(a.BillingMode), "tags_set": body.Tags != nil,
			"org_node_ids": nodeIDs, "org_set": body.OrgNodeIDs != nil}, "ok")
	s.reload(r.Context(), "account updated", true)
	writeJSON(w, http.StatusOK, accountJSON(a, nodeIDs, refs))
}

// ---------------------------------------------------------------------------
// canonical models
// ---------------------------------------------------------------------------

// handleAdminGetAccountDSH reports the account's dsh gateway opt-in state (M52). The flag
// lives on the account row; this read is what the console badge and the toggle button render.
func (s *Server) handleAdminGetAccountDSH(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	a, err := store.GetAccount(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": a.ID, "name": a.Name, "enabled": a.DSHEnabled, "status": a.Status,
		"updated_at": a.UpdatedAt.UTC().Format(time.RFC3339),
	})
}

// dshTenantNameRE mirrors dshgw's config.ValidTenantName so a console-provisioned tenant
// name is always accepted by the daemon without a second guess.
var dshTenantNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,25}[a-z]$|^[a-z]$`)

// dshTenantSlug derives a tenant name candidate from the account name: ASCII letters and
// digits survive, everything else collapses to a dash. Names for accounts with no ASCII
// characters at all fall back to the "dsh-tenant" stem and are made unique by the caller.
func dshTenantSlug(accountName string) string {
	var b strings.Builder
	b.WriteString("dsh-")
	lastDash := true
	for _, r := range strings.ToLower(accountName) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 26 {
		name = strings.TrimRight(name[:26], "-")
	}
	if !dshTenantNameRE.MatchString(name) {
		name = "dsh-tenant"
	}
	return name
}

func randomHex4() string {
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%04x", time.Now().UnixNano()&0xffff)
	}
	return hex.EncodeToString(buf[:])
}

// mintDshgwKey creates the dedicated model credential for a tenant's worker. The plaintext
// exists exactly once (inside this call) and is never persisted, logged or audited.
func (s *Server) mintDshgwKey(ctx context.Context, store KeyStore, accountID int64, tenant, actor string) (string, error) {
	token := ids.APIKey()
	key := &domain.APIKey{
		AccountID: accountID, Name: "dshgw-" + tenant + "-" + randomHex4(),
		KeyPrefix: secret.Prefix(token), KeyHash: secret.Hash(token),
		RecordInputMode: "inherit", Status: "active", CreatedBy: actor,
	}
	if _, err := store.UpsertAPIKey(ctx, key); err != nil {
		return "", err
	}
	return token, nil
}

// disableDshgwKeys revokes every active dshgw-issued model credential of the account, so a
// stopped worker could not be driven by a stale key either.
func (s *Server) disableDshgwKeys(ctx context.Context, store KeyStore, accountID int64, tenant string) (int, error) {
	keys, err := store.ListAPIKeys(ctx, accountID)
	if err != nil {
		return 0, err
	}
	prefix := "dshgw-" + tenant
	revoked := 0
	for _, k := range keys {
		if k.Status != "active" || !strings.HasPrefix(k.Name, prefix) {
			continue
		}
		k.Status = "disabled"
		if _, err := store.UpsertAPIKey(ctx, k); err != nil {
			return revoked, err
		}
		revoked++
	}
	return revoked, nil
}

// handleAdminSetAccountDSH is the console's 启用/停用 DSH button (M52-rev2). Enabling
// provisions the tenant through the local dshgw channel — mint a dedicated worker key,
// create (or start and re-key) the tenant, then record the mapping — so every key of the
// account, present and future, can log into that tenant without any per-key binding or
// root shell. Disabling stops the worker, revokes the worker keys and flips the flag;
// workspace and dsh data are kept for a later re-enable. A no-op write answers 200
// without provisioning again, so double-clicking cannot flood the audit log.
func (s *Server) handleAdminSetAccountDSH(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid account id"))
		return
	}
	a, err := store.GetAccount(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var body struct {
		Enabled *bool   `json:"enabled"`
		Tenant  *string `json:"tenant"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.Enabled == nil {
		writeAPIError(w, domain.ErrInvalidRequest("enabled is required"))
		return
	}
	if *body.Enabled {
		s.enableAccountDSH(w, r, actor, store, s.deps.AdminStore, a, body.Tenant)
		return
	}
	s.disableAccountDSH(w, r, actor, store, s.deps.AdminStore, a)
}

func (s *Server) enableAccountDSH(w http.ResponseWriter, r *http.Request, actor *domain.AdminUser, store AccountAdmin, keys AdminStore, a *domain.Account, requested *string) {
	if s.deps.DshgwAdmin == nil {
		writeAPIError(w, domain.ErrInternal("dshgw provisioning channel is not configured"))
		return
	}
	if a.Status != "active" {
		writeAPIError(w, domain.ErrInvalidRequest("suspended or closed accounts cannot enable dsh"))
		return
	}
	tenant := a.DshTenant
	if requested != nil && strings.TrimSpace(*requested) != "" {
		tenant = strings.TrimSpace(*requested)
	}
	if tenant == "" {
		tenant = dshTenantSlug(a.Name)
	}
	if !dshTenantNameRE.MatchString(tenant) {
		writeAPIError(w, domain.ErrInvalidRequest(`tenant name must match [a-z][a-z0-9-]{0,25}[a-z]`))
		return
	}
	// One tenant serves exactly one account: refuse names that another account already
	// claims, and names a manual dshgw tenant occupies (that would silently put this
	// account's keys into someone else's workspace).
	existing, err := s.deps.DshgwAdmin.ListTenants(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	existsInDshgw := false
	for _, info := range existing {
		if info.Name == tenant {
			existsInDshgw = true
			break
		}
	}
	if claims, err := store.ListAccounts(r.Context()); err == nil {
		for _, other := range claims {
			if other.ID != a.ID && other.DshTenant == tenant {
				writeAPIError(w, domain.ErrInvalidRequest("tenant name is already used by another account"))
				return
			}
		}
	}
	// Uniqueness among console-created tenants keeps the key names unambiguous too.
	if !existsInDshgw {
		for i := 0; i < 32; i++ {
			candidate := tenant
			if i > 0 {
				candidate = fmt.Sprintf("%s-%s", tenant, randomHex4())
			}
			taken := false
			for _, info := range existing {
				if info.Name == candidate {
					taken = true
					break
				}
			}
			if !taken {
				tenant = candidate
				break
			}
		}
	}
	key, err := s.mintDshgwKey(r.Context(), keys, a.ID, tenant, actor.Username)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if existsInDshgw {
		// Re-enable (or retry after a half-finished enable): rotate the worker credential
		// to a fresh key and bring the stopped worker back.
		if err := s.deps.DshgwAdmin.SetTenantKey(r.Context(), tenant, key); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		if err := s.deps.DshgwAdmin.StartTenant(r.Context(), tenant); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	} else if err := s.deps.DshgwAdmin.CreateTenant(r.Context(), tenant, key); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	a.DshTenant = tenant
	a.DSHEnabled = true
	if _, err := store.UpsertAccount(r.Context(), a); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "dsh_enable", "account", strconv.FormatInt(a.ID, 10),
		map[string]any{"enabled": true, "tenant": tenant}, "ok")
	// The key verifier caches the account row, so a stale cache would keep admitting
	// (or refusing) dshgw logins for up to one verifier TTL. Account writes follow
	// the same invalidate-everything rule the PATCH handler uses.
	s.reload(r.Context(), "account dsh flag updated", true)
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": a.ID, "name": a.Name, "enabled": true, "tenant": tenant,
		"changed": true,
	})
}

func (s *Server) disableAccountDSH(w http.ResponseWriter, r *http.Request, actor *domain.AdminUser, store AccountAdmin, keys AdminStore, a *domain.Account) {
	if a.DSHEnabled && s.deps.DshgwAdmin != nil && a.DshTenant != "" {
		if err := s.deps.DshgwAdmin.StopTenant(r.Context(), a.DshTenant); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	if a.DshTenant != "" {
		if _, err := s.disableDshgwKeys(r.Context(), keys, a.ID, a.DshTenant); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	changed := a.DSHEnabled
	a.DSHEnabled = false
	if _, err := store.UpsertAccount(r.Context(), a); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if changed {
		s.audit(r.Context(), actor.Username, "dsh_disable", "account", strconv.FormatInt(a.ID, 10),
			map[string]any{"enabled": false, "tenant": a.DshTenant}, "ok")
		s.reload(r.Context(), "account dsh flag updated", true)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": a.ID, "name": a.Name, "enabled": false, "tenant": a.DshTenant,
		"changed": changed,
	})
}

func (s *Server) handleAdminListModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	list, err := store.ListModels(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	routeCount := map[int64]int{}
	if routes, err := store.ListRoutes(r.Context()); err == nil {
		for _, route := range routes {
			routeCount[route.ModelID]++
		}
	}
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		payload := modelJSON(m)
		payload["route_count"] = routeCount[m.ID]
		out = append(out, payload)
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	writeList(w, window, len(out), page)
}

func (s *Server) handleAdminUpsertModel(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	patchName := r.PathValue("name")
	var body struct {
		PublicName  string          `json:"public_name"`
		DisplayName *string         `json:"display_name"`
		Aliases     json.RawMessage `json:"aliases"`
		Enabled     *bool           `json:"enabled"`
		SalePricing json.RawMessage `json:"sale_pricing"`
		Policy      json.RawMessage `json:"policy"`
		Reasoning   json.RawMessage `json:"reasoning"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	name := firstNonEmpty(patchName, strings.TrimSpace(body.PublicName))
	if name == "" {
		writeAPIError(w, domain.ErrInvalidRequest("public_name is required"))
		return
	}
	created := false
	m, err := store.GetModelByName(r.Context(), name)
	if err != nil {
		if !domain.IsNotFound(err) {
			writeAPIError(w, toAPIError(err))
			return
		}
		created = true
		m = &domain.Model{PublicName: name, Enabled: true}
	}
	if body.DisplayName != nil {
		m.DisplayName = *body.DisplayName
	}
	if body.Enabled != nil {
		m.Enabled = *body.Enabled
	} else if created {
		m.Enabled = true
	}
	if body.Aliases != nil {
		raw, err := jsonArrayString(body.Aliases, "aliases")
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		m.AliasesJSON = raw
	}
	if body.SalePricing != nil {
		raw, err := jsonObjectString(body.SalePricing, "sale_pricing")
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		// A sale price in a currency the gateway cannot convert would silently stop
		// charging, so the document is parsed and its currency checked here.
		set, err := pricing.ParseRuleSet(raw)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		if err := s.validateRuleSetCurrency(set); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		m.SalePricingJSON = normalizeCurrencyInDocument(raw, set)
	}
	if body.Policy != nil {
		raw, err := jsonObjectString(body.Policy, "policy")
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		m.PolicyJSON = raw
	}
	if body.Reasoning != nil {
		raw, err := modelReasoningString(body.Reasoning)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		m.ReasoningJSON = raw
	}
	id, err := store.UpsertModel(r.Context(), m)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	action := "update"
	status := http.StatusOK
	if created {
		action = "create"
		status = http.StatusCreated
	}
	s.audit(r.Context(), actor.Username, action, "model", strconv.FormatInt(id, 10),
		map[string]any{"public_name": name, "enabled": m.Enabled, "reasoning": jsonOrNil(m.ReasoningJSON)}, "ok")
	if err := s.reloadModel(r.Context(), "model "+action); err != nil {
		s.audit(r.Context(), actor.Username, "reload", "model", strconv.FormatInt(id, 10),
			map[string]any{"public_name": name, "reasoning": jsonOrNil(m.ReasoningJSON)}, "failed")
		writeAPIError(w, domain.ErrInternal("model configuration was saved but was not applied because registry reload failed; resolve the reload error and retry the update"))
		return
	}
	writeJSON(w, status, modelJSON(m))
}

// ---------------------------------------------------------------------------
// name-resolution mappings
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListMappings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	list, err := store.ListModelMappings(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		out = append(out, mappingJSON(m))
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	writeList(w, window, len(out), page)
}

func (s *Server) handleAdminUpsertMapping(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	var body struct {
		Kind                string `json:"kind"`
		Pattern             string `json:"pattern"`
		TargetModel         string `json:"target_model"`
		TargetProviderID    int64  `json:"target_provider_id"`
		TargetProvider      string `json:"target_provider"`
		TargetUpstreamModel string `json:"target_upstream_model"`
		Priority            *int   `json:"priority"`
		Enabled             *bool  `json:"enabled"`
		Note                string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	providerID := body.TargetProviderID
	if providerID == 0 && body.TargetProvider != "" {
		providerStore, ok := portReady(w, s.deps.Providers, "provider management")
		if !ok {
			return
		}
		p, err := providerStore.GetProviderByName(r.Context(), body.TargetProvider)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		providerID = p.ID
	}
	kinds := map[string]bool{"exact": true, "prefix": true, "glob": true, "regex": true}
	if !kinds[body.Kind] {
		writeAPIError(w, domain.ErrInvalidRequest("kind must be exact, prefix, glob or regex"))
		return
	}
	if strings.TrimSpace(body.Pattern) == "" {
		writeAPIError(w, domain.ErrInvalidRequest("pattern is required"))
		return
	}
	if body.TargetModel == "" && providerID == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("target_model or target_provider_id is required"))
		return
	}
	if err := validateMappingRule(body.Kind, body.Pattern, body.TargetModel, providerID); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	m := &domain.ModelMapping{
		Kind: body.Kind, Pattern: body.Pattern, TargetModel: body.TargetModel,
		TargetProviderID: providerID, TargetUpstreamModel: body.TargetUpstreamModel,
		Priority: 100, Enabled: true, Note: body.Note,
	}
	if body.Priority != nil {
		m.Priority = *body.Priority
	}
	if body.Enabled != nil {
		m.Enabled = *body.Enabled
	}
	if err := validateNonNegative("priority", body.Priority); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	id, err := store.UpsertModelMapping(r.Context(), m)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "model_mapping", strconv.FormatInt(id, 10),
		map[string]any{"kind": m.Kind, "pattern": m.Pattern, "target_model": m.TargetModel}, "ok")
	s.reload(r.Context(), "model mapping upserted", true)
	writeJSON(w, http.StatusOK, mappingJSON(m))
}

func (s *Server) handleAdminDeleteMapping(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid mapping id"))
		return
	}
	if err := store.DeleteModelMapping(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "delete", "model_mapping", strconv.FormatInt(id, 10), nil, "ok")
	s.reload(r.Context(), "model mapping deleted", true)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// ---------------------------------------------------------------------------
// routes
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListRoutes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	list, err := store.ListRoutes(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	models, providers := s.nameLookups(r.Context())
	out := make([]map[string]any, 0, len(list))
	for _, route := range list {
		out = append(out, routeJSON(route, models[route.ModelID], providers[route.ProviderID]))
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	writeList(w, window, len(out), page)
}

// nameLookups resolves ids to names for presentation only; failures degrade to
// empty names instead of failing the listing.
func (s *Server) nameLookups(ctx context.Context) (map[int64]string, map[int64]string) {
	models := map[int64]string{}
	providers := map[int64]string{}
	if store, ok := portReadyNoWrite(s.deps.Models); ok {
		if list, err := store.ListModels(ctx); err == nil {
			for _, m := range list {
				models[m.ID] = m.PublicName
			}
		}
	}
	if store, ok := portReadyNoWrite(s.deps.Providers); ok {
		if list, err := store.ListProviders(ctx); err == nil {
			for _, p := range list {
				providers[p.ID] = p.Name
			}
		}
	}
	return models, providers
}

func (s *Server) handleAdminUpsertRoute(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	modelStore := store
	providerStore, ok := portReady(w, s.deps.Providers, "provider management")
	if !ok {
		return
	}
	var body struct {
		ModelID       int64           `json:"model_id"`
		Model         string          `json:"model"`
		ProviderID    int64           `json:"provider_id"`
		Provider      string          `json:"provider"`
		UpstreamModel string          `json:"upstream_model"`
		Priority      *int            `json:"priority"`
		Weight        *int            `json:"weight"`
		Enabled       *bool           `json:"enabled"`
		Policy        json.RawMessage `json:"policy"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	modelID := body.ModelID
	if modelID == 0 && body.Model != "" {
		m, err := modelStore.GetModelByName(r.Context(), body.Model)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		modelID = m.ID
	}
	providerID := body.ProviderID
	if providerID == 0 && body.Provider != "" {
		p, err := providerStore.GetProviderByName(r.Context(), body.Provider)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		providerID = p.ID
	}
	if modelID == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("model or model_id is required"))
		return
	}
	if providerID == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("provider or provider_id is required"))
		return
	}
	if _, err := providerStore.GetProvider(r.Context(), providerID); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	route := &domain.Route{
		ModelID: modelID, ProviderID: providerID,
		UpstreamModel: strings.TrimSpace(body.UpstreamModel),
		Priority:      100, Weight: 100, Enabled: true,
	}
	if route.UpstreamModel == "" {
		writeAPIError(w, domain.ErrInvalidRequest("upstream_model is required"))
		return
	}
	if body.Priority != nil {
		route.Priority = *body.Priority
	}
	if body.Weight != nil {
		route.Weight = *body.Weight
	}
	if body.Enabled != nil {
		route.Enabled = *body.Enabled
	}
	if body.Policy != nil {
		raw, err := jsonObjectString(body.Policy, "policy")
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		route.PolicyJSON = raw
	}
	if err := validateNonNegative("priority", body.Priority); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := validateNonNegative("weight", body.Weight); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	id, err := store.UpsertRoute(r.Context(), route)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "route", strconv.FormatInt(id, 10),
		map[string]any{"model_id": modelID, "provider_id": providerID, "upstream_model": route.UpstreamModel}, "ok")
	s.reload(r.Context(), "route upserted", true)
	models, providers := s.nameLookups(r.Context())
	writeJSON(w, http.StatusOK, routeJSON(route, models[route.ModelID], providers[route.ProviderID]))
}

func (s *Server) handleAdminPatchRoute(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid route id"))
		return
	}
	routes, err := store.ListRoutes(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var route *domain.Route
	for _, candidate := range routes {
		if candidate.ID == id {
			route = candidate
			break
		}
	}
	if route == nil {
		writeAPIError(w, domain.ErrNotFound(fmt.Sprintf("route %d", id)))
		return
	}
	var body struct {
		UpstreamModel *string         `json:"upstream_model"`
		Priority      *int            `json:"priority"`
		Weight        *int            `json:"weight"`
		Enabled       *bool           `json:"enabled"`
		Policy        json.RawMessage `json:"policy"`
		ResetCooldown bool            `json:"reset_cooldown"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.UpstreamModel != nil {
		if strings.TrimSpace(*body.UpstreamModel) == "" {
			writeAPIError(w, domain.ErrInvalidRequest("upstream_model must not be empty"))
			return
		}
		route.UpstreamModel = strings.TrimSpace(*body.UpstreamModel)
	}
	if body.Priority != nil {
		route.Priority = *body.Priority
	}
	if body.Weight != nil {
		route.Weight = *body.Weight
	}
	if body.Enabled != nil {
		route.Enabled = *body.Enabled
	}
	if body.Policy != nil {
		raw, err := jsonObjectString(body.Policy, "policy")
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		route.PolicyJSON = raw
	}
	if body.ResetCooldown {
		route.CooldownUntil = nil
	}
	if err := validateNonNegative("priority", body.Priority); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := validateNonNegative("weight", body.Weight); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if _, err := store.UpsertRoute(r.Context(), route); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if body.ResetCooldown {
		if s.deps.Router != nil {
			s.deps.Router.Balancer().ClearCooldown(fmt.Sprintf("route:%d", id))
		}
	}
	s.audit(r.Context(), actor.Username, "update", "route", strconv.FormatInt(id, 10),
		map[string]any{"enabled": route.Enabled, "weight": route.Weight, "priority": route.Priority}, "ok")
	s.reload(r.Context(), "route updated", true)
	models, providers := s.nameLookups(r.Context())
	writeJSON(w, http.StatusOK, routeJSON(route, models[route.ModelID], providers[route.ProviderID]))
}

func (s *Server) handleAdminDeleteRoute(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Models, "model management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid route id"))
		return
	}
	if err := store.DeleteRoute(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "delete", "route", strconv.FormatInt(id, 10), nil, "ok")
	s.reload(r.Context(), "route deleted", true)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// ---------------------------------------------------------------------------
// tags
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListTags(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Tags, "tag management")
	if !ok {
		return
	}
	list, err := store.ListTags(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, tagJSON(t))
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	writeList(w, window, len(out), page)
}

func (s *Server) handleAdminUpsertTag(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Tags, "tag management")
	if !ok {
		return
	}
	var body struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Grants      json.RawMessage `json:"grants"`
		Policy      json.RawMessage `json:"policy"`
		Priority    *int            `json:"priority"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	// A tag name is a human-facing label like an account name: any valid Unicode, trimmed,
	// at most 64 characters. See domain.NormalizeTagName for why the ASCII rule had to go.
	name, err := domain.NormalizeTagName(body.Name)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	grants, err := jsonObjectString(body.Grants, "grants")
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	policy, apiErr := keyPolicyDocument(body.Policy)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	tag := &domain.Tag{Name: name, Description: body.Description, GrantsJSON: grants, PolicyJSON: policy, Priority: 100}
	if body.Priority != nil {
		tag.Priority = *body.Priority
	}
	if err := validateNonNegative("priority", body.Priority); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	id, err := store.UpsertTag(r.Context(), tag)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "tag", strconv.FormatInt(id, 10),
		map[string]any{"name": name}, "ok")
	s.reload(r.Context(), "tag upserted", true)
	writeJSON(w, http.StatusOK, tagJSON(tag))
}

// handleAdminPatchTag updates one tag addressed by id. It exists next to the name-keyed
// upsert because the id is the identity: an upsert cannot touch a row whose name the
// writer would refuse (that is exactly how 蓝精灵1/2/3 became uneditable), and it cannot
// tell a rename from a create.
//
// Only the fields present in the body change. `grants: null` / `policy: null` clear the
// field, which is why the two are read as presence-aware raw JSON rather than structs.
func (s *Server) handleAdminPatchTag(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Tags, "tag management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid tag id"))
		return
	}
	var body struct {
		Name        *string         `json:"name"`
		Description *string         `json:"description"`
		Grants      json.RawMessage `json:"grants"`
		Policy      json.RawMessage `json:"policy"`
		Priority    *int            `json:"priority"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	tag, err := store.GetTagByID(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if body.Name != nil {
		name, err := domain.NormalizeTagName(*body.Name)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		// Renaming is refused rather than performed: a tag is referenced *by name* from
		// accounts.tags_json and api_keys.tags_json, so a rename would silently drop every
		// binding instead of moving it. Accepting the current name keeps the console form
		// (which echoes the name field) idempotent.
		if name != tag.Name {
			writeAPIError(w, domain.ErrInvalidRequest(
				"a tag cannot be renamed: it is attached to accounts and API keys by name, so renaming would drop every binding; "+
					"create a new tag with the wanted name, move the bindings, then delete this one").WithParam("name"))
			return
		}
		tag.Name = name
	}
	if body.Description != nil {
		tag.Description = *body.Description
	}
	if body.Grants != nil {
		grants, err := jsonObjectString(body.Grants, "grants")
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		tag.GrantsJSON = grants
	}
	if body.Policy != nil {
		policy, apiErr := keyPolicyDocument(body.Policy)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		tag.PolicyJSON = policy
	}
	if body.Priority != nil {
		if err := validateNonNegative("priority", body.Priority); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		tag.Priority = *body.Priority
	}
	if err := store.UpdateTag(r.Context(), tag); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "tag", strconv.FormatInt(id, 10), map[string]any{
		"name": tag.Name, "grants_set": body.Grants != nil, "policy_set": body.Policy != nil,
		"priority_set": body.Priority != nil, "description_set": body.Description != nil,
	}, "ok")
	s.reload(r.Context(), "tag updated", true)
	writeJSON(w, http.StatusOK, tagJSON(tag))
}

func (s *Server) handleAdminDeleteTag(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Tags, "tag management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid tag id"))
		return
	}
	if err := store.DeleteTag(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "delete", "tag", strconv.FormatInt(id, 10), nil, "ok")
	s.reload(r.Context(), "tag deleted", true)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// ---------------------------------------------------------------------------
// mcp tokens
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListMCPTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.MCPTokenStore, "MCP token management")
	if !ok {
		return
	}
	accountID := int64(0)
	if raw := r.URL.Query().Get("account_id"); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
			accountID = parsed
		}
	}
	list, err := store.ListMCPTokens(r.Context(), accountID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, t := range list {
		out = append(out, mcpTokenJSON(t))
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	writeList(w, window, len(out), page)
}

func (s *Server) handleAdminCreateMCPToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.MCPTokenStore, "MCP token management")
	if !ok {
		return
	}
	accounts, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return
	}
	var body struct {
		Name      string `json:"name"`
		AccountID int64  `json:"account_id"`
		Account   string `json:"account"`
		Note      string `json:"note"`
		ExpiresAt string `json:"expires_at"`
		Scope     string `json:"scope"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeAPIError(w, domain.ErrInvalidRequest("name is required"))
		return
	}
	accountID := body.AccountID
	if accountID == 0 && body.Account != "" {
		account, err := accounts.GetAccountByName(r.Context(), body.Account)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		accountID = account.ID
	}
	if accountID == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("account or account_id is required"))
		return
	}
	if _, err := accounts.GetAccount(r.Context(), accountID); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var expiresAt *time.Time
	if body.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest("expires_at must be RFC3339"))
			return
		}
		if !parsed.After(time.Now().UTC()) {
			writeAPIError(w, domain.ErrInvalidRequest("expires_at must be in the future"))
			return
		}
		expiresAt = &parsed
	}

	// The scope is the whole security decision for an MCP token: the default is the
	// account-scoped read-only query surface, and anything wider has to be asked for.
	scope := strings.TrimSpace(body.Scope)
	if scope == "" {
		scope = mcpsrv.ScopeQuery
	}
	if !mcpsrv.ValidScope(scope) {
		writeAPIError(w, domain.ErrInvalidRequest("scope must be query, admin_read or admin"))
		return
	}

	token := ids.MCPToken()
	record := &domain.MCPToken{
		AccountID: accountID, Name: strings.TrimSpace(body.Name),
		TokenHash: secret.Hash(token), TokenPrefix: secret.Prefix(token),
		Scope: scope, Status: "active", CreatedBy: actor.Username, Note: body.Note, ExpiresAt: expiresAt,
	}
	id, err := store.UpsertMCPToken(r.Context(), record)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "create", "mcp_token", strconv.FormatInt(id, 10),
		map[string]any{"name": record.Name, "account_id": accountID, "scope": record.Scope}, "ok")
	payload := mcpTokenJSON(record)
	payload["token"] = token
	payload["note"] = "store this token now: it cannot be retrieved again; API keys are rejected on /mcp"
	if scope != mcpsrv.ScopeQuery {
		payload["scope_note"] = "this token may call the management API through MCP; treat it like an administrator credential"
	}
	writeJSON(w, http.StatusCreated, payload)
}

func (s *Server) handleAdminRevokeMCPToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.MCPTokenStore, "MCP token management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid token id"))
		return
	}
	if err := store.RevokeMCPToken(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "revoke", "mcp_token", strconv.FormatInt(id, 10), nil, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": "revoked"})
}

// handleAdminPatchMCPToken changes a token's scope or status. Downgrading a
// powerful token must not require re-issuing it: the old plaintext is gone, so an
// admin could otherwise only revoke and start over.
func (s *Server) handleAdminPatchMCPToken(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.MCPTokenStore, "MCP token management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid token id"))
		return
	}
	var body struct {
		Scope  *string `json:"scope"`
		Status *string `json:"status"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.Scope == nil && body.Status == nil {
		writeAPIError(w, domain.ErrInvalidRequest("scope or status is required"))
		return
	}
	if body.Scope != nil && !mcpsrv.ValidScope(*body.Scope) {
		writeAPIError(w, domain.ErrInvalidRequest("scope must be query, admin_read or admin"))
		return
	}
	if body.Status != nil && *body.Status != "active" && *body.Status != "revoked" {
		writeAPIError(w, domain.ErrInvalidRequest("status must be active or revoked"))
		return
	}

	tokens, err := store.ListMCPTokens(r.Context(), 0)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	var target *domain.MCPToken
	for _, token := range tokens {
		if token.ID == id {
			target = token
			break
		}
	}
	if target == nil {
		writeAPIError(w, domain.ErrNotFound(fmt.Sprintf("mcp token %d", id)))
		return
	}
	previousScope := mcpsrv.NormalizeScope(target.Scope)
	if body.Scope != nil {
		target.Scope = *body.Scope
	}
	if body.Status != nil {
		target.Status = *body.Status
	}
	if _, err := store.UpsertMCPToken(r.Context(), target); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "mcp_token", strconv.FormatInt(id, 10),
		map[string]any{"scope": mcpsrv.NormalizeScope(target.Scope), "previous_scope": previousScope, "status": target.Status}, "ok")
	writeJSON(w, http.StatusOK, mcpTokenJSON(target))
}

// ---------------------------------------------------------------------------
// hooks
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListHooks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.HookStore, "hook management")
	if !ok {
		return
	}
	list, err := store.ListHooks(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, h := range list {
		out = append(out, hookJSON(h))
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	window := sliceWindow(out, page)
	writeList(w, window, len(out), page)
}

func (s *Server) handleAdminUpsertHook(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.HookStore, "hook management")
	if !ok {
		return
	}
	var body struct {
		Name           string          `json:"name"`
		Type           string          `json:"type"`
		URL            string          `json:"url"`
		Secret         *string         `json:"secret"`
		Events         json.RawMessage `json:"events"`
		IncludeContent *bool           `json:"include_content"`
		MaxBytes       *int            `json:"max_bytes"`
		SampleRate     *float64        `json:"sample_rate"`
		Enabled        *bool           `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeAPIError(w, domain.ErrInvalidRequest("name is required"))
		return
	}
	hookType := body.Type
	if hookType == "" {
		hookType = "webhook"
	}
	if !validHookTypes[hookType] {
		writeAPIError(w, domain.ErrInvalidRequest("type must be webhook or jsonl"))
		return
	}
	target := strings.TrimSpace(body.URL)
	if target == "" {
		writeAPIError(w, domain.ErrInvalidRequest("url is required (webhook endpoint or jsonl file path)"))
		return
	}
	if hookType == "webhook" && !strings.HasPrefix(target, "https://") {
		insecure := s.deps.Config != nil && s.deps.Config.Hooks.AllowInsecure
		if !strings.HasPrefix(target, "http://") || !insecure {
			writeAPIError(w, domain.ErrInvalidRequest("webhook url must use https (set hooks.allow_insecure to permit http)"))
			return
		}
	}
	events, err := jsonArrayString(body.Events, "events")
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	h := &domain.Hook{Name: name, Type: hookType, URL: target, EventsJSON: events, Enabled: true, SampleRate: 1}
	if body.Secret != nil {
		h.Secret = *body.Secret
	}
	if body.IncludeContent != nil {
		h.IncludeContent = *body.IncludeContent
	}
	if body.MaxBytes != nil {
		if *body.MaxBytes < 0 {
			writeAPIError(w, domain.ErrInvalidRequest("max_bytes must not be negative"))
			return
		}
		h.MaxBytes = *body.MaxBytes
	}
	if body.SampleRate != nil {
		if *body.SampleRate <= 0 || *body.SampleRate > 1 {
			writeAPIError(w, domain.ErrInvalidRequest("sample_rate must be in (0,1]"))
			return
		}
		h.SampleRate = *body.SampleRate
	}
	if body.Enabled != nil {
		h.Enabled = *body.Enabled
	}

	// UpsertHook matches by name, so inherit the existing secret when omitted.
	if body.Secret == nil {
		if existing, err := store.ListHooks(r.Context()); err == nil {
			for _, candidate := range existing {
				if candidate.Name == name {
					h.Secret = candidate.Secret
					h.ID = candidate.ID
					break
				}
			}
		}
	}
	id, err := store.UpsertHook(r.Context(), h)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "hook", strconv.FormatInt(id, 10),
		map[string]any{"name": name, "type": hookType, "events": events, "secret_set": h.Secret != ""}, "ok")
	s.reloadHooks(r.Context(), actor.Username)
	writeJSON(w, http.StatusOK, hookJSON(h))
}

func (s *Server) handleAdminDeleteHook(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.HookStore, "hook management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid hook id"))
		return
	}
	if err := store.DeleteHook(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "delete", "hook", strconv.FormatInt(id, 10), nil, "ok")
	s.reloadHooks(r.Context(), actor.Username)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// reloadHooks swaps the in-memory hook set after a write. A failure is logged and
// surfaced as a warning field rather than failing the write that already landed.
func (s *Server) reloadHooks(ctx context.Context, actor string) {
	if s.deps.ReloadHooks == nil {
		return
	}
	if err := s.deps.ReloadHooks(ctx); err != nil {
		s.deps.Log.Error("reloading hooks after an admin write failed", "err", err, "actor", actor)
	}
}

// ---------------------------------------------------------------------------
// settings
// ---------------------------------------------------------------------------

func (s *Server) handleAdminGetSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Settings, "settings management")
	if !ok {
		return
	}
	keys := r.URL.Query()["key"]
	if len(keys) == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("at least one key query parameter is required"))
		return
	}
	data := map[string]any{}
	sort.Strings(keys)
	for _, key := range keys {
		value, found, err := store.GetSetting(r.Context(), key)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		if !found {
			data[key] = nil
			continue
		}
		data[key] = jsonOrNil(value)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (s *Server) handleAdminPutSetting(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Settings, "settings management")
	if !ok {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeAPIError(w, domain.ErrInvalidRequest("setting key is required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("failed to read the request body"))
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		writeAPIError(w, domain.ErrInvalidRequest("setting value must be valid JSON"))
		return
	}
	// Accept both a bare value and {"value": ...} so curl and the UI can share one route.
	stored := trimmed
	var wrapper struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil && len(wrapper.Value) > 0 {
		stored = string(wrapper.Value)
	}
	// The FX table is the one setting with a shape the gateway depends on: a bad
	// rate would turn into a wrong charge, so it is validated before it is stored
	// and the live table is rebuilt right after.
	if key == pricing.SettingFXRates {
		rates, err := pricing.ParseFXRates(stored)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		merged := pricing.MergeFXRates(s.configuredFXRates(), rates)
		if _, err := pricing.NewFXTable(s.ledgerCurrency(), merged); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	if err := store.SetSetting(r.Context(), key, stored); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if key == pricing.SettingFXRates {
		s.reloadFX(r.Context(), actor.Username)
	}
	s.audit(r.Context(), actor.Username, "update", "setting", key, map[string]any{"bytes": len(stored)}, "ok")
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": json.RawMessage(stored)})
}
