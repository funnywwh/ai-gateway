package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/orgtree"
)

// This file is the management surface of the organization structure: the node tree and the
// accounts that sit in it.
//
// Two shapes decide most of what follows.
//
// First, the tree is stored flat and returned flat. Every response carries parent_id, depth and
// a rendered path, so a console (or an MCP client) can draw the hierarchy without a second
// request or an assumption about ordering — and without re-deriving ancestry itself.
//
// Second, membership is many-to-many, so it has two write directions: PUT a node's members,
// or PATCH an account's nodes. Both replace the whole set, which makes them idempotent and
// replayable; a rejected write (an unknown id) leaves the stored set untouched.
//
// Every write goes through s.reload(ctx, reason, true) because a node's tags are inherited by
// every API key under it: the verifier's positive cache holds API keys for up to 30 seconds,
// so anything less than a full invalidation would leave a window in which a permission change
// has not taken effect.

// orgMembersLimit caps how many accounts one node reports inline. The console pages accounts
// separately; this list is for "who is in this department" at a glance.
const orgMembersLimit = 500

// ---------------------------------------------------------------------------
// nodes
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListOrgNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Org, "organization management")
	if !ok {
		return
	}
	nodes, err := store.ListOrgNodes(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	includeAccounts := boolQuery(r, "include_accounts")

	// Membership is always read, because account_count is part of every row: the console shows
	// "N 个账号" beside each node and a count that silently read zero unless the caller also
	// asked for the inline member list would be a lie. It is one cheap grouped query; only
	// resolving the member *names* is deferred to include_accounts.
	accountsByID := map[int64]*domain.Account{}
	membersByNode := map[int64][]int64{}
	memberships, err := store.ListOrgMemberships(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	for _, m := range memberships {
		membersByNode[m.NodeID] = append(membersByNode[m.NodeID], m.AccountID)
	}
	if includeAccounts {
		accounts, err := s.adminAccountIndex(r)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		accountsByID = accounts
	}

	index := orgtree.NewIndex(nodes)
	out := make([]map[string]any, 0, len(nodes))
	for _, node := range index.Ordered() {
		entry := orgNodeJSON(node, index, len(membersByNode[node.ID]))
		if includeAccounts {
			members := membersByNode[node.ID]
			truncated := false
			if len(members) > orgMembersLimit {
				members = members[:orgMembersLimit]
				truncated = true
			}
			list := make([]map[string]any, 0, len(members))
			for _, accountID := range members {
				account := accountsByID[accountID]
				if account == nil {
					continue
				}
				list = append(list, map[string]any{"id": account.ID, "name": account.Name})
			}
			entry["accounts"] = list
			entry["accounts_truncated"] = truncated
		}
		out = append(out, entry)
	}

	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeList(w, sliceWindow(out, page), len(out), page)
}

func (s *Server) handleAdminCreateOrgNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Org, "organization management")
	if !ok {
		return
	}
	var body struct {
		Name      string   `json:"name"`
		ParentID  *int64   `json:"parent_id"`
		Note      string   `json:"note"`
		Tags      []string `json:"tags"`
		SortOrder *int     `json:"sort_order"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	name, err := domain.NormalizeOrgNodeName(body.Name)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	tags, apiErr := s.knownOrgTags(r, body.Tags)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	if err := validateNonNegative("sort_order", body.SortOrder); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	node := &domain.OrgNode{Name: name, Note: body.Note, SortOrder: 100}
	if body.ParentID != nil {
		parent := *body.ParentID
		node.ParentID = &parent
		if apiErr := s.validateOrgPlacement(r, parent, 0); apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
	}
	if body.SortOrder != nil {
		node.SortOrder = *body.SortOrder
	}
	node.TagsJSON = marshalOrEmpty(tags)

	id, err := store.CreateOrgNode(r.Context(), node)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "create", "org_node", strconv.FormatInt(id, 10),
		map[string]any{"name": name, "parent_id": node.ParentIDValue(), "tags": tags}, "ok")
	s.reload(r.Context(), "org node created", true)

	index, apiErr := s.orgIndex(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	writeJSON(w, http.StatusCreated, orgNodeJSON(node, index, 0))
}

func (s *Server) handleAdminPatchOrgNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Org, "organization management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid org node id"))
		return
	}
	var body struct {
		Name      *string   `json:"name"`
		ParentID  *int64    `json:"parent_id"`
		Note      *string   `json:"note"`
		Tags      *[]string `json:"tags"`
		SortOrder *int      `json:"sort_order"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	node, err := store.GetOrgNode(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if body.Name != nil {
		name, err := domain.NormalizeOrgNodeName(*body.Name)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		node.Name = name
	}
	if body.Note != nil {
		node.Note = *body.Note
	}
	if body.Tags != nil {
		tags, apiErr := s.knownOrgTags(r, *body.Tags)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		node.TagsJSON = marshalOrEmpty(tags)
	}
	if err := validateNonNegative("sort_order", body.SortOrder); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if body.SortOrder != nil {
		node.SortOrder = *body.SortOrder
	}
	moved := false
	if body.ParentID != nil {
		parent := *body.ParentID
		if parent == 0 {
			node.ParentID = nil
		} else {
			node.ParentID = &parent
		}
		moved = true
	}
	if moved || body.ParentID != nil {
		if apiErr := s.validateOrgPlacement(r, node.ParentIDValue(), id); apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
	}
	if err := store.UpdateOrgNode(r.Context(), node); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "update", "org_node", strconv.FormatInt(id, 10), map[string]any{
		"name": node.Name, "parent_id": node.ParentIDValue(),
		"tags_set": body.Tags != nil, "moved": moved, "note_set": body.Note != nil,
		"sort_order_set": body.SortOrder != nil,
	}, "ok")
	s.reload(r.Context(), "org node updated", true)

	index, apiErr := s.orgIndex(r)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, orgNodeJSON(node, index, 0))
}

