package proxy

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/feishu"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// The key picker (M72) is the step between "who you are" and a session, for an account whose
// keys are all equivalent for authorization but not for the audit trail.
//
// Why the portal owns the page: the identity proof arrives here as a one-time ticket, and this
// process already renders the login form — so the choice is one more form, not another service.
// aigw mints the ticket (mode "keypick", naming an ACCOUNT rather than a tenant), the key list
// comes from aigw's authorize answer for a tenant's own worker key, and the submitted id is
// validated against that list. The form is a request, never a claim.

// pickPath is the picker's route under the portal path prefix, like every other portal route.
const pickPath = "login/pick"

// pickTicketCookie is the cookie aigw sets for a Feishu login's pick ticket (M72). It is the same
// name as the login ticket's because the mode inside the signed payload is what tells the two
// apart: one cookie name, one reader, no way to confuse them by looking in the wrong place.
const pickTicketCookie = feishuTicketCookieName

// pickPage is the picker, rendered as plain HTML with no script: the form posts back to the same
// path and the portal CSP already allows a form action to 'self'. Radio buttons because exactly
// one key has to be chosen.
var pickPage = template.Must(template.New("pick").Parse(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>选择 Key</title><style>body{font:16px system-ui;max-width:34rem;margin:10vh auto;padding:1rem;background:#101318;color:#eef}main{background:#1b2028;padding:2rem;border-radius:12px}button{box-sizing:border-box;width:100%;padding:.8rem;margin:.6rem 0;cursor:pointer}label.key{display:block;padding:.6rem;margin:.4rem 0;background:#232a34;border-radius:8px;cursor:pointer}label.key span.name{font-weight:600}label.key span.meta{display:block;color:#bcc6d6;font-size:.85rem}label.key input{margin-right:.6rem}.error{color:#ff9b9b}.note{color:#bcc6d6;font-size:.9rem}a{color:#8ab4ff}</style></head><body><main><h1>选择一把 Key</h1>{{if .Error}}<p class="error">{{.Error}}</p>{{end}}{{if .Account}}<p class="note">账号「{{.Account}}」有 {{len .Keys}} 把可用的 Key。选中的那把只用于记录这次登录（归属与审计）；模型额度按账号计算，不会因为选了哪把而改变。</p>{{end}}{{if .Keys}}<form method="post" action="{{.PortalPath}}{{.PickPath}}"><input type="hidden" name="ticket" value="{{.Ticket}}">{{range .Keys}}<label class="key"><input type="radio" name="key_id" value="{{.ID}}"{{if .Checked}} checked{{end}}><span class="name">{{.Name}}</span><span class="meta">{{.Prefix}}{{if .LastUsed}} · 最近使用 {{.LastUsed}}{{end}}</span></label>{{end}}<button type="submit">用这把 Key 登录</button></form>{{else}}<p class="error">这个账号没有可用的 Key：请联系管理员在 aigw 控制台签发或启用一把。</p>{{end}}<p class="note"><a href="{{.PortalPath}}">返回登录页</a></p></main></body></html>`))

// pickKey is one radio button's worth of data: what a person recognises, never key material.
type pickKey struct {
	ID       int64
	Name     string
	Prefix   string
	LastUsed string
	Checked  bool
}

// pickView is the picker's template data.
type pickView struct {
	Account string
	Keys    []pickKey
	Error   string
}

// errAccountUnavailable means no tenant this gateway serves belongs to the account the pick
// ticket names: a deployment whose tenant was removed, or a ticket from another deployment.
var errAccountUnavailable = errors.New("no tenant is served for this account")

// offerKeyPick sends the browser to the picker after a key login that has to choose (M72).
//
// The ticket is minted HERE rather than by aigw, because a key login is decided here: this
// process admitted the key and this process has to ask the question. It is signed with the same
// ticket key aigw's tickets are verified with, and it goes into the URL — the page is served by
// the very process that created it, so there is nothing to hand over between hosts.
func (p *Proxy) offerKeyPick(w http.ResponseWriter, r *http.Request, tenant string) {
	if p.Feishu == nil || !p.Feishu.Enabled || p.Feishu.Verifier == nil || !p.Feishu.Verifier.Enabled() {
		// Without a signer there is no picker to offer; the caller's fallback signs the person
		// straight in, which is what happened before M72.
		p.log().Warn("a key login needed the picker but the Feishu handoff is unavailable", "tenant", tenant)
		return
	}
	accountID := p.accountIDForKey(r.Context(), tenant)
	if accountID == 0 {
		p.log().Warn("a key login needed the picker but the account could not be resolved", "tenant", tenant)
		return
	}
	ticket, err := p.Feishu.Verifier.SignPick(accountID, "key:"+strconv.FormatInt(accountID, 10))
	if err != nil {
		p.log().Warn("minting a key-pick ticket failed", "tenant", tenant, "err", err)
		return
	}
	http.Redirect(w, r, p.pickURL(ticket), http.StatusSeeOther)
}

// pickURL is the picker's absolute path on the portal origin, with the ticket in the query.
func (p *Proxy) pickURL(ticket string) string {
	return p.Config.WithTrailingSlash(p.Config.PortalPath()) + pickPath + "?ticket=" + ticket
}

// accountIDForKey resolves which account a tenant's worker key belongs to, which is the only way
// a key login can address the picker (aigw's answer carries the id, M72).
func (p *Proxy) accountIDForKey(ctx context.Context, tenant string) int64 {
	if p.KeySource == nil {
		return 0
	}
	key, err := p.KeySource.Key(tenant)
	if err != nil || strings.TrimSpace(key) == "" {
		return 0
	}
	identity, err := p.aigwIdentity(ctx, key)
	if err != nil {
		return 0
	}
	return identity.AccountID
}

// pickHandler serves the picker: GET renders the form from a pick ticket, POST validates the
// submitted key id against the account's live keys and signs the person in.
func (p *Proxy) pickHandler(w http.ResponseWriter, r *http.Request) {
	if p.Feishu == nil || !p.Feishu.Enabled || p.Feishu.Verifier == nil || !p.Feishu.Verifier.Enabled() {
		http.NotFound(w, r)
		return
	}
	if err := p.checkEdgeOrigin(r, p.Config.ExpectedOrigin(p.Config.PortalPort), false); err != nil {
		p.audit(r, "", "login_reject", err.Error(), http.StatusForbidden)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	wire, err := p.pickTicket(w, r)
	if err != nil {
		p.audit(r, "", "feishu_login_reject", "key pick: "+err.Error(), http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, feishuErrorMessage("ticket"))
		return
	}
	// Rendering the form peeks at the ticket; submitting it consumes the nonce, so a replay (back
	// button, a double submit, a shared URL) fails the second time while a refresh of the form
	// still works.
	verify := p.Feishu.Verifier.PeekPick
	if r.Method == http.MethodPost {
		verify = p.Feishu.Verifier.VerifyPick
	}
	ticket, err := verify(wire)
	if err != nil {
		reason := "ticket"
		var refusal *feishu.Error
		if errors.As(err, &refusal) {
			reason = refusal.Reason
		}
		p.audit(r, "", "feishu_login_reject", "key pick: "+err.Error(), http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, feishuErrorMessage(reason))
		return
	}
	// Which tenant, and which keys, is decided by aigw, asked with the tenant's own worker key —
	// the same credential every other authorization check uses. The ticket says which account may
	// enter; this call is what proves that is still true right now, and it is also what creates
	// the tenant on a first login (dshgw.auto_enable).
	tenant, keys, err := p.pickContext(r.Context(), ticket)
	if err != nil {
		p.renderPickFailure(w, r, ticket, err)
		return
	}
	if r.Method != http.MethodPost {
		p.renderPick(w, http.StatusOK, pickView{Account: tenant.Account, Keys: keys}, wire)
		return
	}
	chosen, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("key_id")), 10, 64)
	if err != nil {
		p.renderPick(w, http.StatusBadRequest, pickView{Account: tenant.Account, Keys: keys, Error: "请选择一把 Key"}, wire)
		return
	}
	selected := selectKey(keys, chosen)
	if selected == nil {
		// The form is not trusted: an id that is not in the freshly fetched list (a key revoked
		// in the meantime, another account's key, a hand-made post) is refused without saying
		// which of those it was.
		p.audit(r, tenant.Name, "feishu_login_reject", "key pick: unknown key id", http.StatusForbidden)
		p.renderPick(w, http.StatusForbidden, pickView{Account: tenant.Account, Keys: keys,
			Error: "这把 Key 现在不可用（可能已被停用）：请另选一把，或返回登录页重新登录。"}, wire)
		return
	}
	p.issueTenantSession(w, r, tenant, "", "feishu_login_success", selected.ID, selected.Name)
}

// pickContext resolves the ticket's account to a tenant, and that tenant's usable keys.
//
// The picker runs before a tenant is entered, so the ticket names no tenant: the gateway asks
// aigw about the key of every tenant it serves until one answers with this account id. That is a
// handful of local calls — the key comes from disk, the answer from aigw — and it is what keeps
// the pick ticket free of topology.
func (p *Proxy) pickContext(ctx context.Context, ticket feishu.Ticket) (registry.Tenant, []pickKey, error) {
	if p.KeySource == nil {
		return registry.Tenant{}, nil, errors.New("the key picker is enabled but no gateway key source is configured")
	}
	if ticket.AccountID == 0 {
		return registry.Tenant{}, nil, errAccountUnavailable
	}
	var lastErr error
	for _, tenant := range p.Registry.List() {
		key, err := p.KeySource.Key(tenant.Name)
		if err != nil || strings.TrimSpace(key) == "" {
			continue
		}
		identity, err := p.aigwIdentity(ctx, key)
		if err != nil {
			// An unavailable aigw is not "this account is unknown": remember it and keep looking,
			// then answer with the authorization error rather than with "not served", which would
			// send the person to their administrator for the wrong reason.
			lastErr = err
			continue
		}
		if identity.AccountID != ticket.AccountID {
			continue
		}
		return tenant, pickKeys(identity.Keys), nil
	}
	if lastErr != nil {
		return registry.Tenant{}, nil, lastErr
	}
	return registry.Tenant{}, nil, errAccountUnavailable
}

// pickKeys maps aigw's key list onto the template's shape, dropping anything without an id.
func pickKeys(keys []aigw.KeyRef) []pickKey {
	out := make([]pickKey, 0, len(keys))
	for _, key := range keys {
		if key.ID == 0 {
			continue
		}
		out = append(out, pickKey{ID: key.ID, Name: key.Name, Prefix: key.KeyPrefix, LastUsed: key.LastUsedAt})
	}
	return out
}

// selectKey returns the chosen key when it is in the list, or nil.
func selectKey(keys []pickKey, id int64) *pickKey {
	for i := range keys {
		if keys[i].ID == id {
			return &keys[i]
		}
	}
	return nil
}

// renderPick renders the picker with the portal's own headers (no-store, the portal CSP). A
// single-key list (the account lost keys while the page was open) preselects it, so submitting
// the form still works.
func (p *Proxy) renderPick(w http.ResponseWriter, status int, view pickView, ticket string) {
	p.setPortalHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if len(view.Keys) == 1 {
		view.Keys[0].Checked = true
	}
	_ = pickPage.Execute(w, struct {
		pickView
		PortalPath string
		PickPath   string
		Ticket     string
	}{view, p.Config.PortalPath(), pickPath, ticket})
}

// renderPickFailure turns a resolver failure into an answer a person can act on. A ticket for an
// account this gateway does not serve is a deployment mismatch; a denial is the account's own
// state; everything else is "try again", with the reason in the log rather than on the page.
func (p *Proxy) renderPickFailure(w http.ResponseWriter, r *http.Request, ticket feishu.Ticket, err error) {
	var denial *aigw.DSHDenial
	switch {
	case errors.Is(err, errAccountUnavailable):
		p.audit(r, "", "feishu_login_reject", "key pick: account not served", http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, feishuErrorMessage("tenant"))
	case errors.As(err, &denial) && denial.Reason == "provision_failed":
		// Entitled but the tenant could not be created: the portal cannot fix that, and
		// "not enabled" would send the person to the wrong place.
		p.audit(r, "", "feishu_login_reject", "key pick: provisioning failed", http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden,
			"该账号的 DSH 租户尚未就绪（自动创建失败）：请联系管理员在 aigw 控制台检查该账号的模型授权。")
	case errors.As(err, &denial):
		reason := "dsh_disabled"
		if denial.Reason == "account_status" {
			reason = "account_status"
		}
		p.audit(r, "", "feishu_login_reject", "key pick: "+denial.Reason, http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, feishuErrorMessage(reason))
	case errors.Is(err, aigw.ErrInvalidKey):
		p.audit(r, "", "feishu_login_reject", "key pick: worker key revoked", http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, feishuErrorMessage("revoked"))
	default:
		p.log().Warn("resolving the key picker failed", "account", ticket.AccountID, "err", err)
		p.audit(r, "", "feishu_login_reject", "key pick: authorization unavailable", http.StatusServiceUnavailable)
		p.renderLogin(w, http.StatusServiceUnavailable, feishuErrorMessage("unavailable"))
	}
}

// pickTicket reads the pick ticket from where the flow left it. Two shapes exist, and both are
// legitimate: a Feishu login gets it as a host-only cookie (aigw runs the callback and cannot
// put it anywhere else — cookies are scoped to a host), while a key login's picker is a redirect
// this process produced, so the ticket is in the URL. The form then carries it in the body.
//
// Two different values is a tampered request, not a preference question — the same rule the
// login ticket follows.
func (p *Proxy) pickTicket(w http.ResponseWriter, r *http.Request) (string, error) {
	fromCookie := ""
	for _, cookie := range r.Cookies() {
		if cookie.Name == pickTicketCookie && cookie.Value != "" {
			if fromCookie != "" {
				return "", errors.New("duplicate pick ticket cookies")
			}
			fromCookie = cookie.Value
		}
	}
	fromQuery := strings.TrimSpace(r.URL.Query().Get("ticket"))
	fromBody := ""
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			return "", errors.New("malformed form")
		}
		fromBody = strings.TrimSpace(r.PostForm.Get("ticket"))
	}
	present := []string{}
	for _, value := range []string{fromCookie, fromQuery, fromBody} {
		if value != "" {
			present = append(present, value)
		}
	}
	if len(present) == 0 {
		return "", errors.New("missing pick ticket")
	}
	for _, value := range present[1:] {
		if value != present[0] {
			return "", errors.New("pick ticket mismatch")
		}
	}
	return present[0], nil
}
