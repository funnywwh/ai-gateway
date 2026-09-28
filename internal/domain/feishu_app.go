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

// FeishuCompanyOverride is what the console changed about a company whose values come from the
// configuration (M95). A nil field means "not overridden: use the configured value"; an empty string
// is a legitimate override (an empty root_node means "name the company node after the company").
//
// SecretEnc is the one exception to the "nil = not overridden" reading: an empty blob means the same
// thing, because an unset secret cannot be expressed as a value. The deployment's own application is
// never stored here — its secret also drives the login flows.
type FeishuCompanyOverride struct {
	AppID     string
	Name      *string
	RootNode  *string
	Note      *string
	Enabled   *bool
	SecretEnc []byte
	UpdatedBy string
	UpdatedAt time.Time
}

// Fields lists the overridden field names, for the console's badge and the list payload. Secret
// material is never included, only the fact that it was overridden.
func (o FeishuCompanyOverride) Fields() []string {
	out := []string{}
	if o.Name != nil {
		out = append(out, "name")
	}
	if o.RootNode != nil {
		out = append(out, "root_node")
	}
	if o.Note != nil {
		out = append(out, "note")
	}
	if o.Enabled != nil {
		out = append(out, "enabled")
	}
	if len(o.SecretEnc) > 0 {
		out = append(out, "secret")
	}
	return out
}

// Empty reports whether nothing is overridden (the row should not exist).
func (o FeishuCompanyOverride) Empty() bool { return len(o.Fields()) == 0 }
