package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/funnywwh/ai-gateway/internal/config"
	"github.com/funnywwh/ai-gateway/internal/domain"
	"github.com/funnywwh/ai-gateway/internal/feishu"
)

// The write half of the company registry (M93): onboarding a client company, rotating its secret,
// pausing it, and removing the registration.
//
// Rules that hold across all of these:
//
//   - the identity application and the companies written into feishu.companies are configuration, so
//     they are refused here with a message naming the setting (the operator either edits the file or
//     registers the company in the console instead);
//   - an app id is never changed after creation: stored organization data refers to it;
//   - a secret is sealed before it is stored, is never echoed, and the audit entry records only that
//     it changed;
//   - deleting removes the registration, not the data.

// companyWriteGate is the shared pre-flight of the four write endpoints.
type companyWriteGate struct {
	Store FeishuCompanyAdmin
	Actor string
}

func (s *Server) openCompanyWriteGate(w http.ResponseWriter, r *http.Request) (companyWriteGate, bool) {
	if !s.feishuEnabled() {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return companyWriteGate{}, false
	}
	actor, ok := s.adminActor(w, r, true)
	if !ok {
		return companyWriteGate{}, false
	}
	store, ok := portReady(w, s.deps.FeishuApps, "the feishu company registry")
	if !ok {
		return companyWriteGate{}, false
	}
	return companyWriteGate{Store: store, Actor: actor.Username}, true
}

// requireSecrets refuses a write that would store a secret on a deployment that cannot seal one.
// The message names both ways out, because "you cannot add a company here" is not an answer.
func (s *Server) requireSecrets(w http.ResponseWriter) bool {
	if s.secretsReady() {
		return true
	}
	writeAPIError(w, domain.ErrInvalidRequest(
		"部署未配置 credentials_key，无法在控制台保存公司密钥；请配上它，或改用 feishu.companies 配置文件登记（那种方式在控制台只读）"))
	return false
}

// normalizeCompanyInput validates the operator's input with the same rules the configuration file
// uses (a name becomes an org node name; an app id has one shape), and returns the trimmed values.
func normalizeCompanyInput(name, appID, rootNode string) (string, string, string, error) {
	cleanName, err := config.NormalizeCompanyName("name", name)
	if err != nil {
		return "", "", "", domain.ErrInvalidRequest(err.Error())
	}
	appID = strings.TrimSpace(appID)
	if !config.ValidFeishuAppID(appID) {
		return "", "", "", domain.ErrInvalidRequest("app_id must be that company's own App ID (cli_…)")
	}
	rootNode = strings.TrimSpace(rootNode)
	if rootNode != "" {
		if _, err := domain.NormalizeOrgNodeName(rootNode); err != nil {
			return "", "", "", domain.ErrInvalidRequest("root_node " + err.Error())
		}
	}
	return cleanName, appID, rootNode, nil
}

// companyNameTaken reports whether another company already uses that name, counting both sources.
// It is the check the two unique indexes cannot do alone, since half the registry lives in a file.
func (s *Server) companyNameTaken(ctx context.Context, name string, exceptID int64) (string, error) {
	rows, err := s.feishuCompanyRows(ctx)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Name != name || (row.ID != 0 && row.ID == exceptID) {
			continue
		}
		if row.Source == "console" {
			return "控制台里已有同名公司 #" + strconv.FormatInt(row.ID, 10), nil
		}
		if row.Identity {
			return "公司名与 feishu.company_name（本公司）相同", nil
		}
		return "公司名与 feishu.companies 里的一家公司相同", nil
	}
	return "", nil
}

// companyAppIDTaken reports whether another company already claims that app id.
func (s *Server) companyAppIDTaken(ctx context.Context, appID string, exceptID int64) (string, error) {
	rows, err := s.feishuCompanyRows(ctx)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.AppID != appID || (row.ID != 0 && row.ID == exceptID) {
			continue
		}
		if row.Source == "console" {
			return "控制台里已有公司 #" + strconv.FormatInt(row.ID, 10) + " 使用这个 App ID", nil
		}
		if row.Identity {
			return "这个 App ID 就是本部署的身份应用 feishu.app_id", nil
		}
		return "这个 App ID 已写在 feishu.companies 里", nil
	}
	return "", nil
}

