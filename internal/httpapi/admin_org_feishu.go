package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
	"github.com/winger/ai-gateway/internal/feishu"
	"github.com/winger/ai-gateway/internal/orgtree"
)

// The console's half of the Feishu directory sync (M70): one read that renders the merge
// preview, one write that executes it, and three per-person operations for the people the
// automatic merge could not place.
//
// The merge rules live in exactly one place, planFeishuOrg, and both the preview and the
// sync consume the same plan — a preview that disagrees with what the sync would do is the
// bug this structure makes impossible. The plan is recomputed per request from four cheap
// local reads (nodes, accounts, memberships, bound keys) plus the cached directory walk;
// nothing is resolved per person against Feishu.

const (
	// feishuDirectoryCacheTTL bounds how long one directory walk is reused (design D6).
	// The walk costs roughly two calls per department against a rate-limited API, while
	// the local side of the merge is four in-memory reads — so the remote half is cached
	// and the local half never is.
	feishuDirectoryCacheTTL = 60 * time.Second
	// feishuDirectoryUserLimit caps the rendered user list of the preview. The sync itself
	// is not capped by this: it plans everyone and reports the rest by count.
	feishuDirectoryUserLimit = 2000
)

// ---------------------------------------------------------------------------
// directory fetch (with the 60 s cache)
// ---------------------------------------------------------------------------

// fetchFeishuDirectory answers the directory snapshot, from cache when fresh unless
// refresh is set. Feishu failures surface as 502 with a reason a person can act on; the
// second return says whether the answer came from the cache.
func (s *Server) fetchFeishuDirectory(w http.ResponseWriter, r *http.Request, refresh bool) (*feishu.Directory, bool, bool) {
	if !refresh {
		s.feishuDirMu.Lock()
		entry := s.feishuDirCache
		s.feishuDirMu.Unlock()
		if entry != nil && time.Since(entry.fetchedAt) < feishuDirectoryCacheTTL {
			dir := entry.dir
			return &dir, true, true
		}
	}
	dir, err := s.deps.Feishu.Client.Directory(r.Context(), feishu.DirectoryOptions{})
	if err != nil {
		s.deps.Log.Warn("the Feishu directory read failed", "err", err)
		writeAPIError(w, feishuUpstreamError(err))
		return nil, false, false
	}
	s.feishuDirMu.Lock()
	s.feishuDirCache = &feishuDirectoryEntry{dir: dir, fetchedAt: time.Now().UTC()}
	s.feishuDirMu.Unlock()
	return &dir, false, true
}

// invalidateFeishuDirectory drops the cached snapshot. Every write of this feature calls
// it: a sync that just created three nodes must not hand the next dialog a stale picture.
func (s *Server) invalidateFeishuDirectory() {
	s.feishuDirMu.Lock()
	s.feishuDirCache = nil
	s.feishuDirMu.Unlock()
}

// feishuDirectoryGate is the shared pre-flight of all five endpoints: Feishu configured,
// the actor an administrator, and the three stores present.
type feishuDirectoryGate struct {
	Org      OrgAdmin
	Accounts AccountAdmin
	Keys     KeyStore
	Actor    string
}

func (s *Server) openFeishuDirectoryGate(w http.ResponseWriter, r *http.Request) (feishuDirectoryGate, bool) {
	if !s.feishuEnabled() {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return feishuDirectoryGate{}, false
	}
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return feishuDirectoryGate{}, false
	}
	orgStore, ok := portReady(w, s.deps.Org, "organization management")
	if !ok {
		return feishuDirectoryGate{}, false
	}
	accounts, ok := portReady(w, s.deps.Accounts, "account management")
	if !ok {
		return feishuDirectoryGate{}, false
	}
	keys, ok := portReady(w, s.deps.AdminStore, "api key management")
	if !ok {
		return feishuDirectoryGate{}, false
	}
	return feishuDirectoryGate{Org: orgStore, Accounts: accounts, Keys: keys, Actor: actor.Username}, true
}

// feishuUpstreamError turns a failed Feishu call into a 502 whose message says what the
// operator can do about it. Feishu's numeric codes stay in the log, not in the page.
func feishuUpstreamError(err error) *domain.APIError {
	var feishuErr *feishu.Error
	if !errors.As(err, &feishuErr) {
		return domain.ErrUpstream(http.StatusBadGateway, "读取飞书通讯录失败，请稍后重试")
	}
	switch feishuErr.Kind {
	case feishu.KindCredentials:
		return domain.ErrUpstream(http.StatusBadGateway,
			"飞书拒绝了应用凭据：检查 feishu.app_id / app_secret（详情见网关日志）")
	case feishu.KindAppUnavailable:
		return domain.ErrUpstream(http.StatusBadGateway,
			"飞书拒绝了这次通讯录读取：多半缺「获取部门基础信息 / 获取用户基本信息」数据权限，加权限并发布新版本（详情见网关日志）")
	case feishu.KindRateLimited:
		return domain.ErrUpstream(http.StatusBadGateway, "飞书限流，请稍后再试（通讯录有 60 秒缓存）")
	default:
		return domain.ErrUpstream(http.StatusBadGateway, "无法完成飞书通讯录读取：检查出网与 feishu.timeout_s（详情见网关日志）")
	}
}

