package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/funnywwh/ai-gateway/internal/config"
	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/feishu"
	"github.com/funnywwh/ai-gateway/internal/orgtree"
)

// The company registry of the Feishu organization sync.
//
// Two sources, one list: the deployment's own application and any company written into
// feishu.companies come from the configuration (M92, read-only here), and the console-managed
// companies come from feishu_apps (M93). The merge key is the app id — it is what the stored
// organization data refers to — and the configuration wins when both sides carry the same one,
// because the file is the operator's explicit, reviewable statement.
//
// Everything downstream (the sync, the directory read, the per-person endpoints) resolves a company
// through this file and then talks to that company's own client. The facts a management page needs
// — names, counts, warnings — live here too, so exactly one place knows what "a company" is.

// feishuCompanyRow is one company as the console and the sync see it.
type feishuCompanyRow struct {
	feishu.Company
	// ID is the console row id; 0 for the identity application and for companies that exist only in
	// the configuration (there is nothing to update or delete there).
	ID int64
	// Source is "identity", "config" or "console".
	Source string
	// Enabled false keeps the company and all its data but refuses to resolve it.
	Enabled bool
	Note    string
	// NameSource says where the effective name came from (M94): "row" (a console row's own name),
	// "override" (the console edited a configuration-sourced company) or "config".
	NameSource string
	// Override is what the console changed about a configuration-sourced company (M96). Its zero value
	// means "nothing was overridden"; the row already carries the resulting values.
	Override domain.FeishuCompanyOverride
	// SecretConfigured reports whether a secret is stored (console rows) — never the secret.
	SecretConfigured bool
	// ClientErr explains why no client could be built (an undecryptable secret, usually after a
	// credentials_key rotation). Callers must not use Client when this is set.
	ClientErr string
	Warnings  []string
	// ShadowedByConfig marks a console row whose app id is also configured: the configuration wins,
	// so this row is redundant. Its id is still reported so the console can clean it up.
	ShadowedByConfig bool
}