// handleAdminCreateFeishuCompany registers one client company.
func (s *Server) handleAdminCreateFeishuCompany(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openCompanyWriteGate(w, r)
	if !ok {
		return
	}
	var body struct {
		Name      string `json:"name"`
		AppID     string `json:"app_id"`
		AppSecret string `json:"app_secret"`
		RootNode  string `json:"root_node"`
		Note      string `json:"note"`
		Enabled   *bool  `json:"enabled"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	name, appID, rootNode, err := normalizeCompanyInput(body.Name, body.AppID, body.RootNode)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if strings.TrimSpace(body.AppSecret) == "" {
		writeAPIError(w, domain.ErrInvalidRequest("app_secret is required: 没有密钥就没法读这家公司的通讯录"))
		return
	}
	if !s.requireSecrets(w) {
		return
	}
	ctx := r.Context()
	if who, err := s.companyAppIDTaken(ctx, appID, 0); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	} else if who != "" {
		writeAPIError(w, domain.ErrConflict(who))
		return
	}
	if who, err := s.companyNameTaken(ctx, name, 0); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	} else if who != "" {
		writeAPIError(w, domain.ErrConflict(who))
		return
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	app := &domain.FeishuApp{
		Name: name, AppID: appID, RootNode: rootNode, Note: strings.TrimSpace(body.Note),
		Enabled: enabled, CreatedBy: gate.Actor, UpdatedBy: gate.Actor,
	}
	// Two steps, and on purpose: the sealing AAD binds the row id, so the row has to exist before its
	// secret can be sealed against it. A failed seal removes the row again — a company without a
	// usable secret would only show up as a broken row on the next page load.
	id, err := gate.Store.UpsertFeishuApp(ctx, app)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	sealed, err := s.sealCompanySecret(id, body.AppSecret)
	if err != nil {
		if _, cleanupErr := gate.Store.DeleteFeishuApp(ctx, id); cleanupErr != nil && s.deps.Log != nil {
			s.deps.Log.Warn("removing a company row whose secret failed to seal failed", "id", id, "err", cleanupErr)
		}
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	app.ID, app.SecretEnc = id, sealed
	if _, err := gate.Store.UpsertFeishuApp(ctx, app); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	s.audit(ctx, gate.Actor, "create", "feishu_company", strconv.FormatInt(id, 10), map[string]any{
		"name": name, "app_id": appID, "root_node": rootNode, "enabled": enabled, "secret_set": true,
	}, "ok")
	s.invalidateFeishuDirectory()

	status, payload := s.companyPagePayload(ctx, id, http.StatusCreated)
	writeJSON(w, status, payload)
}

// sealCompanySecret seals one secret for the row it belongs to, with an error an operator can act on.
func (s *Server) sealCompanySecret(appRowID int64, secret string) ([]byte, error) {
	seals, ok := portReadyNoWrite(s.deps.FeishuAppSecrets)
	if !ok || !seals.Ready() {
		return nil, errors.New("credentials_key is not configured")
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, errors.New("app_secret is empty")
	}
	sealed, err := seals.Seal(appRowID, secret)
	if err != nil {
		return nil, err
	}
	return sealed, nil
}

// handleAdminUpdateFeishuCompany edits one registration: name, root node, note, enabled flag, and
// optionally a new secret. The app id is not editable — it is the key the imported data carries.
func (s *Server) handleAdminUpdateFeishuCompany(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openCompanyWriteGate(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid company id"))
		return
	}
	ctx := r.Context()
	app, err := gate.Store.GetFeishuApp(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if who, err := s.companyAppIDTaken(ctx, app.AppID, id); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	} else if who != "" {
		// A configured company shares this app id: editing the row changes nothing the sync sees.
		writeAPIError(w, domain.ErrConflict(
			"这条登记被配置覆盖（"+who+"）：请改配置，或删除这条登记后按需要重新登记"))
		return
	}
	var body struct {
		Name      *string `json:"name"`
		RootNode  *string `json:"root_node"`
		Note      *string `json:"note"`
		Enabled   *bool   `json:"enabled"`
		AppSecret *string `json:"app_secret"`
		AppID     *string `json:"app_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}
	if body.AppID != nil && strings.TrimSpace(*body.AppID) != app.AppID {
		writeAPIError(w, domain.ErrInvalidRequest(
			"app_id 不可更改：它是已导入的节点与人员映射的归属键。要换公司就新建一家，同步后清理旧公司（…/links + 删节点）再删除这条登记"))
		return
	}
	changes := map[string]any{}
	if body.Name != nil {
		name, _, _, err := normalizeCompanyInput(*body.Name, app.AppID, "")
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		if who, err := s.companyNameTaken(ctx, name, id); err != nil {
			writeAPIError(w, toAPIError(err))
			return
		} else if who != "" {
			writeAPIError(w, domain.ErrConflict(who))
			return
		}
		changes["name"] = name
		app.Name = name
	}
	if body.RootNode != nil {
		rootNode := strings.TrimSpace(*body.RootNode)
		if rootNode != "" {
			if _, err := domain.NormalizeOrgNodeName(rootNode); err != nil {
				writeAPIError(w, domain.ErrInvalidRequest("root_node " + err.Error()))
				return
			}
		}
		changes["root_node"] = rootNode
		app.RootNode = rootNode
	}
	if body.Note != nil {
		changes["note"] = strings.TrimSpace(*body.Note)
		app.Note = strings.TrimSpace(*body.Note)
	}
	if body.Enabled != nil {
		changes["enabled"] = *body.Enabled
		app.Enabled = *body.Enabled
	}
	secretChanged := false
	if body.AppSecret != nil && strings.TrimSpace(*body.AppSecret) != "" {
		if !s.requireSecrets(w) {
			return
		}
		sealed, err := s.sealCompanySecret(app.ID, *body.AppSecret)
		if err != nil {
			writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
			return
		}
		app.SecretEnc = sealed
		secretChanged = true
		// The value itself never reaches the audit trail: "who rotated the key when" is the fact
		// worth keeping, the key is not.
		changes["secret_changed"] = true
	}
	app.UpdatedBy = gate.Actor
	if _, err := gate.Store.UpsertFeishuApp(ctx, app); err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if len(changes) == 0 {
		writeAPIError(w, domain.ErrInvalidRequest("nothing to update: 至少给一个字段（name/root_node/note/enabled/app_secret）"))
		return
	}
	s.audit(ctx, gate.Actor, "update", "feishu_company", strconv.FormatInt(id, 10), changes, "ok")
	if secretChanged || changes["enabled"] != nil {
		s.invalidateFeishuDirectory()
	}
	status, payload := s.companyPagePayload(ctx, id, http.StatusOK)
	writeJSON(w, status, payload)
}

// handleAdminDeleteFeishuCompany removes the registration. The organization data it brought in stays
// — deleting the registration and deleting the imported tree are separate decisions, which is why
// the response reports what is still there.
func (s *Server) handleAdminDeleteFeishuCompany(w http.ResponseWriter, r *http.Request) {
	gate, ok := s.openCompanyWriteGate(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, domain.ErrInvalidRequest("invalid company id"))
		return
	}
	ctx := r.Context()
	app, err := gate.Store.GetFeishuApp(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	nodes, links := 0, 0
	if counter, ready := portReadyNoWrite(s.deps.FeishuApps); ready {
		nodes, links, err = counter.CountFeishuAppData(ctx, app.AppID)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
	}
	deleted, err := gate.Store.DeleteFeishuApp(ctx, id)
	if err != nil {
		writeAPIError(w, toAPIError(err))
		return
	}
	if deleted {
		s.audit(ctx, gate.Actor, "delete", "feishu_company", strconv.FormatInt(id, 10), map[string]any{
			"name": app.Name, "app_id": app.AppID, "company_nodes_kept": nodes, "person_links_kept": links,
		}, "ok")
	}
	s.invalidateFeishuDirectory()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "deleted": deleted, "id": id, "name": app.Name, "app_id": app.AppID,
		"company_nodes": nodes, "linked_accounts": links,
		"note": "只删了登记：组织节点、成员关系与人员映射都保留。" +
			"要清数据用 DELETE /admin/api/v1/org/feishu/companies/{app_id}/links 删映射、" +
			"DELETE /admin/api/v1/org/nodes/{id}?cascade=true 删节点",
	})
}