// ---------------------------------------------------------------------------
// the merge plan
// ---------------------------------------------------------------------------

// feishuDeptPlan is one Feishu department and what the merge would do about it.
type feishuDeptPlan struct {
	Dept feishu.Department
	// NodeID is the local org node this department merges into; 0 when one will be created
	// (or when there is nothing local to merge with).
	NodeID int64
	// Matched is "id" (recognized by feishu_department_id), "name" (same name under the
	// same local parent) or "" (nothing local matches).
	Matched string
	// WillPin says a name match will get the department id stamped on it during sync —
	// that stamping is what makes next week's sync recognize the node after a rename.
	WillPin bool
	// NameConflict marks a name match onto a node that is already pinned to a different
	// department: the pin is skipped (first come, first served), never overwritten.
	NameConflict bool
	// The create side. A department with no name (missing permission), an invalid name, or
	// a depth beyond the tree's limit is skipped, and the field says why.
	WillCreate    bool
	Skipped       bool
	DepthExceeded bool
	NameInvalid   bool
	// parent links the plan to its parent plan, so creation runs parents-first.
	parent *feishuDeptPlan
	// justCreated is set when this very sync created the node; the membership pass uses
	// it to attach people to departments that did not exist when the plan was computed.
	justCreated bool
}

// feishuUserPlan is one Feishu person and where the merge would put them.
type feishuUserPlan struct {
	User feishu.DirectoryUser
	// Account is the local account this person merges onto; nil when unmatched.
	Account *domain.Account
	// MatchedBy is "open_id" (the account already carries this identity), "api_key" (a
	// bound key of that account carries it — the sync promotes the identity onto the
	// account), "name" (exact name, account unbound) or "" (unmatched, operator decides).
	MatchedBy string
	// NeedsBind says the identity still has to be written during the sync.
	NeedsBind bool
	// BindName/BindUnionID are the identity the sync writes. They default to the directory
	// person; the api_key channel prefers the key binding's own values, which carry a real
	// name even when the directory walk could not read one (missing data permission).
	BindName    string
	BindUnionID string
	// JoinDepts are the person's departments, in directory order.
	JoinDepts []*feishuDeptPlan
	// JoinNodeIDs are the node ids (existing ones) the person is not yet attached to and
	// would be attached to during the sync; nodes created by the same run are added on top.
	JoinNodeIDs []int64
	// SkipReason explains a person that looks matchable but must not be auto-merged.
	SkipReason string
}

type feishuOrgPlan struct {
	Dir         *feishu.Directory
	Departments []*feishuDeptPlan
	Users       []*feishuUserPlan
	byDeptID    map[string]*feishuDeptPlan
}