// configFeishuCompanies is the configuration half of the registry: the identity application first,
// then feishu.companies in file order. A deployment whose dependency list predates M92 (an older
// test fixture) has no list at all and is treated as the single-company deployment M70 supported.
func configFeishuCompanies(deps *FeishuDeps) []feishu.Company {
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

// feishuCompanyRows merges configuration and console rows. A missing registry port (a deployment or
// fixture that has not wired it) simply leaves the configuration half.
func (s *Server) feishuCompanyRows(ctx context.Context) ([]feishuCompanyRow, error) {
	configured := configFeishuCompanies(s.deps.Feishu)
	store, storeReady := portReadyNoWrite(s.deps.FeishuApps)
	// The console's field overrides (M94/M96) apply to configuration-sourced companies, which have no
	// row to edit. A failure to read them costs the overrides, not the list: the configured values are
	// always a usable answer.
	overrides := map[string]domain.FeishuCompanyOverride{}
	if storeReady {
		stored, err := store.ListFeishuCompanyOverrides(ctx)
		if err != nil {
			if s.deps.Log != nil {
				s.deps.Log.Warn("reading feishu company overrides failed", "err", err)
			}
		} else {
			overrides = stored
		}
	}

	cfg := s.feishuConfig()
	rows := make([]feishuCompanyRow, 0, len(configured)+2)
	byAppID := map[string]int{}
	byName := map[string]int{}
	for i := range configured {
		company := configured[i]
		source := "config"
		if company.Identity {
			source = "identity"
		}
		row := feishuCompanyRow{
			Company: company, Source: source, Enabled: true, SecretConfigured: true,
			NameSource: "config",
		}
		if override, ok := overrides[company.AppID]; ok {
			row.Override = override
			// Overrides win over the file, field by field: they are the operator's newer decision made
			// where the company is managed (M96).
			if override.Name != nil {
				row.Name = *override.Name
				row.NameSource = "override"
			}
			// The company node's name: an override wins, else an explicitly configured root_node
			// (a local node name the file states), else the effective company name. The identity
			// application has no explicit root_node — its feishu.Company.RootName is just the
			// company name — so a rename moves it, which is the whole point (M94).
			switch {
			case override.RootNode != nil:
				row.RootName = *override.RootNode
				if row.RootName == "" {
					row.RootName = row.Name
				}
			case configuredExplicitRoot(cfg, company.AppID) != "":
				row.RootName = configuredExplicitRoot(cfg, company.AppID)
			default:
				row.RootName = row.Name
			}
			if override.Note != nil {
				row.Note = *override.Note
			}
			if override.Enabled != nil {
				row.Enabled = *override.Enabled
			}
			// A client company's secret may be overridden too; the identity application's never is
			// (the API layer refuses it), so this only ever rebuilds a client company's client.
			if len(override.SecretEnc) > 0 {
				row.Client, row.ClientErr = s.feishuClientForOverride(company.AppID, override.SecretEnc)
				row.SecretConfigured = row.Client != nil
			}
		}
		rows = append(rows, row)
		byAppID[company.AppID] = len(rows) - 1
		if _, dup := byName[row.Name]; !dup {
			byName[row.Name] = len(rows) - 1
		}
	}

	if !storeReady {
		return rows, nil
	}
	apps, err := store.ListFeishuApps(ctx)
	if err != nil {
		return nil, err
	}
	for _, app := range apps {
		row := feishuCompanyRow{
			Company: feishu.Company{
				AppID: app.AppID, Name: app.Name, RootName: app.CompanyNodeName(),
			},
			ID: app.ID, Source: "console", Enabled: app.Enabled, Note: app.Note,
			SecretConfigured: app.HasSecret(), NameSource: "row",
		}
		if idx, shadowed := byAppID[app.AppID]; shadowed {
			// Same application on both sides: one company. The configuration is authoritative, and
			// the console shows the row as redundant instead of hiding it (otherwise the operator
			// would see a database row nowhere, with no way to remove it).
			rows[idx].ShadowedByConfig = true
			rows[idx].Warnings = append(rows[idx].Warnings,
				"shadowed_by_config: 控制台还有一条相同 app_id 的登记 #"+strconv.FormatInt(app.ID, 10)+"，它被配置覆盖（可在控制台删除）")
			continue
		}
		if _, dup := byName[app.Name]; dup {
			// A different application with the same company name: `company` accepts either, so the
			// name can only resolve to one of them — the configured one, which is also the older,
			// explicit statement. The console row stays reachable by app id.
			row.Warnings = append(row.Warnings,
				"duplicate_name: 公司名与配置里的另一家公司重复；按名字解析会落到配置那家，请改名或用 app_id")
		}
		client, clientErr := s.feishuClientForApp(app)
		row.Client, row.ClientErr = client, clientErr
		rows = append(rows, row)
		if _, dup := byName[app.Name]; !dup {
			byName[app.Name] = len(rows) - 1
		}
	}
	return rows, nil
}

// feishuCompanyRowsOrConfig is the display-path variant: a failed registry read costs a warning in
// the log and a configuration-only answer, because a missing company label must never turn an
// otherwise fine page into an error.
func (s *Server) feishuCompanyRowsOrConfig(ctx context.Context) []feishuCompanyRow {
	rows, err := s.feishuCompanyRows(ctx)
	if err == nil {
		return rows
	}
	if s.deps.Log != nil {
		s.deps.Log.Warn("listing console-managed feishu companies failed", "err", err)
	}
	configured := configFeishuCompanies(s.deps.Feishu)
	out := make([]feishuCompanyRow, 0, len(configured))
	for i := range configured {
		source := "config"
		if configured[i].Identity {
			source = "identity"
		}
		out = append(out, feishuCompanyRow{Company: configured[i], Source: source, Enabled: true})
	}
	return out
}

// feishuClientForOverride builds the directory-read client of a company whose secret the console
// overrode (M96). The secret is opened per call and never kept; the resulting client has its own
// tenant-token cache, which is the price of letting the console change a secret without a restart —
// and the directory itself is cached for 60 s, so the extra token mints are bounded.
func (s *Server) feishuClientForOverride(appID string, ciphertext []byte) (*feishu.Client, string) {
	secrets, ready := portReadyNoWrite(s.deps.FeishuAppSecrets)
	if !ready {
		return nil, "部署未配置 credentials_key，无法解密这家公司的密钥覆盖"
	}
	secret, err := secrets.OpenByApp(appID, ciphertext)
	if err != nil {
		return nil, "控制台保存的 App Secret 无法解密（credentials_key 可能已轮换）：请重新填写或「恢复配置值」"
	}
	if strings.TrimSpace(secret) == "" {
		return nil, "控制台保存的 App Secret 为空"
	}
	return feishu.NewCompanyClient(s.feishuConfig(), appID, secret), ""
}

// feishuClientForApp builds the directory-read client of one console row. The secret is decrypted
// here and nowhere else; a failure comes back as a reason string the console can show.
func (s *Server) feishuClientForApp(app *domain.FeishuApp) (*feishu.Client, string) {
	secrets, ready := portReadyNoWrite(s.deps.FeishuAppSecrets)
	if !ready {
		return nil, "部署未配置 credentials_key，无法解密这家公司的密钥"
	}
	if !app.HasSecret() {
		return nil, "还没有保存 App Secret：请在控制台编辑这家公司并填入"
	}
	secret, err := secrets.Open(app.ID, app.SecretEnc)
	if err != nil {
		return nil, "App Secret 无法解密（credentials_key 可能已轮换）：请重新填写"
	}
	if strings.TrimSpace(secret) == "" {
		return nil, "App Secret 为空：请重新填写"
	}
	return feishu.NewCompanyClient(s.feishuConfig(), app.AppID, secret), ""
}

// feishuConfig is the configuration block the company clients inherit (endpoints, timeout).
func (s *Server) feishuConfig() config.Feishu {
	if s.deps.Config == nil {
		return config.Feishu{}
	}
	return s.deps.Config.Feishu
}

// resolveFeishuCompany turns a request's `company` parameter (an app id or a company name) into one
// company, refusing the three states that must not silently fall back to another company: unknown,
// disabled, and "the secret cannot be used".
func (s *Server) resolveFeishuCompany(w http.ResponseWriter, r *http.Request, token string) (*feishu.Company, bool) {
	rows, err := s.feishuCompanyRows(r.Context())
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return nil, false
	}
	if len(rows) == 0 {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return nil, false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		// The identity application, i.e. the M70 behavior every existing caller relies on — including
		// the pause and credential guards, which apply to it like to any other company (M96 lets the
		// console pause it).
		return usableCompany(w, rows[0])
	}
	for i := range rows {
		row := rows[i]
		if row.AppID != token && row.Name != token {
			continue
		}
		return usableCompany(w, row)
	}
	writeAPIError(w, domain.ErrInvalidRequest(
		"unknown company "+token+"; known companies: "+describeCompanyRows(rows)))
	return nil, false
}