func (s *Server) handleAdminDeleteOrgNode(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Org, "organization management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid org node id"))
		return
	}
	node, err := store.GetOrgNode(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	deleted, err := store.DeleteOrgNode(r.Context(), id, boolQuery(r, "cascade"))
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "delete", "org_node", strconv.FormatInt(id, 10),
		map[string]any{"name": node.Name, "nodes_deleted": deleted}, "ok")
	s.reload(r.Context(), "org node deleted", true)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true, "nodes_deleted": deleted})
}

// ---------------------------------------------------------------------------
// membership
// ---------------------------------------------------------------------------

func (s *Server) handleAdminListOrgNodeAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.adminActor(w, r, false); !ok {
		return
	}
	store, ok := portReady(w, s.deps.Org, "organization management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid org node id"))
		return
	}
	if _, err := store.GetOrgNode(r.Context(), id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	accountIDs, err := store.ListOrgNodeAccountIDs(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	accounts, err := s.adminAccountIndex(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	out := make([]map[string]any, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		entry := map[string]any{"id": accountID}
		if account := accounts[accountID]; account != nil {
			entry["name"] = account.Name
			entry["status"] = account.Status
			// The organization page renders each account as a person row carrying its DSH state,
			// its Feishu identity and how many keys it has (M72): all three decide what an
			// operator does with that row, and reading them here is one page instead of a
			// request per row.
			entry["dsh_enabled"] = account.DSHEnabled
			entry["dsh_tenant"] = account.DshTenant
			// The person row is where DSH is enabled from (M72), so the dialog's pre-filled tenant
			// name comes with it (M74) — same field, same value as the account list's.
			entry["dsh_tenant_suggested"] = dshTenantNameForAccount(account)
			entry["dsh_disabled_at"] = timeOrNil(account.DshDisabledAt)
			entry["dsh_effective"] = accountDSHEffective(s.deps.Config != nil && s.deps.Config.Dshgw.AutoEnable, account)
			entry["feishu"] = accountFeishuJSON(account)
			if s.deps.KeyStore != nil {
				if keys, err := s.deps.KeyStore.ListAPIKeys(r.Context(), accountID); err == nil {
					total := 0
					for _, key := range keys {
						if key != nil {
							total++
						}
					}
					entry["key_count"] = total
					entry["active_key_count"] = len(s.accountKeyChoices(r.Context(), accountID))
				}
			}
		}
		out = append(out, entry)
	}
	page, err := pageConfig.params(r)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	writeList(w, sliceWindow(out, page), len(out), page)
}

func (s *Server) handleAdminSetOrgNodeAccounts(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return
	}
	store, ok := portReady(w, s.deps.Org, "organization management")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid org node id"))
		return
	}
	var body struct {
		AccountIDs *[]int64 `json:"account_ids"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.AccountIDs == nil {
		writeAPIError(w, domain.ErrInvalidRequest("account_ids is required (pass an empty array to remove every account)").WithParam("account_ids"))
		return
	}
	node, err := store.GetOrgNode(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := store.SetOrgNodeMembers(r.Context(), id, *body.AccountIDs); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	after, err := store.ListOrgNodeAccountIDs(r.Context(), id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(r.Context(), actor.Username, "assign", "org_node", strconv.FormatInt(id, 10),
		map[string]any{"name": node.Name, "members_after": len(after)}, "ok")
	s.reload(r.Context(), "org node members updated", true)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "account_ids": after, "count": len(after),
		"account_count": len(after), "updated": true,
	})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// orgIndex loads the current tree as an index. It is used to render paths and depths in a
// write response, where the caller wants the same shape a list returns.
func (s *Server) orgIndex(r *http.Request) (*orgtree.Index, *domain.APIError) {
	store, ok := portReadyNoWrite(s.deps.Org)
	if !ok {
		return orgtree.NewIndex(nil), nil
	}
	nodes, err := store.ListOrgNodes(r.Context())
	if err != nil {
		return nil, toAPIError(err)
	}
	return orgtree.NewIndex(nodes), nil
}

// adminAccountIndex loads accounts by id for the endpoints that render members inline.
func (s *Server) adminAccountIndex(r *http.Request) (map[int64]*domain.Account, error) {
	store, ok := portReadyNoWrite(s.deps.Accounts)
	if !ok {
		return map[int64]*domain.Account{}, nil
	}
	list, err := store.ListAccounts(r.Context())
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*domain.Account, len(list))
	for _, account := range list {
		out[account.ID] = account
	}
	return out, nil
}

// validateOrgPlacement checks a node placement against the tree: the parent must exist, the
// move must not detach a subtree from every root (a cycle), and the result must stay within
// the depth limit.
//
// moveID is 0 when a node is being created (there is no subtree to move yet).
func (s *Server) validateOrgPlacement(r *http.Request, parentID, moveID int64) *domain.APIError {
	if parentID == 0 {
		return nil
	}
	store, ok := portReadyNoWrite(s.deps.Org)
	if !ok {
		return nil
	}
	nodes, err := store.ListOrgNodes(r.Context())
	if err != nil {
		return toAPIError(err)
	}
	index := orgtree.NewIndex(nodes)
	parent := index.Node(parentID)
	if parent == nil {
		return domain.ErrNotFound("org node " + strconv.FormatInt(parentID, 10))
	}
	if moveID != 0 {
		if moveID == parentID || index.WouldCreateCycle(moveID, parentID) {
			return domain.ErrInvalidRequest(
				"an org node cannot be moved under itself or one of its own descendants: " +
					"that would detach the subtree from every root").WithParam("parent_id")
		}
	}
	depth := index.Depth(parentID) + 1 + index.SubtreeHeight(moveID)
	if depth > orgtree.MaxDepth {
		return domain.ErrInvalidRequest(
			"this placement would put a node at depth " + strconv.Itoa(depth) +
				", above the limit of " + strconv.Itoa(orgtree.MaxDepth)).WithParam("parent_id")
	}
	return nil
}

// knownOrgTags validates the tag names attached to a node.
//
// A node's tags are inherited by every account in its subtree, so a name that does not exist
// is refused rather than stored and silently dropped — the same reasoning the API key import
// path uses. The asymmetry with PATCH /accounts (which still accepts unknown names) is
// deliberate: that behaviour predates this check and changing it would break existing
// importers, while a node is a new write path that can afford to be strict.
func (s *Server) knownOrgTags(r *http.Request, names []string) ([]string, *domain.APIError) {
	tags, ok := portReadyNoWrite(s.deps.Tags)
	if !ok {
		return nil, nil
	}
	if len(names) == 0 {
		return nil, nil
	}
	list, err := tags.ListTags(r.Context())
	if err != nil {
		return nil, toAPIError(err)
	}
	if apiErr := unknownTagName(names, list); apiErr != nil {
		return nil, apiErr
	}
	cleaned := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			cleaned = append(cleaned, name)
		}
	}
	return cleaned, nil
}

// orgNodeJSON renders one node. index may be nil when ancestry is not needed.
func orgNodeJSON(node *domain.OrgNode, index *orgtree.Index, accountCount int) map[string]any {
	parentID := any(nil)
	if node.ParentID != nil {
		parentID = *node.ParentID
	}
	depth := 0
	path := node.Name
	if index != nil {
		if d := index.Depth(node.ID); d >= 0 {
			depth = d
		}
		if rendered := renderOrgPath(node.ID, index); rendered != "" {
			path = rendered
		}
	}
	return map[string]any{
		"id": node.ID, "parent_id": parentID, "name": node.Name, "note": node.Note,
		"tags":       orgNodeTagNames(node),
		"sort_order": node.SortOrder, "depth": depth, "path": path,
		"account_count": accountCount,
		"created_at":    node.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":    node.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// renderOrgPath builds the "根/…/自身" label path.
func renderOrgPath(id int64, index *orgtree.Index) string {
	chain := index.Chain(id)
	if len(chain) == 0 {
		return ""
	}
	parts := make([]string, 0, len(chain))
	for _, nodeID := range chain {
		if node := index.Node(nodeID); node != nil {
			parts = append(parts, node.Name)
		}
	}
	return strings.Join(parts, "/")
}

// orgNodeTagNames decodes a node's tag names for display, tolerating a malformed value the
// same way the resolver does.
func orgNodeTagNames(node *domain.OrgNode) []string {
	names := orgtree.NodeTagNames(node)
	if names == nil {
		return []string{}
	}
	return names
}

// orgRefsForAccounts renders the organizations an account belongs to, for the account JSON.
// The result is sorted so two reads of the same state produce the same document.
func orgRefsForAccounts(index *orgtree.Index, nodeIDs []int64) ([]int64, []map[string]any) {
	if len(nodeIDs) == 0 || index == nil {
		return []int64{}, []map[string]any{}
	}
	ids := make([]int64, 0, len(nodeIDs))
	refs := make([]map[string]any, 0, len(nodeIDs))
	seen := map[int64]struct{}{}
	for _, nodeID := range nodeIDs {
		if _, repeat := seen[nodeID]; repeat {
			continue
		}
		node := index.Node(nodeID)
		if node == nil {
			// The membership names a node that no longer exists: dropped, so the account page
			// does not render a dangling id.
			continue
		}
		seen[nodeID] = struct{}{}
		ids = append(ids, nodeID)
		refs = append(refs, map[string]any{"id": nodeID, "name": node.Name, "path": renderOrgPath(nodeID, index)})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i]["id"].(int64) < refs[j]["id"].(int64) })
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, refs
}

// orgMembershipsByAccount groups every membership by account.
func orgMembershipsByAccount(list []domain.OrgMembership) map[int64][]int64 {
	out := make(map[int64][]int64, len(list))
	for _, m := range list {
		out[m.AccountID] = append(out[m.AccountID], m.NodeID)
	}
	return out
}

// boolQuery reads a boolean query parameter, treating anything but an explicit true as false.
func boolQuery(r *http.Request, name string) bool {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return false
	}
	value, err := strconv.ParseBool(raw)
	return err == nil && value
}

// jsonInt64List decodes an optional list of ids, used by the account write path.
func jsonInt64List(raw json.RawMessage) ([]int64, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, domain.ErrInvalidRequest("org_node_ids must be an array of integers")
	}
	return ids, nil
}