// handleAdminProbeFeishuCompany tests one company's credentials without storing anything: a bounded
// directory read that answers "wrong secret", "missing permissions" or "readable" in two or three
// Feishu calls. It is the cheapest way to catch the two failures that actually happen.
func (s *Server) handleAdminProbeFeishuCompany(w http.ResponseWriter, r *http.Request) {
	if !s.feishuEnabled() {
		writeAPIError(w, domain.ErrUnsupported("feishu is not enabled on this deployment"))
		return
	}
	if _, ok := s.adminActor(w, r, true); !ok {
		return
	}
	var body struct {
		ID        *int64 `json:"id"`
		AppID     string `json:"app_id"`
		AppSecret string `json:"app_secret"`
	}
	if err := decodeOptionalJSON(r, &body); err != nil {
		writeAPIError(w, domain.ErrInvalidRequest(err.Error()))
		return
	}

	appID := strings.TrimSpace(body.AppID)
	secret := strings.TrimSpace(body.AppSecret)
	switch {
	case body.ID != nil && *body.ID > 0:
		store, ok := portReady(w, s.deps.FeishuApps, "the feishu company registry")
		if !ok {
			return
		}
		app, err := store.GetFeishuApp(r.Context(), *body.ID)
		if err != nil {
			writeAPIError(w, toAPIError(err))
			return
		}
		if !s.secretsReady() {
			writeProbeVerdict(w, probeVerdict{
				Stage: "credentials",
				Message: "部署未配置 credentials_key，无法解密已保存的密钥；" +
					"要么配上它，要么在请求里直接带上 app_id + app_secret",
			})
			return
		}
		seals, _ := portReadyNoWrite(s.deps.FeishuAppSecrets)
		opened, err := seals.Open(app.ID, app.SecretEnc)
		if err != nil || strings.TrimSpace(opened) == "" {
			writeProbeVerdict(w, probeVerdict{
				Stage:   "credentials",
				Message: "已保存的 App Secret 无法解密（credentials_key 可能已轮换）：请在控制台重新填写",
			})
			return
		}
		appID, secret = app.AppID, opened
	case appID == "" || secret == "":
		writeAPIError(w, domain.ErrInvalidRequest(
			"probe needs either id (use the stored secret) or app_id + app_secret"))
		return
	}
	if !config.ValidFeishuAppID(appID) {
		writeProbeVerdict(w, probeVerdict{Stage: "credentials", Message: "App ID 形状不对：应该是 cli_…"})
		return
	}

	client := feishu.NewCompanyClient(s.feishuConfig(), appID, secret)
	// A bounded walk: the answer needs one page of departments, not the whole tenant (the full walk
	// costs about two calls per department).
	dir, err := client.Directory(r.Context(), feishu.DirectoryOptions{MaxDepartments: 6, MaxPages: 2})
	if err != nil {
		verdict, upstream := probeVerdictFromError(err)
		if upstream != nil {
			writeAPIError(w, upstream)
			return
		}
		writeProbeVerdict(w, verdict)
		return
	}
	if !dir.NamesAvailable {
		writeProbeVerdict(w, probeVerdict{
			Stage: "names",
			Message: "能读到部门，但部门与人员没有名称：这个应用缺「获取部门基础信息」「获取用户基本信息」" +
				"两个只读权限（或权限变更没有发布版本）。加权限并发布后重试。",
			DepartmentsSeen: len(dir.Departments),
		})
		return
	}
	writeProbeVerdict(w, probeVerdict{
		OK:              true,
		Stage:           "ok",
		Message:         "可读：密钥有效，部门名称也能读到",
		DepartmentsSeen: len(dir.Departments),
		NamesAvailable:  true,
		Samples:         directorySamples(&dir, 3),
	})
}

