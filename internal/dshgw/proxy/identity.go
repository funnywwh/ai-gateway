package proxy

import (
	"context"
	"strings"
	"time"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/registry"
)

// AccountNamer is the optional half of the authorization client that names the person a key
// belongs to (M67). It is separate from DSHAuthorizer on purpose: the admission decision and
// the display of who was admitted have different consequences, so a client (or a test double)
// that can only answer the first one must still be usable, and the gateway then shows the
// tenant name instead of a person's.
type AccountNamer interface {
	Identity(ctx context.Context, key string) (aigw.Identity, error)
}

// identityTTL bounds how long a name may be shown after it was learned. A name is display
// data, so staleness is cheap: the alternative — one aigw round trip per request, on the
// path that also proxies the UI — is not.
const identityTTL = 5 * time.Minute

// tenantIdentity is what the tenant's sidebar shows about the signed-in person.
type tenantIdentity struct {
	Tenant     string
	Account    string
	FeishuName string
	Resolved   time.Time
}

// Name is the label the interface renders: the Feishu name when the person bound one, then
// the account's console name, then the tenant slug — the only one a hand-made tenant has.
func (i tenantIdentity) Name() string {
	for _, candidate := range []string{i.FeishuName, i.Account, i.Tenant} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// identity answers the identity of one tenant, resolving it at most once per TTL.
//
// The tenant name always comes from the registry, which is why this never fails: a person
// looking at their own sidebar must not see "unavailable" because a display lookup could not
// reach aigw. Everything beyond the tenant name is best effort and arrives on a later load.
func (p *Proxy) identity(ctx context.Context, tenant registry.Tenant) tenantIdentity {
	now := p.now()
	p.identityMu.Lock()
	cached, ok := p.identities[tenant.Name]
	p.identityMu.Unlock()
	if ok && now.Sub(cached.Resolved) < identityTTL {
		return cached
	}
	resolved := tenantIdentity{Tenant: tenant.Name, Account: tenant.Account, Resolved: now}
	// Ask aigw for the names: the tenant slug alone tells a person nothing, and the account
	// label recorded on the tenant is only a fallback for when this call cannot be made.
	// One round trip per tenant per TTL, whatever the cache already holds.
	if namer, ok := p.Authorizer.(AccountNamer); ok && p.KeySource != nil {
		if key, err := p.KeySource.Key(tenant.Name); err != nil {
			p.log().Warn("reading the tenant's key for its identity failed", "tenant", tenant.Name, "err", err)
		} else {
			lookupCtx, cancel := context.WithTimeout(ctx, p.Config.ValidateTimeout.Duration())
			identity, err := namer.Identity(lookupCtx, key)
			cancel()
			switch {
			case err != nil:
				p.log().Warn("resolving the tenant's identity failed; the sidebar keeps the tenant name",
					"tenant", tenant.Name, "err", err)
			default:
				if name := strings.TrimSpace(identity.Account); name != "" {
					resolved.Account = name
					p.rememberAccount(tenant.Name, name)
				}
				resolved.FeishuName = strings.TrimSpace(identity.FeishuName)
			}
		}
	}
	// A cached identity that could not be refreshed keeps its names: the alternative is a
	// sidebar that loses the person's name because aigw blinked.
	if ok && resolved.FeishuName == "" {
		resolved.FeishuName = cached.FeishuName
	}
	p.identityMu.Lock()
	p.identities[tenant.Name] = resolved
	p.identityMu.Unlock()
	return resolved
}

// rememberAccount writes a label learned from aigw onto the tenant record, so the next
// dshgw start (and any other reader of registry.json) has it without asking anyone.
func (p *Proxy) rememberAccount(tenant, account string) {
	stored, ok := p.Registry.Get(tenant)
	if !ok || stored.Account == account {
		return
	}
	if err := p.Registry.SetAccount(tenant, account); err != nil {
		p.log().Warn("recording the tenant's account label failed", "tenant", tenant, "err", err)
		return
	}
	if err := p.Registry.Save(); err != nil {
		p.log().Warn("saving the tenant registry after recording its account label failed", "tenant", tenant, "err", err)
	}
}

// forgetIdentities drops the identities of tenants that no longer exist, so a registry
// reload cannot leave a name attached to a name that may be reused by a new account.
func (p *Proxy) forgetIdentities(tenants map[string]int) {
	p.identityMu.Lock()
	defer p.identityMu.Unlock()
	for name := range p.identities {
		if _, exists := tenants[name]; !exists {
			delete(p.identities, name)
		}
	}
}