// planFeishuOrg computes what a sync would do, from the directory and the four local reads.
// It is read-only with respect to storage: the preview renders it, the sync executes it.
func planFeishuOrg(ctx context.Context, dir *feishu.Directory, orgStore OrgAdmin, accountStore AccountAdmin, keyStore KeyStore) (*feishuOrgPlan, error) {
	nodes, err := orgStore.ListOrgNodes(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := accountStore.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	memberships, err := orgStore.ListOrgMemberships(ctx)
	if err != nil {
		return nil, err
	}
	identities, err := keyStore.ListAPIKeyFeishuIdentities(ctx)
	if err != nil {
		return nil, err
	}

	plan := &feishuOrgPlan{Dir: dir, byDeptID: map[string]*feishuDeptPlan{}}

	// --- departments (the directory list is BFS-ordered: parents always come first) ----
	pinnedByDept := map[string]*domain.OrgNode{}
	existingBySibling := map[string]*domain.OrgNode{}
	for _, node := range nodes {
		if node.FeishuDepartmentID != "" {
			pinnedByDept[node.FeishuDepartmentID] = node
		}
		existingBySibling[siblingKey(node.ParentID, node.Name)] = node
	}
	// Planned (not yet created) departments register under the same sibling key with their
	// future place in the tree, so children of a to-be-created parent still resolve.
	plannedBySibling := map[string]*feishuDeptPlan{}

	for i := range dir.Departments {
		dept := dir.Departments[i]
		entry := &feishuDeptPlan{Dept: dept}
		var parent *feishuDeptPlan
		if dept.ParentID != "" {
			parent = plan.byDeptID[dept.ParentID]
		}
		entry.parent = parent
		parentKey := siblingKeyOfPlan(parent)

		if node, ok := pinnedByDept[dept.ID]; ok {
			entry.NodeID, entry.Matched = node.ID, "id"
		} else if dept.Name != "" {
			if node, ok := existingBySibling[parentKey+dept.Name]; ok {
				entry.NodeID, entry.Matched = node.ID, "name"
				switch node.FeishuDepartmentID {
				case "":
					entry.WillPin = true
				case dept.ID:
					// Already pinned to this department (a racing refresh); nothing to do.
				default:
					// The same-name node belongs to another Feishu department: that pin is
					// someone else's fact and is never overwritten.
					entry.NameConflict = true
				}
			}
		}
		if entry.NodeID == 0 && !entry.NameConflict {
			switch {
			case dept.Name == "":
				// Without the name permission an unnamed department cannot become a node:
				// creating "od_xxx" junk or silently matching nothing are both worse than
				// telling the operator to add the permission (or create the node by hand).
				entry.Skipped = true
			case parentDepth(parent) >= orgtree.MaxDepth:
				entry.DepthExceeded = true
				entry.Skipped = true
			default:
				if _, err := domain.NormalizeOrgNodeName(dept.Name); err != nil {
					entry.NameInvalid = true
					entry.Skipped = true
				} else {
					entry.WillCreate = true
				}
			}
		}
		plan.Departments = append(plan.Departments, entry)
		plan.byDeptID[dept.ID] = entry
		if dept.Name != "" {
			plannedBySibling[parentKey+dept.Name] = entry
		}
	}

	// --- people --------------------------------------------------------------
	accountsByOpenID := map[string]*domain.Account{}
	accountsByID := map[int64]*domain.Account{}
	accountsByName := map[string][]*domain.Account{}
	for _, account := range accounts {
		accountsByID[account.ID] = account
		if account.FeishuOpenID != "" {
			accountsByOpenID[account.FeishuOpenID] = account
		}
		name := strings.TrimSpace(account.Name)
		if name != "" {
			accountsByName[name] = append(accountsByName[name], account)
		}
	}
	identitiesByOpenID := map[string]domain.KeyFeishuIdentity{}
	for _, identity := range identities {
		if _, seen := identitiesByOpenID[identity.Binding.OpenID]; !seen {
			identitiesByOpenID[identity.Binding.OpenID] = identity
		}
	}
	membersByAccount := map[int64]map[int64]bool{}
	for _, m := range memberships {
		if membersByAccount[m.AccountID] == nil {
			membersByAccount[m.AccountID] = map[int64]bool{}
		}
		membersByAccount[m.AccountID][m.NodeID] = true
	}

	claimed := map[int64]bool{} // account ids already claimed by a same-name merge
	for _, person := range dir.Users {
		entry := &feishuUserPlan{User: person, BindName: person.Name, BindUnionID: person.UnionID}
		for _, departmentID := range person.DepartmentIDs {
			if dept, ok := plan.byDeptID[departmentID]; ok {
				entry.JoinDepts = append(entry.JoinDepts, dept)
			}
		}

		account := accountsByOpenID[person.OpenID]
		if account != nil {
			entry.MatchedBy = "open_id"
		} else if identity, ok := identitiesByOpenID[person.OpenID]; ok {
			account = accountsByID[identity.AccountID]
			entry.MatchedBy = "api_key"
			if strings.TrimSpace(entry.BindName) == "" {
				entry.BindName = identity.Binding.Name
				entry.BindUnionID = identity.Binding.UnionID
			}
		} else if dir.NamesAvailable {
			name := strings.TrimSpace(person.Name)
			// The same-name merge (D3): only an account that carries no Feishu identity
			// yet, and only the first person that claims it. A second Feishu person with
			// the same name stays unmatched — the operator decides, never the algorithm.
			for _, candidate := range accountsByName[name] {
				if candidate.FeishuOpenID != "" || claimed[candidate.ID] {
					continue
				}
				account = candidate
				entry.MatchedBy = "name"
				claimed[candidate.ID] = true
				break
			}
		}
		entry.Account = account
		if account != nil {
			entry.NeedsBind = account.FeishuOpenID != person.OpenID
			for _, dept := range entry.JoinDepts {
				if dept.NodeID > 0 && !membersByAccount[account.ID][dept.NodeID] {
					entry.JoinNodeIDs = append(entry.JoinNodeIDs, dept.NodeID)
				}
				// Departments that will be created first join during the sync, once the
				// node exists; the plan keeps the department so the sync can resolve it.
			}
		}
		plan.Users = append(plan.Users, entry)
	}
	return plan, nil
}

// siblingKey is the uniqueness scope of an org node name: among siblings, roots included.
func siblingKey(parent *int64, name string) string {
	if parent == nil {
		return "root|" + name
	}
	return "p" + strconv.FormatInt(*parent, 10) + "|" + name
}

// siblingKeyOfPlan is where a planned department sits locally: its node when it has one,
// a per-department placeholder while it is still to be created.
func siblingKeyOfPlan(entry *feishuDeptPlan) string {
	if entry == nil {
		return "root|"
	}
	if entry.NodeID > 0 {
		return "p" + strconv.FormatInt(entry.NodeID, 10) + "|"
	}
	return "new:" + entry.Dept.ID + "|"
}

// parentDepth is the local depth of a plan's parent; -1 stands for the virtual root.
func parentDepth(entry *feishuDeptPlan) int {
	if entry == nil {
		return -1
	}
	return entry.Dept.Depth
}

// planStats summarizes the plan for the confirm dialog and the sync response.
type planStats struct {
	Departments        int `json:"departments"`
	DepartmentsCreate  int `json:"departments_to_create"`
	DepartmentsPin     int `json:"departments_to_pin"`
	DepartmentsSkipped int `json:"departments_skipped"`
	Users              int `json:"users"`
	UsersMatched       int `json:"users_matched"`
	UsersUnmatched     int `json:"users_unmatched"`
	UsersAlreadySynced int `json:"users_already_synced"`
	MembershipsToAdd   int `json:"memberships_to_add"`
}

func computePlanStats(plan *feishuOrgPlan) planStats {
	stats := planStats{Departments: len(plan.Departments), Users: len(plan.Users)}
	for _, dept := range plan.Departments {
		switch {
		case dept.WillCreate:
			stats.DepartmentsCreate++
		case dept.WillPin:
			stats.DepartmentsPin++
		case dept.Skipped:
			stats.DepartmentsSkipped++
		}
	}
	for _, user := range plan.Users {
		if user.Account == nil {
			stats.UsersUnmatched++
			continue
		}
		// A membership on a node this very run will create counts too, otherwise the confirm
		// dialog promises "0 new memberships" on a first sync — which is exactly the run
		// where every membership is new.
		joins := len(user.JoinNodeIDs) + plannedJoins(user)
		if user.NeedsBind || joins > 0 {
			stats.UsersMatched++
			stats.MembershipsToAdd += joins
		} else {
			stats.UsersAlreadySynced++
		}
	}
	return stats
}

// plannedJoins counts the person's departments that have no local node yet and will get one.
func plannedJoins(user *feishuUserPlan) int {
	count := 0
	for _, dept := range user.JoinDepts {
		if dept.WillCreate {
			count++
		}
	}
	return count
}

// ---------------------------------------------------------------------------
// endpoints
// ---------------------------------------------------------------------------

// handleAdminListFeishuDirectory renders the merge preview the dialog opens with.
func (s *Server) handleAdminListFeishuDirectory(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	dir, cached, ok := s.fetchFeishuDirectory(w, r, boolQuery(r, "refresh"))
	if !ok {
		return
	}
	plan, err := planFeishuOrg(r.Context(), dir, gate.Org, gate.Accounts, gate.Keys)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	// Per-department people counts (direct members; the dialog adds descendants itself).
	directUsers := map[string]int{}
	for _, person := range plan.Dir.Users {
		for _, departmentID := range person.DepartmentIDs {
			directUsers[departmentID]++
		}
	}

	departments := make([]map[string]any, 0, len(plan.Departments))
	for _, dept := range plan.Departments {
		var parentID any
		if dept.Dept.ParentID != "" {
			parentID = dept.Dept.ParentID
		}
		departments = append(departments, map[string]any{
			"id": dept.Dept.ID, "parent_id": parentID, "name": dept.Dept.Name, "depth": dept.Dept.Depth,
			"direct_user_count": directUsers[dept.Dept.ID],
			"local": map[string]any{
				"node_id":       jsonNilInt64(dept.NodeID),
				"matched":       dept.Matched,
				"will_pin":      dept.WillPin,
				"name_conflict": dept.NameConflict,
				"will_create":   dept.WillCreate,
				"skipped":       dept.Skipped,
			},
		})
	}

	users := make([]map[string]any, 0, len(plan.Users))
	usersTruncated := false
	for i, user := range plan.Users {
		if i >= feishuDirectoryUserLimit {
			usersTruncated = true
			break
		}
		entry := map[string]any{
			"open_id": user.User.OpenID, "union_id": user.User.UnionID, "name": user.User.Name,
			"department_ids": user.User.DepartmentIDs,
			"account":        nil,
			"join_nodes":     joinNodesJSON(user),
		}
		if user.Account != nil {
			entry["account"] = map[string]any{
				"id": user.Account.ID, "name": user.Account.Name, "matched_by": user.MatchedBy,
				"needs_bind": user.NeedsBind,
			}
		}
		users = append(users, entry)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at":      dir.FetchedAt.UTC().Format(time.RFC3339),
		"cached":          cached,
		"names_available": dir.NamesAvailable,
		"truncated":       dir.Truncated,
		"departments":     departments,
		"users":           users,
		"users_truncated": usersTruncated,
		"stats":           computePlanStats(plan),
		"warnings":        feishuWarnings(dir),
	})
}

