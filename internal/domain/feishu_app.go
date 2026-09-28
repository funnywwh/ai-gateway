package domain

import "time"

// FeishuApp is one client company's self-built Feishu application as the console manages it
// (M93): the row that turns "onboard a customer company" into a console action instead of an edit
// of the configuration file plus a restart.
//
// The deployment's own application is never one of these. It lives in feishu.app_id because it
// carries the callback URL and the signing keys of the login flows — configuration is where a
// deployment's identity belongs. The companies list merges both sources, configuration first.
type FeishuApp struct {
	ID int64
	// Name is the company's label in the console and the default name of its company node.
	Name string
	// AppID identifies the company in everything that is stored: which org nodes it brought in,
	// which person mappings belong to it, which audit entries are about it. It is unique among
	// console rows and is never changed after creation — changing it would orphan that data.
	AppID string
	// SecretEnc is that application's secret, sealed with credentials_key (AES-256-GCM, scoped
	// AAD). The plaintext exists only inside the request that wrote or reads it: it is never
	// returned by an API, never logged and never audited. Empty means "not configured yet".
	SecretEnc []byte
	// RootNode overrides the local company node's name (empty = Name).
	RootNode string
	Note     string
	// Enabled false keeps the row and all its data but drops the company out of the sync
	// dropdown and refuses to resolve it by app id or name.
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
	CreatedBy string
	UpdatedBy string
}

// CompanyNodeName is the name the local company node gets for this row.
func (a *FeishuApp) CompanyNodeName() string {
	if a == nil {
		return ""
	}
	if a.RootNode != "" {
		return a.RootNode
	}
	return a.Name
}

// HasSecret reports whether a secret is stored, without decrypting anything.
func (a *FeishuApp) HasSecret() bool { return a != nil && len(a.SecretEnc) > 0 }