// usableCompany answers the two states that must not be resolved silently: a company the console
// paused, and one whose stored credentials cannot be used.
func usableCompany(w http.ResponseWriter, row feishuCompanyRow) (*feishu.Company, bool) {
	if !row.Enabled {
		writeAPIError(w, domain.ErrInvalidRequest(
			"公司「"+row.Name+"」已在控制台停用：启用后再同步（节点与人员映射都还在）"))
		return nil, false
	}
	if row.Client == nil {
		writeAPIError(w, domain.ErrInvalidRequest(
			"公司「"+row.Name+"」的凭据不可用："+row.ClientErr))
		return nil, false
	}
	return &row.Company, true
}

// describeCompanyRows lists the known companies for an error message: name (source) + app id, so an
// operator can copy whichever value the parameter accepts. No secrets are involved.
func describeCompanyRows(rows []feishuCompanyRow) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		label := row.Name
		switch {
		case row.Identity:
			label += " (identity)"
		case row.Source == "config":
			label += " (config)"
		}
		if !row.Enabled {
			label += "[disabled]"
		}
		parts = append(parts, label+"="+row.AppID)
	}
	return strings.Join(parts, ", ")
}

// feishuCompaniesForDisplay is the company list for payloads that only need names and labels. It
// includes disabled companies on purpose: a node's "which company is this" label must stay readable
// after the company was paused, and the account payload labels every mapping the same way.
func (s *Server) feishuCompaniesForDisplay(ctx context.Context) []feishu.Company {
	rows := s.feishuCompanyRowsOrConfig(ctx)
	out := make([]feishu.Company, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].Company)
	}
	return out
}

// companyNameMap maps every known app id to its display name. A failed registry read degrades to the
// configuration names (see feishuCompanyRowsOrConfig); an app id whose company is unknown stays in
// the map with an empty name, because the node list must still show which company owns the node.
func (s *Server) companyNameMap(ctx context.Context) map[string]string {
	rows := s.feishuCompanyRowsOrConfig(ctx)
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.AppID] = row.Name
	}
	return out
}

// overrideTime renders an override's timestamp, or nil when nothing was overridden.
func overrideTime(override domain.FeishuCompanyOverride) *time.Time {
	if override.Empty() || override.UpdatedAt.IsZero() {
		return nil
	}
	at := override.UpdatedAt
	return &at
}