func jsonNilInt64(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// joinNodesJSON renders where a person would land locally.
func joinNodesJSON(user *feishuUserPlan) []map[string]any {
	out := []map[string]any{}
	for _, dept := range user.JoinDepts {
		out = append(out, map[string]any{
			"department_id": dept.Dept.ID, "name": dept.Dept.Name,
			"node_id": jsonNilInt64(dept.NodeID), "will_create": dept.WillCreate,
		})
	}
	return out
}

func feishuWarnings(dir *feishu.Directory) []string {
	warnings := []string{}
	if !dir.NamesAvailable {
		warnings = append(warnings, "names_unavailable")
	}
	if dir.Truncated {
		warnings = append(warnings, "directory_truncated")
	}
	return warnings
}

// feishuUserFromDirectory re-resolves one person from the (cached) directory. The
// per-person endpoints need the union id and the display name, which only the directory
// carries — an open_id alone in the path is not enough to write a binding.
func (s *Server) feishuUserFromDirectory(w http.ResponseWriter, r *http.Request, openID string) (*feishu.DirectoryUser, *feishu.Directory, bool) {
	dir, _, ok := s.fetchFeishuDirectory(w, r, false)
	if !ok {
		return nil, nil, false
	}
	for i := range dir.Users {
		if dir.Users[i].OpenID == openID {
			return &dir.Users[i], dir, true
		}
	}
	writeAPIError(w, domain.ErrNotFound("feishu user "+openID+" is not in the current directory; refresh and retry"))
	return nil, nil, false
}

// ensureUserDepartmentNodes makes sure the departments a person belongs to exist as local
// nodes — the missing ones are created parents-first exactly like the bulk sync would,
// including any ancestors between the root and the person's departments. It answers the
// node ids of the person's own departments, how many nodes appeared, and the warnings for
// departments that could not be placed.
func ensureUserDepartmentNodes(ctx context.Context, gate feishuDirectoryGate, dir *feishu.Directory, person *feishu.DirectoryUser) (nodeIDs []int64, created int, warnings []string, err error) {
	if len(person.DepartmentIDs) == 0 {
		return nil, 0, nil, nil
	}
	// The union of the person's departments and all their ancestors, as a set of ids.
	needed := map[string]bool{}
	byID := map[string]feishu.Department{}
	for i := range dir.Departments {
		byID[dir.Departments[i].ID] = dir.Departments[i]
	}
	for _, departmentID := range person.DepartmentIDs {
		for id := departmentID; id != "" && !needed[id]; {
			needed[id] = true
			department, ok := byID[id]
			if !ok {
				break
			}
			id = department.ParentID
		}
	}

	nodes, err := gate.Org.ListOrgNodes(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	pinnedByDept := map[string]*domain.OrgNode{}
	existingBySibling := map[string]*domain.OrgNode{}
	for _, node := range nodes {
		if node.FeishuDepartmentID != "" {
			pinnedByDept[node.FeishuDepartmentID] = node
		}
		existingBySibling[siblingKey(node.ParentID, node.Name)] = node
	}

	// Resolve (and create) in the directory's BFS order, which is parents-first; planned
	// siblings register under their future key so children chain onto them.
	resolved := map[string]int64{} // department id → node id
	plannedSibling := map[string]*feishuDeptPlan{}
	now := time.Now().UTC()
	for i := range dir.Departments {
		dept := dir.Departments[i]
		if !needed[dept.ID] {
			continue
		}
		parentKey := "root|"
		if dept.ParentID != "" {
			if parent, ok := plannedSibling["dept:"+dept.ParentID]; ok {
				parentKey = siblingKeyOfPlan(parent)
			} else if parentID, ok := resolved[dept.ParentID]; ok {
				parentKey = "p" + strconv.FormatInt(parentID, 10) + "|"
			} else {
				// The parent is not in the needed set and has no node: this department
				// would dangle, which only a broken ancestor chain can cause.
				warnings = append(warnings, "部门 "+dept.ID+" 的上级节点缺失，已跳过")
				continue
			}
		}
		if node, ok := pinnedByDept[dept.ID]; ok {
			resolved[dept.ID] = node.ID
			plannedSibling["dept:"+dept.ID] = &feishuDeptPlan{Dept: dept, NodeID: node.ID}
			continue
		}
		if node, ok := existingBySibling[parentKey+dept.Name]; ok && dept.Name != "" {
			resolved[dept.ID] = node.ID
			plannedSibling["dept:"+dept.ID] = &feishuDeptPlan{Dept: dept, NodeID: node.ID}
			if node.FeishuDepartmentID == "" {
				// Stamp the id like the sync does, so the next run recognizes the node.
				if pinErr := gate.Org.SetOrgNodeFeishuDepartment(ctx, node.ID, dept.ID); pinErr != nil {
					warnings = append(warnings, "节点「"+node.Name+"」标记飞书部门失败："+pinErr.Error())
				} else {
					created++
				}
			}
			continue
		}
		if dept.Name == "" {
			warnings = append(warnings, "部门 "+dept.ID+" 没有名称（缺数据权限），已跳过")
			continue
		}
		name, nameErr := domain.NormalizeOrgNodeName(dept.Name)
		if nameErr != nil {
			warnings = append(warnings, "部门 "+dept.ID+" 的名称无法使用（"+nameErr.Error()+"），已跳过")
			continue
		}
		node := &domain.OrgNode{Name: name, SortOrder: 100, FeishuDepartmentID: dept.ID, FeishuSyncedAt: &now}
		if parentID, ok := resolved[dept.ParentID]; ok && dept.ParentID != "" {
			parent := parentID
			node.ParentID = &parent
		} else if dept.ParentID != "" {
			warnings = append(warnings, "部门 "+dept.ID+" 的上级节点缺失，已跳过")
			continue
		}
		id, createErr := gate.Org.CreateOrgNode(ctx, node)
		if createErr != nil {
			if toAPIError(createErr).Status == http.StatusConflict {
				warnings = append(warnings, "部门「"+dept.Name+"」已存在同名节点，未重复创建")
				continue
			}
			return nil, created, warnings, createErr
		}
		created++
		resolved[dept.ID] = id
		plannedSibling["dept:"+dept.ID] = &feishuDeptPlan{Dept: dept, NodeID: id}
	}

	for _, departmentID := range person.DepartmentIDs {
		if id, ok := resolved[departmentID]; ok {
			nodeIDs = append(nodeIDs, id)
		} else {
			warnings = append(warnings, "人员所属部门 "+departmentID+" 没有对应的本地节点")
		}
	}
	return nodeIDs, created, warnings, nil
}

// handleAdminSyncFeishuOrg executes the plan: departments first (parents before children),
// then the people the merge matched on its own.
func (s *Server) handleAdminSyncFeishuOrg(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	dir, _, ok := s.fetchFeishuDirectory(w, r, false)
	if !ok {
		return
	}
	plan, err := planFeishuOrg(r.Context(), dir, gate.Org, gate.Accounts, gate.Keys)
	if !ok {
		return
	}
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}

	ctx := r.Context()
	now := time.Now().UTC()
	// The plan is mutated as the sync runs (created departments stop being "to create"), so
	// the numbers the operator confirmed are captured first. What actually happened is in
	// created_nodes/linked_users below.
	plannedStats := computePlanStats(plan)
	type createdNode struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		DepartmentID string `json:"department_id"`
	}
	type skippedRow struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	type linkedUser struct {
		OpenID      string  `json:"open_id"`
		Name        string  `json:"name"`
		AccountID   int64   `json:"account_id"`
		AccountName string  `json:"account_name"`
		MatchedBy   string  `json:"matched_by"`
		NodesAdded  int     `json:"nodes_added"`
		JoinNodeIDs []int64 `json:"join_node_ids"`
	}

	createdNodes := []createdNode{}
	skippedDepartments := []skippedRow{}
	linkedUsers := []linkedUser{}
	skippedUsers := []skippedRow{}
	nodesTouched, membersTouched := 0, 0

	// Departments, in BFS order (a plan entry is always appended after its parent, so the
	// parent's node id exists by the time a child is created under it).
	for _, dept := range plan.Departments {
		switch {
		case dept.WillCreate:
			node := &domain.OrgNode{Name: dept.Dept.Name, SortOrder: 100,
				FeishuDepartmentID: dept.Dept.ID, FeishuSyncedAt: &now}
			if dept.parent != nil && dept.parent.NodeID > 0 {
				parent := dept.parent.NodeID
				node.ParentID = &parent
			}
			id, err := gate.Org.CreateOrgNode(ctx, node)
			if err != nil {
				// A sibling with the same name appeared meanwhile: report and go on — one
				// lost department must not fail a whole sync.
				if toAPIError(err).Status == http.StatusConflict {
					skippedDepartments = append(skippedDepartments,
						skippedRow{ID: dept.Dept.ID, Name: dept.Dept.Name, Reason: "sibling_name_exists"})
					continue
				}
				writeAPIError(w, toAPIError(err))
				return
			}
			dept.NodeID, dept.WillCreate = id, false
			dept.justCreated = true
			createdNodes = append(createdNodes, createdNode{ID: id, Name: node.Name, DepartmentID: dept.Dept.ID})
			nodesTouched++
			s.audit(ctx, gate.Actor, "create", "org_node", strconv.FormatInt(id, 10), map[string]any{
				"name": node.Name, "source": "feishu", "feishu_department_id": dept.Dept.ID,
			}, "ok")
		case dept.WillPin:
			if err := gate.Org.SetOrgNodeFeishuDepartment(ctx, dept.NodeID, dept.Dept.ID); err != nil {
				if toAPIError(err).Status == http.StatusConflict {
					skippedDepartments = append(skippedDepartments,
						skippedRow{ID: dept.Dept.ID, Name: dept.Dept.Name, Reason: "node_pinned_to_other_department"})
					continue
				}
				writeAPIError(w, toAPIError(err))
				return
			}
			dept.WillPin = false
			nodesTouched++
		}
	}

	// People the merge matched on its own. Memberships are re-read once here: the plan's
	// snapshot predates the node creation above, and the new nodes are exactly what a
	// matched person may now need to join.
	existingMemberships, err := gate.Org.ListOrgMemberships(ctx)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	membersByAccount := map[int64]map[int64]bool{}
	for _, m := range existingMemberships {
		if membersByAccount[m.AccountID] == nil {
			membersByAccount[m.AccountID] = map[int64]bool{}
		}
		membersByAccount[m.AccountID][m.NodeID] = true
	}

	for _, user := range plan.Users {
		if user.Account == nil {
			continue
		}
		joinNodeIDs := append([]int64{}, user.JoinNodeIDs...)
		for _, dept := range user.JoinDepts {
			if dept.justCreated {
				joinNodeIDs = append(joinNodeIDs, dept.NodeID)
			}
		}
		// Drop anything the account already holds (races or a stale plan snapshot).
		holders := membersByAccount[user.Account.ID]
		if holders == nil {
			holders = map[int64]bool{}
			membersByAccount[user.Account.ID] = holders
		}
		fresh := joinNodeIDs[:0]
		for _, id := range joinNodeIDs {
			if !holders[id] {
				fresh = append(fresh, id)
			}
		}
		joinNodeIDs = fresh
		if !user.NeedsBind && len(joinNodeIDs) == 0 {
			continue // already in sync: the second run of a sync writes nothing
		}
		if user.NeedsBind {
			if err := gate.Accounts.BindAccountFeishu(ctx, user.Account.ID, domain.FeishuBinding{
				OpenID: user.User.OpenID, UnionID: user.BindUnionID, Name: user.BindName, BoundBy: "sync",
			}); err != nil {
				if toAPIError(err).Status == http.StatusConflict {
					// The identity landed on another account meanwhile (or this account was
					// bound by hand): first come, first served, reported not overwritten.
					skippedUsers = append(skippedUsers,
						skippedRow{ID: user.User.OpenID, Name: user.User.Name, Reason: "identity_taken"})
					continue
				}
				writeAPIError(w, toAPIError(err))
				return
			}
		}
		nodesAdded := 0
		if len(joinNodeIDs) > 0 {
			if err := gate.Org.AddAccountOrgNodes(ctx, user.Account.ID, joinNodeIDs); err != nil {
				writeAPIError(w, toAPIError(err))
				return
			}
			nodesAdded = len(joinNodeIDs)
			membersTouched += nodesAdded
			for _, id := range joinNodeIDs {
				holders[id] = true
			}
		}
		linkedUsers = append(linkedUsers, linkedUser{
			OpenID: user.User.OpenID, Name: user.User.Name,
			AccountID: user.Account.ID, AccountName: user.Account.Name,
			MatchedBy: user.MatchedBy, NodesAdded: nodesAdded, JoinNodeIDs: joinNodeIDs,
		})
		s.audit(ctx, gate.Actor, "feishu_bind", "account", strconv.FormatInt(user.Account.ID, 10), map[string]any{
			"open_id": user.User.OpenID, "name": user.User.Name, "matched_by": user.MatchedBy,
			"nodes_added": nodesAdded, "source": "sync",
		}, "ok")
	}

	s.audit(ctx, gate.Actor, "sync_feishu", "org", "", map[string]any{
		"created_nodes": len(createdNodes), "linked_users": len(linkedUsers),
		"skipped_departments": len(skippedDepartments), "skipped_users": len(skippedUsers),
	}, "ok")

	if nodesTouched > 0 || membersTouched > 0 {
		// Node tags are inherited by whole subtrees and memberships feed the routing
		// snapshot, so one full invalidation covers every write of this sync.
		s.reload(ctx, "feishu org synced", true)
	}
	s.invalidateFeishuDirectory()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "stats": plannedStats,
		"created_nodes": createdNodes, "linked_users": linkedUsers,
		"skipped_departments": skippedDepartments, "skipped_users": skippedUsers,
		"warnings": feishuWarnings(dir),
	})
}

