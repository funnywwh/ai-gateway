package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/feishu"
	"github.com/funnywwh/ai-gateway/internal/orgtree"
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

// fetchFeishuDirectory answers the directory snapshot of one company, from cache when fresh
// unless refresh is set. Feishu failures surface as 502 with a reason a person can act on; the
// second return says whether the answer came from the cache.
func (s *Server) fetchFeishuDirectory(w http.ResponseWriter, r *http.Request, company *feishu.Company, refresh bool) (*feishu.Directory, bool, bool) {
	if !refresh {
		s.feishuDirMu.Lock()
		entry := s.feishuDirCache[company.AppID]
		s.feishuDirMu.Unlock()
		if entry != nil && time.Since(entry.fetchedAt) < feishuDirectoryCacheTTL {
			dir := entry.dir
			return &dir, true, true
		}
	}
	dir, err := company.Client.Directory(r.Context(), feishu.DirectoryOptions{})
	if err != nil {
		s.deps.Log.Warn("the Feishu directory read failed", "company", company.AppID, "err", err)
		writeAPIError(w, feishuUpstreamError(err))
		return nil, false, false
	}
	s.feishuDirMu.Lock()
	if s.feishuDirCache == nil {
		s.feishuDirCache = map[string]*feishuDirectoryEntry{}
	}
	s.feishuDirCache[company.AppID] = &feishuDirectoryEntry{dir: dir, fetchedAt: time.Now().UTC()}
	s.feishuDirMu.Unlock()
	return &dir, false, true
}

// invalidateFeishuDirectory drops every cached snapshot. Every write of this feature calls it:
// a sync that just created three nodes must not hand the next dialog a stale picture, and since
// writes are rare, clearing all companies is both the simplest rule and the safest one.
func (s *Server) invalidateFeishuDirectory() {
	s.feishuDirMu.Lock()
	s.feishuDirCache = nil
	s.feishuDirMu.Unlock()
}

// feishuCompanies is the company list every org-sync endpoint resolves its `company` parameter
// against. A deployment whose dependencies predate M92 (an older test fixture) has no list and
// therefore behaves as the single-company deployment M70 supported.
func (s *Server) feishuCompanies() []feishu.Company {
	deps := s.deps.Feishu
	if deps == nil {
		return nil
	}
	if len(deps.Companies) > 0 {
		return deps.Companies
	}
	if deps.Client != nil {
		return []feishu.Company{{
			AppID: deps.Client.AppID, Name: "本公司", RootName: "本公司", Identity: true, Client: deps.Client,
		}}
	}
	return nil
}

// resolveFeishuCompany turns the request's company parameter (an app id or a company name) into
// one company. An empty token is the identity application, which is what keeps every M70/M72
// caller — scripts, MCP clients, the console's account binding — working unchanged.
func (s *Server) resolveFeishuCompany(w http.ResponseWriter, token string) (*feishu.Company, bool) {
	company, err := feishu.FindCompany(s.feishuCompanies(), token)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return nil, false
	}
	return company, true
}

// feishuDirectoryGate is the shared pre-flight of all five endpoints: Feishu configured, the
// actor an administrator, and the stores present.
type feishuDirectoryGate struct {
	Org      OrgAdmin
	Accounts AccountAdmin
	Keys     KeyStore
	People   FeishuPersonAdmin
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
	people, ok := portReady(w, s.deps.FeishuPeople, "feishu company person mapping")
	if !ok {
		return feishuDirectoryGate{}, false
	}
	return feishuDirectoryGate{Org: orgStore, Accounts: accounts, Keys: keys, People: people, Actor: actor.Username}, true
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

// feishuSelection is the set of Feishu department ids the operator chose to sync. A nil
// set means "everything", which is what an omitted parameter has always meant (M70) and
// keeps the MCP/script path working unchanged.
//
// Selection decides *what gets written this run*, never how people are matched: the
// same-name claiming stays a property of the whole directory, so picking a subset cannot
// silently change which account a person merges onto (design D11).
type feishuSelection struct {
	ids map[string]bool
}

func (s feishuSelection) all() bool { return s.ids == nil }

func (s feishuSelection) has(id string) bool { return s.all() || s.ids[id] }

// parseFeishuSelection turns the request's department ids into a selection, dropping ids the
// directory does not know (a department deleted between preview and sync) and reporting them
// so the caller can tell the operator instead of failing the whole run (design D14).
func parseFeishuSelection(raw []string) feishuSelection {
	if raw == nil {
		return feishuSelection{}
	}
	ids := map[string]bool{}
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			id := strings.TrimSpace(part)
			if id == "" {
				continue
			}
			if ids[id] {
				continue
			}
			ids[id] = true
		}
	}
	return feishuSelection{ids: ids}
}

// decodeOptionalJSON decodes a body that may legitimately be absent. The sync endpoint is
// usable with no body at all (that is the "sync everything" call), so an empty body is not
// the malformed-JSON error decodeJSON reports.
func decodeOptionalJSON(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("failed to read the request body")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("malformed JSON body")
	}
	return nil
}