// rootNameTaken reports the root-level node already wearing a company's name, when there is one.
// It is what the console shows next to a rename: the company node cannot be created while that node
// sits at the root level, and the operator has two documented ways out (sync first, or rename/move
// that node).
func rootNameTaken(root *companyRootPlan, nodes []*domain.OrgNode) any {
	if root == nil || root.NodeID != 0 {
		return nil
	}
	for _, node := range nodes {
		if node.ParentIDValue() == 0 && node.Name == root.Name {
			return map[string]any{"node_id": node.ID, "name": node.Name}
		}
	}
	return nil
}

// secretsReady reports whether the deployment can seal a company secret.
func (s *Server) secretsReady() bool {
	secrets, ok := portReadyNoWrite(s.deps.FeishuAppSecrets)
	return ok && secrets.Ready()
}

// ---------------------------------------------------------------------------
// company page: list
// ---------------------------------------------------------------------------

// handleAdminListFeishuCompanies answers the console's company page: which companies can be
// imported, where each one's node is, how much is already imported, and whether its secret is
// usable. It never returns secret material — only whether one is configured.
func (s *Server) handleAdminListFeishuCompanies(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return
	}
	// Any authenticated administrator may read the list (the route's role is viewer, not admin).
	// The session check belongs here, in the handler: the route table's Role is what `admin_describe`
	// and the MCP bridge match against, it is not HTTP middleware. Without this line the endpoint
	// answered anyone who could reach the port — found on the live deployment on 2026-09-28.
	if _, ok := s.adminActor(w, r, false); !ok {
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
	ctx := r.Context()
	rows, err := s.feishuCompanyRows(ctx)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if len(rows) == 0 {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return
	}
	nodes, err := orgStore.ListOrgNodes(ctx)
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
	// Distinct accounts per company: one account may map to people in several companies, and a count
	// of rows would then say "2 accounts" for one person.
	linksByApp := map[string]map[int64]bool{}
	if people, ready := portReadyNoWrite(s.deps.FeishuPeople); ready {
		links, err := people.ListFeishuPersonLinks(ctx)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		for _, link := range links {
			if linksByApp[link.AppID] == nil {
				linksByApp[link.AppID] = map[int64]bool{}
			}
			linksByApp[link.AppID][link.AccountID] = true
		}
	}
	identityLogins := 0
	for _, account := range allAccounts {
		if account.FeishuOpenID != "" {
			identityLogins++
		}
	}

	data := make([]map[string]any, 0, len(rows))
	identityAppID := ""
	for i := range rows {
		row := rows[i]
		root := planCompanyRoot(&row.Company, nodes, index)
		linked := len(linksByApp[row.AppID])
		if row.Identity {
			// The identity application's people are bound on the account itself (that column is the
			// portal login), so its mapping count comes from there.
			linked = identityLogins
			identityAppID = row.AppID
		}
		data = append(data, map[string]any{
			"id":                 jsonNilInt64(row.ID),
			"app_id":             row.AppID,
			"name":               row.Name,
			"identity":           row.Identity,
			"source":             row.Source,
			"enabled":            row.Enabled,
			"note":               row.Note,
			"secret_configured":  row.SecretConfigured,
			"client_ready":       row.Client != nil,
			"client_error":       row.ClientErr,
			"name_source":        row.NameSource,
			"overridden":         row.Override.Fields(),
			"overridden_by":      row.Override.UpdatedBy,
			"overridden_at":      timeOrNil(overrideTime(row.Override)),
			"secret_overridden":  len(row.Override.SecretEnc) > 0,
			"shadowed_by_config": row.ShadowedByConfig,
			"warnings":           row.Warnings,
			"root_node_id":       jsonNilInt64(root.NodeID),
			"root_node_name":     root.Name,
			"root_matched":       root.Matched,
			"root_will_create":   root.WillCreate,
			"root_blocked":       root.Blocked,
			// The root-level node that already wears this company's name (M94): the rename dialog
			// uses it to explain what the next sync would do, and how to avoid the clash.
			"root_name_taken":    rootNameTaken(root, nodes),
			"company_nodes":      nodesByApp[row.AppID],
			"linked_accounts":    linked,
		})
	}
	if identityAppID == "" {
		identityAppID = rows[0].AppID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":            data,
		"count":           len(data),
		"identity_app_id": identityAppID,
		// The console needs to know whether it may offer "new company" at all; when it may not, the
		// reason is the missing credentials_key (the interface says so).
		"secrets_ready": s.secretsReady(),
	})
}
