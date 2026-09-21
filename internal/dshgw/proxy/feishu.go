package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/feishu"
)

// feishuTicketCookieName is where aigw puts the login ticket. It is read here rather than
// taken from the query when both are present: the cookie is the one the browser received on
// its way back from aigw, and a mismatch means the URL was tampered with.
const feishuTicketCookieName = "aigw_dshgw_ticket"

// feishuLoginURL is the browser-visible address of aigw's login entry, or "" when the
// feature is off (which is also what hides the portal button).
func (p *Proxy) feishuLoginURL() string {
	if p.Feishu == nil || !p.Feishu.Enabled {
		return ""
	}
	return strings.TrimSpace(p.Feishu.AigwLoginURL)
}

// feishuLogin redeems the ticket aigw issued for one identity and starts that tenant's
// session — the same cookie, the same TTL and the same request-time revalidation as a key
// login, because everything after "who is this" is deliberately identical.
func (p *Proxy) feishuLogin(w http.ResponseWriter, r *http.Request) {
	if p.Feishu == nil || !p.Feishu.Enabled || !p.Feishu.Verifier.Enabled() {
		p.audit(r, "", "feishu_login_reject", "feishu disabled", http.StatusNotFound)
		http.NotFound(w, r)
		return
	}
	if err := p.checkEdgeOrigin(r, p.Config.ExpectedOrigin(p.Config.PortalPort), false); err != nil {
		p.audit(r, "", "feishu_login_reject", err.Error(), http.StatusForbidden)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ticket, err := p.feishuTicket(r)
	if err != nil {
		// The refusal reason decides the wording: "start again" for an expired or replayed
		// link, "invalid" for anything that does not verify at all.
		reason := "ticket"
		var refusal *feishu.Error
		if errors.As(err, &refusal) {
			reason = refusal.Reason
		}
		p.audit(r, "", "feishu_login_reject", err.Error(), http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, feishuErrorMessage(reason))
		return
	}
	// The ticket names a tenant, and the tenant must exist here: a binding that points at a
	// tenant this gateway does not serve is a deployment mistake, not a login.
	tenant, ok := p.Registry.Get(ticket.Tenant)
	if !ok {
		p.audit(r, "", "feishu_login_reject", "unknown tenant", http.StatusForbidden)
		p.renderLogin(w, http.StatusForbidden, feishuErrorMessage("tenant"))
		return
	}
	// aigw already checked the account; this asks the same question of the tenant's worker key
	// so that "the account was disabled a minute ago" is honoured here too, with the same
	// fail-closed semantics as every other authorization check in this gateway.
	if err := p.recheckFeishuAccess(r, tenant.Name); err != nil {
		var denial *aigw.DSHDenial
		switch {
		case errors.As(err, &denial):
			reason := "dsh disabled"
			message := feishuErrorMessage("dsh_disabled")
			if denial.Reason == "account_status" {
				reason, message = "account suspended", feishuErrorMessage("account_status")
			}
			p.audit(r, tenant.Name, "feishu_login_reject", reason, http.StatusForbidden)
			p.renderLogin(w, http.StatusForbidden, message)
		case errors.Is(err, aigw.ErrInvalidKey):
			p.audit(r, tenant.Name, "feishu_login_reject", "worker key revoked", http.StatusForbidden)
			p.renderLogin(w, http.StatusForbidden, feishuErrorMessage("revoked"))
		default:
			p.log().Warn("the Feishu login authorization check failed", "tenant", tenant.Name, "err", err)
			p.audit(r, tenant.Name, "feishu_login_reject", "authorization unavailable", http.StatusServiceUnavailable)
			p.renderLogin(w, http.StatusServiceUnavailable, feishuErrorMessage("unavailable"))
		}
		return
	}
	// Same lifecycle moment as a key login (M69): re-apply the platform slice of this tenant's
	// dsh configuration and make sure its worker is up. A Feishu login carries no key, so the
	// platform slice comes from the tenant's stored worker key alone.
	p.prepareLogin(r, tenant, "")
	token, err := p.Sessions.Issue(tenant.Name, p.Config.SessionTTL.Duration())
	if err != nil {
		p.log().Error("issue dshgw session failed", "tenant", tenant.Name, "err", err)
		p.renderLogin(w, http.StatusInternalServerError, "无法创建会话")
		return
	}
	// The ticket already proved WHICH Feishu person this is; the name behind it is what the
	// tenant's sidebar shows, so warm the identity while the browser is redirected (M67).
	p.identity(r.Context(), tenant)
	p.setSessionCookie(w, tenant.Name, token, false)
	p.audit(r, tenant.Name, "feishu_login_success", "authenticated", http.StatusFound)
	if p.Activity != nil {
		if err := p.Activity.MarkLogin(tenant.Name, p.now()); err != nil {
			p.log().Error("persist login activity failed", "tenant", tenant.Name, "err", err)
		}
	}
	http.Redirect(w, r, p.Config.WithTrailingSlash(p.Config.TenantOrigin(tenant.Name)), http.StatusFound)
}

// recheckFeishuAccess asks aigw whether the tenant's account may still use the dsh gateway,
// using the tenant's own worker key. It reuses the interval/per-request caching the account
// switch already documents, so a revocation takes effect within the configured window.
func (p *Proxy) recheckFeishuAccess(r *http.Request, tenant string) error {
	if p.KeySource == nil {
		return errors.New("feishu login is enabled but no gateway key source is configured")
	}
	if p.Authorizer == nil {
		return errors.New("feishu login is enabled but no authorizer is configured")
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.Config.ValidateTimeout.Duration())
	defer cancel()
	key, err := p.KeySource.Key(tenant)
	if err != nil {
		return err
	}
	_, err = p.Authorizer.Authorize(ctx, key)
	return err
}

// feishuTicket picks the ticket from the cookie, falling back to the query for a deployment
// whose portal is on another host (where a cookie set by aigw would never arrive). Two
// different values is a tampered request, not a preference question.
func (p *Proxy) feishuTicket(r *http.Request) (feishu.Ticket, error) {
	var zero feishu.Ticket
	fromCookie := ""
	for _, cookie := range r.Cookies() {
		if cookie.Name == feishuTicketCookieName && cookie.Value != "" {
			if fromCookie != "" {
				return zero, errors.New("duplicate ticket cookies")
			}
			fromCookie = cookie.Value
		}
	}
	fromQuery := r.URL.Query().Get("ticket")
	switch {
	case fromCookie != "" && fromQuery != "" && fromCookie != fromQuery:
		return zero, errors.New("ticket mismatch")
	case fromCookie != "":
		return p.Feishu.Verifier.Verify(fromCookie)
	case fromQuery != "":
		return p.Feishu.Verifier.Verify(fromQuery)
	default:
		return zero, errors.New("missing ticket")
	}
}

// feishuErrorMessage turns a refusal into something the person can act on. The distinction
// that matters to them is "this account is not set up yet" versus "start again" versus
// "the gateway cannot check right now".
func feishuErrorMessage(reason string) string {
	switch reason {
	case "unbound":
		return "该飞书账号尚未绑定任何 API Key，无法登录。请先用 API Key 登录，或联系管理员在 aigw 控制台完成绑定。"
	case "dsh_disabled":
		return "该账号未启用 dsh，请联系管理员在 aigw 控制台启用。"
	case "account_status":
		return "该账号已停用，无法登录 dsh。"
	case "tenant_missing":
		return "该账号的 dsh 租户尚未就绪，请联系管理员检查租户状态。"
	case "tenant":
		return "登录票据指向的租户在本网关上不存在，请联系管理员。"
	// The ticket verifier's own vocabulary. A person must never read "bad signature" or
	// "malformed": those describe bytes, not what happened to them.
	case "ticket", "malformed", "bad signature", "unsupported version", "unknown mode", "incomplete ticket":
		return "登录链接无效，请重新点击「飞书登录」。"
	case "expired", "expiry beyond the accepted window":
		return "飞书授权已过期，请重新点击「飞书登录」。"
	case "already used":
		return "这个登录链接已经使用过了，请重新点击「飞书登录」。"
	case "revoked":
		return "该租户的凭据已被吊销，请联系管理员。"
	case "unavailable":
		return "认证服务暂不可用，请稍后再试；API Key 登录不受影响。"
	case "cancelled":
		return "已取消飞书授权，未登录。"
	case "no_app_permission":
		return "你在飞书侧没有该应用的使用权限，请联系飞书管理员。"
	case "rate_limited":
		return "操作过于频繁，请稍后再试。"
	case "app_error", "invalid", "error", "":
		return "飞书登录未能完成，请重试；若持续失败请联系管理员。"
	default:
		return "飞书登录未能完成（" + reason + "），请重试或联系管理员。"
	}
}