// unknownSelectionIDs reports the requested ids that the directory does not contain.
func unknownSelectionIDs(sel feishuSelection, dir *feishu.Directory) []string {
	if sel.all() {
		return nil
	}
	known := map[string]bool{feishu.RootDepartmentID: true}
	for _, dept := range dir.Departments {
		known[dept.ID] = true
	}
	unknown := []string{}
	for id := range sel.ids {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// companyRootPlan is where one company's subtree hangs: the local node that stands for the
// company itself, i.e. the directory's virtual root "0" (M92).
//
// Three ways it can come to exist, in this order of preference: it is already marked as this
// company's node; an unclaimed root node with the company's name is adopted (linked) by this
// run; or it is created. Adoption is what lets an operator create the company node by hand
// first — under whatever name `root_node` names — and have the sync recognize it.
type companyRootPlan struct {
	// NodeID is 0 while the company node still has to be created.
	NodeID int64
	Name   string
	// Matched is "marker" (already linked to this company), "name" (an unclaimed root node is
	// being adopted) or "" (nothing local to adopt).
	Matched    string
	WillCreate bool
	WillAdopt  bool
	// Blocked says the company node cannot be resolved at all (a root node with its name is
	// someone else's): a sync would have nowhere to hang the company's departments, so it is
	// refused before anything is written rather than falling back to forest roots.
	Blocked bool
	// Reparent lists this company's top-level department nodes that currently sit at the root
	// level; adopting the company node moves them under it, which is the one documented
	// structural invariant of a company sync (design D4/D11).
	Reparent []*domain.OrgNode
	// Warnings are refusals and oddities the operator should see (a taken name, a node deeper
	// than the tree allows).
	Warnings []string
	// ParentID is the node's current parent in the marker case (nil = it is a root, as it
	// should be). Anything else is reported, never moved silently.
	ParentID *int64

	reparentIDs map[int64]bool
	appID       string
}

// siblingPrefix is the sibling scope of a company's top-level departments: the company node
// itself when it exists, a per-company placeholder while it is still to be created.
func (p *companyRootPlan) siblingPrefix() string {
	if p.NodeID > 0 {
		return "p" + strconv.FormatInt(p.NodeID, 10) + "|"
	}
	return "company:" + p.appID + "|"
}

// marksReparent reports whether this run moves that node under the company node.
func (p *companyRootPlan) marksReparent(nodeID int64) bool {
	return p.reparentIDs[nodeID]
}

// planCompanyRoot resolves a company's node and the legacy departments it adopts, from the local
// tree alone (no storage access: the preview and the sync share this answer).
func planCompanyRoot(company *feishu.Company, nodes []*domain.OrgNode, index *orgtree.Index) *companyRootPlan {
	root := &companyRootPlan{
		Name:        company.RootName,
		appID:       company.AppID,
		reparentIDs: map[int64]bool{},
	}
	if root.Name == "" {
		root.Name = company.Name
	}
	var namesake *domain.OrgNode
	for _, node := range nodes {
		if linksToCompany(node, company) && node.FeishuDepartmentID == feishu.RootDepartmentID {
			root.NodeID, root.Matched, root.ParentID = node.ID, "marker", node.ParentID
			if node.ParentID != nil {
				// Somebody moved the company node into the tree. It is still the company's node
				// (the departments hang under wherever it is), so it is used and reported.
				root.Warnings = append(root.Warnings, "root_not_at_top: 公司节点「"+node.Name+"」不在根层，本次仍按它挂载，建议把它移回根层")
			}
			break
		}
	}
	if root.NodeID == 0 {
		for _, node := range nodes {
			if node.ParentIDValue() != 0 || node.Name != root.Name {
				continue
			}
			switch {
			case node.FeishuDepartmentID == "" && node.FeishuAppID == "":
				namesake = node
			case node.FeishuDepartmentID == feishu.RootDepartmentID:
				// Another company's node with the same name (checked above for this one).
				namesake = nil
				root.Blocked = true
				root.Warnings = append(root.Warnings,
					"root_name_conflict: 根层「"+root.Name+"」已经是另一家公司的公司节点，请为这家公司配置 root_node 换个名字")
			case node.FeishuAppID != "" || node.FeishuDepartmentID != "":
				// A department node (or another company's node) wearing the company's name:
				// adopting it would graft a company onto someone else's department.
				namesake = nil
				root.Blocked = true
				root.Warnings = append(root.Warnings,
					"root_name_conflict: 根层「"+root.Name+"」已属于另一个飞书部门/公司，请为这家公司配置 root_node 换个名字")
			}
			break
		}
		if namesake != nil {
			root.NodeID, root.Matched, root.WillAdopt = namesake.ID, "name", true
		} else if len(root.Warnings) == 0 {
			root.WillCreate = true
		}
	}
	// Legacy top-level departments: pinned to this company but still sitting at the root level.
	// Only nodes this company owns are touched — a hand-made root node is never moved.
	for _, node := range nodes {
		if !linksToCompany(node, company) || node.FeishuDepartmentID == "" ||
			node.FeishuDepartmentID == feishu.RootDepartmentID || node.ParentIDValue() != 0 {
			continue
		}
		if index != nil && index.SubtreeHeight(node.ID)+1 > orgtree.MaxDepth {
			root.Warnings = append(root.Warnings,
				"reparent_depth_exceeded: 部门「"+node.Name+"」的子树加上公司节点会超过 "+strconv.Itoa(orgtree.MaxDepth)+" 层，已保持原位")
			continue
		}
		root.Reparent = append(root.Reparent, node)
		root.reparentIDs[node.ID] = true
	}
	return root
}

// linksToCompany reports whether a node's Feishu link belongs to that company. A node written
// before M92 has no company at all; those rows are unambiguously the identity application's,
// which is what lets the plan work even if the startup adoption could not run.
func linksToCompany(node *domain.OrgNode, company *feishu.Company) bool {
	if node == nil || node.FeishuAppID == "" {
		return company.Identity
	}
	return node.FeishuAppID == company.AppID
}

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
	// WillStampScope says an id match resolved onto a node written before M92 (no company):
	// the sync stamps this company on it, which is the same self-healing the startup adoption
	// does for the whole table.
	WillStampScope bool
	// WillReparent says the node exists but sits at the root level instead of under this
	// company's node; this run moves it (design D4/D11).
	WillReparent bool
	// NameConflict marks a name match onto a node that is already pinned to a different
	// department: the pin is skipped (first come, first served), never overwritten.
	NameConflict bool
	// The create side. A department with no name (missing permission), an invalid name, or
	// a depth beyond the tree's limit is skipped, and the field says why.
	WillCreate    bool
	Skipped       bool
	DepthExceeded bool
	NameInvalid   bool
	// LocalDepth is the depth the department will have in the local tree (the company node
	// counts), so the depth limit is checked against what the tree will actually look like.
	LocalDepth int
	// Selected says the operator checked this department; Included says it takes part in
	// this run — either because it was checked, or because a checked descendant needs it as
	// its parent (design D9). An included-but-not-selected department contributes its node
	// and nothing else: its people stay out of scope.
	Selected bool
	Included bool
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
	// JoinCompanyRoot marks a person the directory lists directly under the company (no
	// department at all): the virtual root is their only switch, and the company node is
	// where they belong (M92, design D6).
	JoinCompanyRoot bool
	// InScope says this person takes part in the run: one of their own departments is
	// selected, or they belong to no department and the virtual root was selected (D8/D10).
	InScope bool
	// SkipReason explains a person that looks matchable but must not be auto-merged.
	SkipReason string
}

type feishuOrgPlan struct {
	Dir     *feishu.Directory
	Company *feishu.Company
	// Root is where this company's subtree hangs (M92).
	Root        *companyRootPlan
	Departments []*feishuDeptPlan
	Users       []*feishuUserPlan
	Selection   feishuSelection
	// UnknownSelection lists requested ids the directory does not know (D14).
	UnknownSelection []string
	byDeptID         map[string]*feishuDeptPlan
}

// planFeishuOrg computes what a sync of one company would do, from that company's directory and
// the local reads. It is read-only with respect to storage: the preview renders it, the sync
// executes it.
//
// Company scoping (M92) is the difference from M70: a department is recognized by
// (application, department id) or by a name under its parent inside *this* company's subtree,
// and a person is matched through this company's own mapping — the identity application through
// accounts.feishu_*, any other company through feishu_person_links.
func planFeishuOrg(ctx context.Context, company *feishu.Company, dir *feishu.Directory, orgStore OrgAdmin, accountStore AccountAdmin, peopleStore FeishuPersonAdmin, sel feishuSelection) (*feishuOrgPlan, error) {
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
	links, err := peopleStore.ListFeishuPersonLinks(ctx)
	if err != nil {
		return nil, err
	}
	index := orgtree.NewIndex(nodes)

	plan := &feishuOrgPlan{Dir: dir, Company: company, Selection: sel,
		UnknownSelection: unknownSelectionIDs(sel, dir), byDeptID: map[string]*feishuDeptPlan{}}
	plan.Root = planCompanyRoot(company, nodes, index)

	// --- departments (the directory list is BFS-ordered: parents always come first) ----
	pinnedByDept := map[string]*domain.OrgNode{}
	existingBySibling := map[string]*domain.OrgNode{}
	for _, node := range nodes {
		if node.FeishuDepartmentID != "" && linksToCompany(node, company) {
			pinnedByDept[node.FeishuDepartmentID] = node
		}
		existingBySibling[siblingKey(node.ParentID, node.Name)] = node
	}
	// Root depth is where the company node lives; a top-level department hangs under it, so it
	// is one level deeper than a company node at the top (design D3).
	rootDepth := 0
	if plan.Root.NodeID > 0 && index != nil {
		if d := index.Depth(plan.Root.NodeID); d >= 0 {
			rootDepth = d
		}
	}
	rootKey := plan.Root.siblingPrefix()

	for i := range dir.Departments {
		dept := dir.Departments[i]
		entry := &feishuDeptPlan{Dept: dept}
		var parent *feishuDeptPlan
		if dept.ParentID != "" {
			parent = plan.byDeptID[dept.ParentID]
		}
		entry.parent = parent
		// A top-level department's local parent is this company's node, never the forest root:
		// that is what keeps two companies' 研发部 apart (M92).
		parentKey := rootKey
		if parent != nil {
			parentKey = siblingKeyOfPlan(parent)
			entry.LocalDepth = parent.LocalDepth + 1
		} else {
			entry.LocalDepth = rootDepth + 1
		}

		if node, ok := pinnedByDept[dept.ID]; ok {
			entry.NodeID, entry.Matched = node.ID, "id"
			// A link written before M92 carries no company; the sync stamps this one on it as
			// it goes, which is the same repair the startup adoption performs for the table.
			entry.WillStampScope = node.FeishuAppID == ""
			entry.WillReparent = plan.Root.marksReparent(node.ID)
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
			} else if node, ok := existingBySibling["root|"+dept.Name]; ok && parent == nil &&
				node.FeishuDepartmentID == "" && node.FeishuAppID == "" {
				// A hand-made root-level node with this department's name: it is *not* merged
				// (the company's departments live under its company node), but the operator is
				// told why a second node with that name is about to appear.
				plan.Root.Warnings = append(plan.Root.Warnings,
					"unmerged_root_node: 根层的「"+dept.Name+"」不属于任何公司，未参与合并；它多半是手工建的，"+
						"需要的话请手工移到公司节点下")
			}
		}
		if entry.NodeID == 0 && !entry.NameConflict {
			switch {
			case dept.Name == "":
				// Without the name permission an unnamed department cannot become a node:
				// creating "od_xxx" junk or silently matching nothing are both worse than
				// telling the operator to add the permission (or create the node by hand).
				entry.Skipped = true
			case entry.LocalDepth > orgtree.MaxDepth:
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
		if sel.has(dept.ID) {
			entry.Selected, entry.Included = true, true
		}
		plan.Departments = append(plan.Departments, entry)
		plan.byDeptID[dept.ID] = entry
	}

	// A selected department needs its ancestors to exist (a node cannot hang in mid-air), so
	// they are pulled into the run — node only: their people are not in scope (D9/D10).
	for _, entry := range plan.Departments {
		if !entry.Selected {
			continue
		}
		for parent := entry.parent; parent != nil; parent = parent.parent {
			parent.Included = true
		}
	}

	// --- people --------------------------------------------------------------
	accountsByOpenID := map[string]*domain.Account{}
	accountsByID := map[int64]*domain.Account{}
	accountsByName := map[string][]*domain.Account{}
	// mapped marks every account that already carries a Feishu mapping in *any* company: the
	// same-name channel must never put a second person on it (design D10). For the identity
	// application that mapping is the account's own identity; for the others it is a link row.
	mapped := map[int64]bool{}
	// linkedHere is this company's own person → mapping, used to decide whether a matched person
	// still needs a write.
	linkedHere := map[string]domain.FeishuPersonLink{}
	for _, account := range accounts {
		accountsByID[account.ID] = account
		if account.FeishuOpenID != "" {
			mapped[account.ID] = true
			if company.Identity {
				accountsByOpenID[account.FeishuOpenID] = account
			}
		}
		name := strings.TrimSpace(account.Name)
		if name != "" {
			accountsByName[name] = append(accountsByName[name], account)
		}
	}
	for _, link := range links {
		mapped[link.AccountID] = true
		if link.AppID != company.AppID {
			continue
		}
		linkedHere[link.OpenID] = link
		if accountsByOpenID[link.OpenID] == nil {
			if account := accountsByID[link.AccountID]; account != nil {
				accountsByOpenID[link.OpenID] = account
			}
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
			// Scope follows the person's OWN departments (D10): a department pulled in only
			// as somebody else's parent does not drag its people into the run.
			if sel.has(departmentID) {
				entry.InScope = true
			}
		}
		// People Feishu lists directly under the company belong to no department: the
		// virtual root is the only switch that decides for them (D8), and the company node is
		// where they land (M92, design D6).
		if len(person.DepartmentIDs) == 0 && sel.has(feishu.RootDepartmentID) {
			entry.InScope = true
			entry.JoinCompanyRoot = true
		}

		account := accountsByOpenID[person.OpenID]
		if account != nil {
			entry.MatchedBy = "open_id"
		} else if dir.NamesAvailable {
			name := strings.TrimSpace(person.Name)
			// The same-name merge (D3): only an account that carries no Feishu identity
			// yet, and only the first person that claims it. A second Feishu person with
			// the same name stays unmatched — the operator decides, never the algorithm.
			for _, candidate := range accountsByName[name] {
				if mapped[candidate.ID] || claimed[candidate.ID] {
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
			if company.Identity {
				entry.NeedsBind = account.FeishuOpenID != person.OpenID
			} else {
				// Another company writes a link row instead of touching the account identity:
				// that identity is what the DSH portal logs in with, and this company's people
				// could not complete an OAuth flow against our application anyway.
				entry.NeedsBind = linkedHere[person.OpenID].OpenID == ""
			}
			for _, dept := range entry.JoinDepts {
				if dept.NodeID > 0 && !membersByAccount[account.ID][dept.NodeID] {
					entry.JoinNodeIDs = append(entry.JoinNodeIDs, dept.NodeID)
				}
				// Departments that will be created first join during the sync, once the
				// node exists; the plan keeps the department so the sync can resolve it.
			}
			if entry.JoinCompanyRoot && plan.Root.NodeID > 0 && !membersByAccount[account.ID][plan.Root.NodeID] {
				entry.JoinNodeIDs = append(entry.JoinNodeIDs, plan.Root.NodeID)
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

// planStats summarizes the plan for the confirm dialog and the sync response.
type planStats struct {
	// Departments is how many departments take part in the run (selected plus the ancestors
	// they need); DepartmentsSelected is how many the operator actually checked, and
	// DepartmentsAncestors how many exist only to hold a selected department's place.
	Departments          int `json:"departments"`
	DepartmentsSelected  int `json:"departments_selected"`
	DepartmentsAncestors int `json:"departments_ancestors"`
	DepartmentsCreate    int `json:"departments_to_create"`
	DepartmentsPin       int `json:"departments_to_pin"`
	DepartmentsSkipped   int `json:"departments_skipped"`
	// The user counters describe the people in scope only; UsersOutOfScope says how many
	// people the current selection leaves alone.
	Users              int `json:"users"`
	UsersInScope       int `json:"users_in_scope"`
	UsersOutOfScope    int `json:"users_out_of_scope"`
	UsersMatched       int `json:"users_matched"`
	UsersUnmatched     int `json:"users_unmatched"`
	UsersAlreadySynced int `json:"users_already_synced"`
	MembershipsToAdd   int `json:"memberships_to_add"`
	// The company node and the legacy departments this run adopts under it (M92). They are part
	// of the numbers the operator confirms: creating a node changes the tree, and moving an old
	// top-level department changes what its accounts inherit.
	CompanyNodeCreate bool `json:"company_node_to_create"`
	CompanyNodeAdopt  bool `json:"company_node_to_adopt"`
	ReparentNodes     int  `json:"reparent_nodes"`
}

func computePlanStats(plan *feishuOrgPlan) planStats {
	stats := planStats{Users: len(plan.Users)}
	if plan.Root != nil {
		stats.CompanyNodeCreate = plan.Root.WillCreate
		stats.CompanyNodeAdopt = plan.Root.WillAdopt
		stats.ReparentNodes = len(plan.Root.Reparent)
	}
	if !plan.Selection.all() {
		// Counted from the selection, not from the department rows: the virtual root "0" is a
		// checkbox in the dialog but is not a department, and a header that said "已选 0 个部门"
		// while the root is ticked would be a lie.
		stats.DepartmentsSelected = len(plan.Selection.ids) - len(plan.UnknownSelection)
		if stats.DepartmentsSelected < 0 {
			stats.DepartmentsSelected = 0
		}
	}
	for _, dept := range plan.Departments {
		if !dept.Included {
			continue
		}
		stats.Departments++
		if !dept.Selected {
			stats.DepartmentsAncestors++
		}
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
		if !user.InScope {
			stats.UsersOutOfScope++
			continue
		}
		stats.UsersInScope++
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
	company, ok := s.resolveFeishuCompany(w, r.URL.Query().Get("company"))
	if !ok {
		return
	}
	dir, cached, ok := s.fetchFeishuDirectory(w, r, company, boolQuery(r, "refresh"))
	if !ok {
		return
	}
	selection := parseFeishuSelection(r.URL.Query()["departments"])
	plan, err := planFeishuOrg(r.Context(), company, dir, gate.Org, gate.Accounts, gate.People, selection)
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
			// selected/included are what the dialog's checkboxes and the "为层级补建" marks read.
			"selected": dept.Selected,
			"included": dept.Included,
			"local": map[string]any{
				"node_id":       jsonNilInt64(dept.NodeID),
				"matched":       dept.Matched,
				"will_pin":      dept.WillPin,
				"name_conflict": dept.NameConflict,
				"will_create":   dept.WillCreate,
				"skipped":       dept.Skipped,
				// M92: the local depth counts the company node, and the legacy departments this
				// run adopts under it are visible per row as well as in the summary.
				"local_depth":      dept.LocalDepth,
				"will_reparent":    dept.WillReparent,
				"will_stamp_scope": dept.WillStampScope,
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
			"in_scope":       user.InScope,
			"account":        nil,
			"join_nodes":     joinNodesJSON(user),
			// People the directory lists under the company itself land on the company node.
			"join_company_root": user.JoinCompanyRoot,
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
		"company":         feishuCompanyJSON(company, plan.Root),
		"names_available": dir.NamesAvailable,
		"truncated":       dir.Truncated,
		"departments":     departments,
		"users":           users,
		"users_truncated": usersTruncated,
		"stats":           computePlanStats(plan),
		"selection":       r.URL.Query()["departments"],
		// Ids the directory does not know (a department deleted in Feishu, say): reported so
		// the dialog can drop them from its checkboxes instead of pretending they synced.
		"unknown_department_ids": plan.UnknownSelection,
		"warnings":               append(feishuWarnings(dir), plan.Root.Warnings...),
	})
}

// feishuCompanyJSON renders the company a request is about, together with where its subtree
// hangs: which local node is (or becomes) the company node, how it was resolved, and the legacy
// top-level departments this run adopts under it (M92).
func feishuCompanyJSON(company *feishu.Company, root *companyRootPlan) map[string]any {
	out := map[string]any{
		"app_id":   company.AppID,
		"name":     company.Name,
		"identity": company.Identity,
	}
	if root == nil {
		return out
	}
	out["root"] = map[string]any{
		"node_id":     jsonNilInt64(root.NodeID),
		"name":        root.Name,
		"matched":     root.Matched,
		"will_create": root.WillCreate,
		"will_adopt":  root.WillAdopt,
		// Blocked means the sync would refuse: the console disables the button and shows why.
		"blocked": root.Blocked,
	}
	out["reparent_node_ids"] = reparentNodeIDs(root.Reparent)
	out["warnings"] = root.Warnings
	return out
}

// reparentNodeIDs renders the legacy department nodes a run adopts as plain ids (the dialog only
// counts them and links to them); an empty list, never null, so the console needs no null check.
func reparentNodeIDs(nodes []*domain.OrgNode) []int64 {
	out := make([]int64, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node.ID)
	}
	return out
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
// carries — an open_id alone in the path is not enough to write a binding. The company matters
// twice over (M92): its directory is what an open id is looked up in, and an open id is only
// unique inside one company.
func (s *Server) feishuUserFromDirectory(w http.ResponseWriter, r *http.Request, company *feishu.Company, openID string) (*feishu.DirectoryUser, *feishu.Directory, bool) {
	dir, _, ok := s.fetchFeishuDirectory(w, r, company, false)
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
// including any ancestors between the company node and the person's departments, and including
// the company node itself when this is a company's first contact (M92). It answers the node ids
// of the person's own departments, how many nodes appeared, and the warnings for departments
// that could not be placed.
func ensureUserDepartmentNodes(ctx context.Context, company *feishu.Company, gate feishuDirectoryGate, dir *feishu.Directory, person *feishu.DirectoryUser) (nodeIDs []int64, created int, warnings []string, err error) {
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
	// The company node comes first: without it a top-level department has nowhere to go, and
	// this per-person path must land people in the same shape the bulk sync produces.
	root := planCompanyRoot(company, nodes, orgtree.NewIndex(nodes))
	warnings = append(warnings, root.Warnings...)
	rootNodeID := root.NodeID
	if root.Blocked {
		// The company node cannot be resolved. This per-person operation is still useful (the
		// account and its mapping are written), so it continues without a company node and its
		// departments are reported as unplaceable — refusing everything would hide that the
		// mapping itself worked.
		rootNodeID = 0
	}
	if rootNodeID == 0 && !root.Blocked {
		id, createErr := gate.Org.CreateOrgNode(ctx, &domain.OrgNode{
			Name: root.Name, SortOrder: 100, FeishuAppID: company.AppID,
			FeishuDepartmentID: feishu.RootDepartmentID,
		})
		if createErr != nil {
			if toAPIError(createErr).Status == http.StatusConflict {
				warnings = append(warnings, "公司节点「"+root.Name+"」已存在同名节点，未能创建")
			} else {
				return nil, created, warnings, createErr
			}
		} else {
			rootNodeID = id
			created++
		}
	} else if root.WillAdopt && rootNodeID > 0 {
		if linkErr := gate.Org.SetOrgNodeFeishuDepartment(ctx, rootNodeID, company.AppID, feishu.RootDepartmentID); linkErr != nil {
			warnings = append(warnings, "公司节点「"+root.Name+"」标记失败："+linkErr.Error())
		}
	}

	pinnedByDept := map[string]*domain.OrgNode{}
	existingBySibling := map[string]*domain.OrgNode{}
	for _, node := range nodes {
		if node.FeishuDepartmentID != "" && linksToCompany(node, company) {
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
		parentKey := root.siblingPrefix()
		var parentID *int64
		if dept.ParentID == "" {
			if rootNodeID > 0 {
				value := rootNodeID
				parentID = &value
			}
		} else if parent, ok := plannedSibling["dept:"+dept.ParentID]; ok {
			parentKey = siblingKeyOfPlan(parent)
			if parent.NodeID > 0 {
				value := parent.NodeID
				parentID = &value
			}
		} else if resolvedID, ok := resolved[dept.ParentID]; ok {
			parentKey = "p" + strconv.FormatInt(resolvedID, 10) + "|"
			value := resolvedID
			parentID = &value
		} else {
			// The parent is not in the needed set and has no node: this department
			// would dangle, which only a broken ancestor chain can cause.
			warnings = append(warnings, "部门 "+dept.ID+" 的上级节点缺失，已跳过")
			continue
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
				if pinErr := gate.Org.SetOrgNodeFeishuDepartment(ctx, node.ID, company.AppID, dept.ID); pinErr != nil {
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
		node := &domain.OrgNode{Name: name, SortOrder: 100, FeishuAppID: company.AppID,
			FeishuDepartmentID: dept.ID, FeishuSyncedAt: &now, ParentID: parentID}
		if dept.ParentID != "" && parentID == nil {
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

// handleAdminSyncFeishuOrg executes the plan: the company node, departments (parents before
// children) and then the people the merge matched on its own — for one company.
func (s *Server) handleAdminSyncFeishuOrg(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	// Which departments this run covers (design §12). The body is optional: an absent
	// department_ids keeps the M70 "sync everything" semantics for MCP and scripts, while an
	// explicit empty list is refused rather than silently doing nothing.
	var body struct {
		DepartmentIDs []string `json:"department_ids"`
		Company       string   `json:"company"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	// The company may also arrive as a query parameter, which is what the console uses (the body
	// keeps the M70 shape for scripts and MCP clients); the body wins when both are present.
	companyToken := strings.TrimSpace(body.Company)
	if companyToken == "" {
		companyToken = r.URL.Query().Get("company")
	}
	company, ok := s.resolveFeishuCompany(w, companyToken)
	if !ok {
		return
	}
	dir, _, ok := s.fetchFeishuDirectory(w, r, company, false)
	if !ok {
		return
	}
	selection := parseFeishuSelection(body.DepartmentIDs)
	if body.DepartmentIDs != nil && len(selection.ids) == 0 {
		writeAPIError(w, domain.ErrInvalidRequest(
			"department_ids is empty: pick at least one department, or omit the field to sync every department"))
		return
	}
	plan, err := planFeishuOrg(r.Context(), company, dir, gate.Org, gate.Accounts, gate.People, selection)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if !selection.all() && len(plan.UnknownSelection) == len(selection.ids) {
		// Every id the operator sent is gone from the directory: refreshing the dialog is the
		// only useful answer (design D14).
		writeAPIError(w, domain.ErrInvalidRequest(
			"none of the requested departments exist in the Feishu directory anymore; refresh the dialog and pick again"))
		return
	}
	if plan.Root.Blocked {
		// Nothing has been written yet, and without a company node the company's departments
		// would land at the forest root — a tree nobody asked for. Refuse and name the setting.
		writeAPIError(w, domain.ErrConflict(
			"根层已有同名节点「"+plan.Root.Name+"」，无法作为"+company.Name+"的公司节点；"+
				"请把 feishu.companies[].root_node 改成别的名字，或先重命名那个节点"))
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

	// The company node comes first (M92): every top-level department of this company hangs under
	// it, so it has to exist before the first department is created. Adopting an existing node
	// means stamping the company and the virtual root "0" on it — that is what makes the next run
	// recognize it. A name that is already taken by another claim is a refusal, not a guess: the
	// operator fixes `root_node` and retries, which is why this error names the setting.
	rootNodeID := plan.Root.NodeID
	rootCreatedNow := false
	switch {
	case plan.Root.WillCreate:
		id, err := gate.Org.CreateOrgNode(ctx, &domain.OrgNode{
			Name: plan.Root.Name, SortOrder: 100, FeishuAppID: company.AppID,
			FeishuDepartmentID: feishu.RootDepartmentID, FeishuSyncedAt: &now,
		})
		if err != nil {
			if toAPIError(err).Status == http.StatusConflict {
				writeAPIError(w, domain.ErrConflict(
					"根层已有同名节点「"+plan.Root.Name+"」，无法作为这家公司的公司节点；"+
						"请把 feishu.companies[].root_node 改成别的名字，或先重命名那个节点"))
				return
			}
			writeAPIError(w, toAPIError(err))
			return
		}
		rootNodeID, rootCreatedNow = id, true
		nodesTouched++
		createdNodes = append(createdNodes, createdNode{ID: id, Name: plan.Root.Name, DepartmentID: feishu.RootDepartmentID})
		s.audit(ctx, gate.Actor, "create", "org_node", strconv.FormatInt(id, 10), map[string]any{
			"name": plan.Root.Name, "source": "feishu", "company": company.AppID, "company_node": true,
		}, "ok")
	case plan.Root.WillAdopt:
		if err := gate.Org.SetOrgNodeFeishuDepartment(ctx, rootNodeID, company.AppID, feishu.RootDepartmentID); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		nodesTouched++
	}

	// The company's legacy top-level departments move under the company node. The operator saw
	// the count in the confirmation dialog; a move is reported per node, never silent. A move
	// also stamps the company on a link written before M92 — that is the pair (company,
	// department) the next run matches by.
	reparented := []*domain.OrgNode{}
	if rootNodeID > 0 {
		for _, node := range plan.Root.Reparent {
			if err := gate.Org.SetOrgNodeParent(ctx, node.ID, rootNodeID); err != nil {
				skippedDepartments = append(skippedDepartments,
					skippedRow{ID: node.FeishuDepartmentID, Name: node.Name, Reason: "reparent_failed"})
				continue
			}
			if node.FeishuAppID == "" {
				if err := gate.Org.SetOrgNodeFeishuDepartment(ctx, node.ID, company.AppID, node.FeishuDepartmentID); err != nil {
					skippedDepartments = append(skippedDepartments,
						skippedRow{ID: node.FeishuDepartmentID, Name: node.Name, Reason: "reparent_failed"})
					continue
				}
			}
			reparented = append(reparented, node)
			nodesTouched++
			s.audit(ctx, gate.Actor, "update", "org_node", strconv.FormatInt(node.ID, 10), map[string]any{
				"name": node.Name, "source": "feishu", "company": company.AppID,
				"moved_under_company_node": rootNodeID,
			}, "ok")
		}
	}

	// Departments, in BFS order (a plan entry is always appended after its parent, so the
	// parent's node id exists by the time a child is created under it). Departments outside
	// the selection are skipped entirely — including the ones that would only be renamed or
	// pinned by a full run.
	for _, dept := range plan.Departments {
		if !dept.Included {
			continue
		}
		switch {
		case dept.WillCreate:
			node := &domain.OrgNode{Name: dept.Dept.Name, SortOrder: 100,
				FeishuAppID: company.AppID, FeishuDepartmentID: dept.Dept.ID, FeishuSyncedAt: &now}
			switch {
			case dept.parent != nil && dept.parent.NodeID > 0:
				parent := dept.parent.NodeID
				node.ParentID = &parent
			case dept.parent == nil && rootNodeID > 0:
				// A top-level department of this company: it hangs under the company node, not
				// at the forest root (M92, design D3).
				parent := rootNodeID
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
				"name": node.Name, "source": "feishu", "company": company.AppID,
				"feishu_department_id": dept.Dept.ID,
			}, "ok")
		case dept.WillPin:
			if err := gate.Org.SetOrgNodeFeishuDepartment(ctx, dept.NodeID, company.AppID, dept.Dept.ID); err != nil {
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
		case dept.WillStampScope:
			// An id match resolved onto a link from before M92 (no company): stamp this company
			// on it, the same repair the startup adoption performs for the whole table.
			if err := gate.Org.SetOrgNodeFeishuDepartment(ctx, dept.NodeID, company.AppID, dept.Dept.ID); err != nil {
				if toAPIError(err).Status == http.StatusConflict {
					skippedDepartments = append(skippedDepartments,
						skippedRow{ID: dept.Dept.ID, Name: dept.Dept.Name, Reason: "node_pinned_to_other_department"})
					continue
				}
				writeAPIError(w, toAPIError(err))
				return
			}
			dept.WillStampScope = false
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
		if user.Account == nil || !user.InScope {
			continue
		}
		joinNodeIDs := append([]int64{}, user.JoinNodeIDs...)
		for _, dept := range user.JoinDepts {
			if dept.justCreated {
				joinNodeIDs = append(joinNodeIDs, dept.NodeID)
			}
		}
		// People the directory lists under the company itself join the company node, which may
		// have been created by this very run (M92, design D6).
		if user.JoinCompanyRoot && rootCreatedNow {
			joinNodeIDs = append(joinNodeIDs, rootNodeID)
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
			// The identity application writes the account's identity (that column is also the
			// DSH portal login); any other company writes a company-scoped mapping and leaves
			// the account alone (M92, design D5).
			if company.Identity {
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
			} else if err := gate.People.UpsertFeishuPersonLink(ctx, domain.FeishuPersonLink{
				AppID: company.AppID, OpenID: user.User.OpenID, UnionID: user.BindUnionID,
				Name: user.BindName, AccountID: user.Account.ID, BoundBy: "sync",
			}); err != nil {
				if toAPIError(err).Status == http.StatusConflict {
					// This account already maps to another person of this company: two people of
					// one directory never share an account automatically.
					skippedUsers = append(skippedUsers,
						skippedRow{ID: user.User.OpenID, Name: user.User.Name, Reason: "account_taken"})
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
			"nodes_added": nodesAdded, "source": "sync", "company": company.AppID,
		}, "ok")
	}

	s.audit(ctx, gate.Actor, "sync_feishu", "org", "", map[string]any{
		"company":              company.AppID,
		"created_nodes":        len(createdNodes),
		"linked_users":         len(linkedUsers),
		"skipped_departments":  len(skippedDepartments),
		"skipped_users":        len(skippedUsers),
		"departments_selected": plannedStats.DepartmentsSelected,
		"departments_ancestors": plannedStats.DepartmentsAncestors,
		"departments_unknown":  len(plan.UnknownSelection),
		"reparented_nodes":     len(reparented),
	}, "ok")

	if nodesTouched > 0 || membersTouched > 0 {
		// Node tags are inherited by whole subtrees and memberships feed the routing
		// snapshot, so one full invalidation covers every write of this sync.
		s.reload(ctx, "feishu org synced", true)
	}
	s.invalidateFeishuDirectory()

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "stats": plannedStats,
		"company":      feishuCompanyJSON(company, plan.Root),
		"root_node_id": jsonNilInt64(rootNodeID),
		"created_nodes": createdNodes, "linked_users": linkedUsers,
		"reparented_nodes":     reparentRows(reparented),
		"skipped_departments":  skippedDepartments,
		"skipped_users":        skippedUsers,
		"unknown_department_ids": plan.UnknownSelection,
		"warnings":               append(feishuWarnings(dir), plan.Root.Warnings...),
	})
}

// reparentRows renders the legacy departments a sync adopted under their company node: id, name
// and the Feishu department they carry, so the console can list what moved without a second read.
func reparentRows(nodes []*domain.OrgNode) []map[string]any {
	out := []map[string]any{}
	for _, node := range nodes {
		out = append(out, map[string]any{
			"id": node.ID, "name": node.Name, "feishu_department_id": node.FeishuDepartmentID,
		})
	}
	return out
}

// handleAdminCreateAccountFromFeishuUser creates the local account for one unmatched person.
func (s *Server) handleAdminCreateAccountFromFeishuUser(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	company, ok := s.resolveFeishuCompany(w, r.URL.Query().Get("company"))
	if !ok {
		return
	}
	openID := r.PathValue("open_id")
	person, dir, ok := s.feishuUserFromDirectory(w, r, company, openID)
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
	if company.Identity {
		if err := gate.Accounts.BindAccountFeishu(r.Context(), id, domain.FeishuBinding{
			OpenID: person.OpenID, UnionID: person.UnionID, Name: person.Name, BoundBy: gate.Actor,
		}); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	} else if err := gate.People.UpsertFeishuPersonLink(r.Context(), domain.FeishuPersonLink{
		AppID: company.AppID, OpenID: person.OpenID, UnionID: person.UnionID,
		Name: person.Name, AccountID: id, BoundBy: gate.Actor,
	}); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	nodeIDs, createdNodes, warnings, err := ensureUserDepartmentNodes(r.Context(), company, gate, dir, person)
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
		"name": name, "source": "feishu", "open_id": person.OpenID, "company": company.AppID,
		"org_node_ids": nodeIDs, "departments_created": createdNodes,
	}, "ok")
	s.reload(r.Context(), "feishu user joined as an account", true)
	s.invalidateFeishuDirectory()

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok": true, "account": map[string]any{"id": id, "name": name},
		"open_id": person.OpenID, "company": feishuCompanyJSON(company, nil),
		"org_node_ids": nodeIDs, "warnings": warnings,
	})
}

// handleAdminBindAccountFeishuUser attaches one person's identity to an existing account.
func (s *Server) handleAdminBindAccountFeishuUser(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	company, ok := s.resolveFeishuCompany(w, r.URL.Query().Get("company"))
	if !ok {
		return
	}
	openID := r.PathValue("open_id")
	person, dir, ok := s.feishuUserFromDirectory(w, r, company, openID)
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
	previous := account.FeishuOpenID
	if company.Identity {
		if account.FeishuOpenID != "" && account.FeishuOpenID != openID {
			writeAPIError(w, domain.ErrConflict(
				"账户「"+account.Name+"」已绑定另一个飞书身份：请先解绑，再重新绑定"))
			return
		}
	} else {
		// Another company maps people through link rows. One account may be one person in each
		// company (the same human in two customers' directories), but never two in one.
		links, err := gate.People.ListFeishuPersonLinks(r.Context())
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		for _, link := range links {
			if link.AppID == company.AppID && link.AccountID == account.ID && link.OpenID != openID {
				writeAPIError(w, domain.ErrConflict(
					"账户「"+account.Name+"」在「"+company.Name+"」已经对应另一个人（"+link.Name+"）：请先解绑，再重新绑定"))
				return
			}
		}
	}
	if company.Identity {
		if err := gate.Accounts.BindAccountFeishu(r.Context(), account.ID, domain.FeishuBinding{
			OpenID: person.OpenID, UnionID: person.UnionID, Name: person.Name, BoundBy: gate.Actor,
		}); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	} else if err := gate.People.UpsertFeishuPersonLink(r.Context(), domain.FeishuPersonLink{
		AppID: company.AppID, OpenID: person.OpenID, UnionID: person.UnionID,
		Name: person.Name, AccountID: account.ID, BoundBy: gate.Actor,
	}); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	nodeIDs, createdNodes, warnings, err := ensureUserDepartmentNodes(r.Context(), company, gate, dir, person)
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
		"company": company.AppID,
	}, "ok")
	if len(nodeIDs) > 0 || createdNodes > 0 {
		s.reload(r.Context(), "feishu user joined org nodes", true)
	}
	s.invalidateFeishuDirectory()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "account": map[string]any{"id": account.ID, "name": account.Name},
		"open_id": person.OpenID, "company": feishuCompanyJSON(company, nil),
		"org_node_ids": nodeIDs, "warnings": warnings,
	})
}

// handleAdminUnbindAccountFeishuUser clears one person's account mapping for one company. It is
// idempotent and never touches the other companies' mappings — nor, for a client company, the
// account's own identity, which is what the portal login reads.
func (s *Server) handleAdminUnbindAccountFeishuUser(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openFeishuDirectoryGate(w, r)
	if !ok {
		return
	}
	company, ok := s.resolveFeishuCompany(w, r.URL.Query().Get("company"))
	if !ok {
		return
	}
	openID := r.PathValue("open_id")
	if !company.Identity {
		changed, err := gate.People.DeleteFeishuPersonLink(r.Context(), company.AppID, openID)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		if changed {
			s.audit(r.Context(), gate.Actor, "feishu_unbind", "feishu_person", openID,
				map[string]any{"company": company.AppID, "open_id": openID}, "ok")
		}
		s.invalidateFeishuDirectory()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "unbound": changed, "company": feishuCompanyJSON(company, nil),
		})
		return
	}
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
			map[string]any{"open_id": openID, "account_name": account.Name, "company": company.AppID}, "ok")
	}
	s.invalidateFeishuDirectory()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unbound": changed, "account_id": account.ID})
}