// probeVerdict is the answer of a probe: a verdict, not an error, because "the secret is wrong" is
// exactly the information the operator asked for.
type probeVerdict struct {
	OK              bool
	Stage           string
	Message         string
	DepartmentsSeen int
	NamesAvailable  bool
	Samples         []map[string]any
}

func writeProbeVerdict(w http.ResponseWriter, verdict probeVerdict) {
	payload := map[string]any{
		"ok":               verdict.OK,
		"stage":            verdict.Stage,
		"message":          verdict.Message,
		"departments_seen": verdict.DepartmentsSeen,
		"names_available":  verdict.NamesAvailable,
	}
	if verdict.Samples != nil {
		payload["samples"] = verdict.Samples
	}
	writeJSON(w, http.StatusOK, payload)
}

// probeVerdictFromError separates "Feishu answered and told us why" (a verdict) from "we could not
// talk to Feishu at all" (an upstream error the caller reports as a 502, like every other read).
func probeVerdictFromError(err error) (probeVerdict, *domain.APIError) {
	var feishuErr *feishu.Error
	if !errors.As(err, &feishuErr) {
		return probeVerdict{}, feishuUpstreamError(err)
	}
	switch feishuErr.Kind {
	case feishu.KindCredentials:
		return probeVerdict{Stage: "credentials",
			Message: "App ID / App Secret 被飞书拒绝：请核对开发者后台「凭证与基础信息」里的值（详情见网关日志）"}, nil
	case feishu.KindAppUnavailable:
		return probeVerdict{Stage: "permission",
			Message: "飞书拒绝了这次读取：多半是应用的可用范围/权限没配对，或改动没发布版本（详情见网关日志）"}, nil
	case feishu.KindRateLimited:
		return probeVerdict{Stage: "rate_limited",
			Message: "飞书限流：稍后重试（目录读取有 60 秒缓存，控制台的同步不受影响）"}, nil
	default:
		return probeVerdict{}, feishuUpstreamError(err)
	}
}

