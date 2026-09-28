package feishu

import (
	"fmt"
	"strings"
	"time"

	"github.com/funnywwh/ai-gateway/internal/config"
)

// Company is one Feishu enterprise whose contact directory this deployment reads (M92).
//
// Exactly one company is the identity application (Identity: true) — the one configured as
// feishu.app_id, which serves binding, DSH portal login and console login. The others exist so
// their organization structure can be imported: their people get an organization mapping, never
// a login, because this deployment's application is not installed in their tenant.
type Company struct {
	// AppID identifies the company in everything that is stored: scoping a node's department
	// link, a person mapping, an audit entry. The name is a label; the app id is the identity.
	AppID    string
	Name     string
	RootName string
	// Identity marks the deployment's own application (the only one that can write
	// accounts.feishu_*, and the only one whose people can sign in with Feishu).
	Identity bool
	// Client reads that company's directory. Each company has its own client, which also means
	// its own tenant-access-token cache — one company's token is never reused for another.
	Client *Client
}

// CompaniesFromConfig builds the company list of one deployment: the identity application first,
// then the configured extra companies in configuration order. Order is preserved rather than
// sorted so the console's dropdown and every log line stay reproducible for a given file.
//
// identityClient is the client the identity flows were already built with (feishu.New); passing
// it in keeps one token cache for that application instead of minting a second one.
func CompaniesFromConfig(cfg config.Feishu, identityName string, identityClient *Client) []Company {
	out := []Company{{
		AppID:    strings.TrimSpace(cfg.AppID),
		Name:     identityName,
		RootName: identityName,
		Identity: true,
		Client:   identityClient,
	}}
	for _, company := range cfg.Companies {
		appID := strings.TrimSpace(company.AppID)
		out = append(out, Company{
			AppID:    appID,
			Name:     strings.TrimSpace(company.Name),
			RootName: company.RootNodeName(),
			Client:   NewCompanyClient(cfg, appID, strings.TrimSpace(company.AppSecret)),
		})
	}
	return out
}

// NewCompanyClient builds the directory-read client of one company: its own credentials, the
// deployment's endpoints and timeout (a test points those at a stub, and a deployment never has
// to state them twice).
func NewCompanyClient(base config.Feishu, appID, secret string) *Client {
	return &Client{
		AppID:          strings.TrimSpace(appID),
		AppSecret:      strings.TrimSpace(secret),
		TenantTokenURL: strings.TrimSpace(base.TenantTokenURL),
		ContactURL:     strings.TrimSpace(base.ContactURL),
		Timeout:        time.Duration(base.TimeoutS) * time.Second,
	}
}

// FindCompany resolves a `company` parameter: an app id or a company name, exact after trimming.
// An empty token is the identity application, which is what keeps M70/M72 callers — scripts, MCP
// clients, the console's account binding — working unchanged.
func FindCompany(list []Company, token string) (*Company, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("this deployment has no Feishu company configured")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		for i := range list {
			if list[i].Identity {
				return &list[i], nil
			}
		}
		return &list[0], nil
	}
	for i := range list {
		if list[i].AppID == token || list[i].Name == token {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("unknown company %q; known companies: %s", token, DescribeCompanies(list))
}

// DescribeCompanies renders the company list for an error message or a log line: name and app id,
// so an operator can tell which value to pass. Secrets never appear here (there is nothing to
// redact: the list carries none).
func DescribeCompanies(list []Company) string {
	parts := make([]string, 0, len(list))
	for _, company := range list {
		label := company.Name
		if company.Identity {
			label += " (identity)"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", label, company.AppID))
	}
	return strings.Join(parts, ", ")
}