// handleAdminCreateAccountFromFeishuUser creates the local account for one unmatched person.
func (s *Server) handleAdminCreateAccountFromFeishuUser(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	openID := r.PathValue("open_id")
	person, dir, ok := s.feishuUserFromDirectory(w, r, openID)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = strings.TrimSpace(person.Name)
	}
	if name == "" {
		// No name permission and no hand-typed name: refuse rather than create "ou_xxx".
		writeAPIError(w, domain.ErrInvalidRequest(
			"这个飞书人员没有可读的姓名：请先在飞书后台补数据权限，或在请求里手填 name"))
		return
	}
	name, err := domain.NormalizeAccountName(name)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if existing, err := gate.Accounts.GetAccountByName(r.Context(), name); err == nil && existing != nil {
		writeAPIError(w, domain.ErrConflict(
			"已存在同名账户「"+name+"」：请改用「绑定账号」把这个人员绑到它上面"))
		return
	}
	account := &domain.Account{
		Name: name, BillingMode: domain.BillingPrepaid, Note: body.Note,
		TagsJSON: "[]", Status: "active", AutoSuspend: true,
	}
	id, err := gate.Accounts.UpsertAccount(r.Context(), account)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if err := gate.Accounts.BindAccountFeishu(r.Context(), id, domain.FeishuBinding{
		OpenID: person.OpenID, UnionID: person.UnionID, Name: person.Name, BoundBy: gate.Actor,
	}); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	nodeIDs, createdNodes, warnings, err := ensureUserDepartmentNodes(r.Context(), gate, dir, person)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if len(nodeIDs) > 0 {
		if err := gate.Org.AddAccountOrgNodes(r.Context(), id, nodeIDs); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	s.audit(r.Context(), gate.Actor, "create", "account", strconv.FormatInt(id, 10), map[string]any{
		"name": name, "source": "feishu", "open_id": person.OpenID,
		"org_node_ids": nodeIDs, "departments_created": createdNodes,
	}, "ok")
	s.reload(r.Context(), "feishu user joined as an account", true)
	s.invalidateFeishuDirectory()

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "account": map[string]any{"id": id, "name": name},
		"open_id": person.OpenID, "org_node_ids": nodeIDs, "warnings": warnings,
	})
}