// directorySamples renders the first departments of a probe as id/name pairs.
func directorySamples(dir *feishu.Directory, limit int) []map[string]any {
	out := []map[string]any{}
	for _, department := range dir.Departments {
		if len(out) >= limit {
			break
		}
		out = append(out, map[string]any{"id": department.ID, "name": department.Name})
	}
	return out
}

// companyPagePayload re-reads one company through the same merge the list uses, so a write answers
// with exactly the row the console will render next (counts, root plan and warnings included).
func (s *Server) companyPagePayload(ctx context.Context, id int64, status int) (int, map[string]any) {
	rows, err := s.feishuCompanyRows(ctx)
	if err != nil {
		return status, map[string]any{"ok": true, "id": id}
	}
	for i := range rows {
		if rows[i].ID != id {
			continue
		}
		row := rows[i]
		return status, map[string]any{
			"ok": true, "id": id, "company": map[string]any{
				"id": id, "app_id": row.AppID, "name": row.Name, "root_node": row.RootName,
				"enabled": row.Enabled, "note": row.Note,
				"secret_configured": row.SecretConfigured, "client_ready": row.Client != nil,
				"client_error": row.ClientErr, "warnings": row.Warnings,
			},
		}
	}
	return status, map[string]any{"ok": true, "id": id}
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
	company, ok := s.resolveFeishuCompany(w, r, strings.TrimSpace(r.PathValue("app_id")))
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