// handleAdminBindAccountFeishuUser attaches one person's identity to an existing account.
func (s *Server) handleAdminBindAccountFeishuUser(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	openID := r.PathValue("open_id")
	person, dir, ok := s.feishuUserFromDirectory(w, r, openID)
	if !ok {
		return
	}
	var body struct {
		AccountID *int64 `json:"account_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.AccountID == nil || *body.AccountID <= 0 {
		writeAPIError(w, domain.ErrInvalidRequest("account_id is required"))
		return
	}
	account, err := gate.Accounts.GetAccount(r.Context(), *body.AccountID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if account.FeishuOpenID != "" && account.FeishuOpenID != openID {
		writeAPIError(w, domain.ErrConflict(
			"账户「"+account.Name+"」已绑定另一个飞书身份：请先解绑，再重新绑定"))
		return
	}
	previous := account.FeishuOpenID
	if err := gate.Accounts.BindAccountFeishu(r.Context(), account.ID, domain.FeishuBinding{
		OpenID: person.OpenID, UnionID: person.UnionID, Name: person.Name, BoundBy: gate.Actor,
	}); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	nodeIDs, createdNodes, warnings, err := ensureUserDepartmentNodes(r.Context(), gate, dir, person)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if len(nodeIDs) > 0 {
		if err := gate.Org.AddAccountOrgNodes(r.Context(), account.ID, nodeIDs); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	s.audit(r.Context(), gate.Actor, "feishu_bind", "account", strconv.FormatInt(account.ID, 10), map[string]any{
		"open_id": person.OpenID, "name": person.Name, "previous_open_id": previous,
		"matched_by": "manual", "org_node_ids": nodeIDs, "departments_created": createdNodes,
	}, "ok")
	if len(nodeIDs) > 0 || createdNodes > 0 {
		s.reload(r.Context(), "feishu user joined org nodes", true)
	}
	s.invalidateFeishuDirectory()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "account": map[string]any{"id": account.ID, "name": account.Name},
		"open_id": person.OpenID, "org_node_ids": nodeIDs, "warnings": warnings,
	})
}

// handleAdminUnbindAccountFeishuUser clears one person's account mapping. It is idempotent
// and never touches the key-level bindings (M60), which are what the portal login reads.
func (s *Server) handleAdminUnbindAccountFeishuUser(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	openID := r.PathValue("open_id")
	account, err := gate.Accounts.FindAccountByFeishuOpenID(r.Context(), openID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if account == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unbound": false})
		return
	}
	changed, err := gate.Accounts.UnbindAccountFeishu(r.Context(), account.ID)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if changed {
		s.audit(r.Context(), gate.Actor, "feishu_unbind", "account", strconv.FormatInt(account.ID, 10),
			map[string]any{"open_id": openID, "account_name": account.Name}, "ok")
	}
	s.invalidateFeishuDirectory()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unbound": changed, "account_id": account.ID})
}
