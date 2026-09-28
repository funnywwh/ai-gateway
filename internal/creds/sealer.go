package creds

import (
	"encoding/json"
	"fmt"
)

// Sealer adapts the AES-GCM helpers into a per-provider sealing port. It is the
// only thing the management API needs from this package, which keeps key handling
// out of the HTTP layer.
type Sealer struct {
	key []byte
}

// NewSealer wraps a derived key. An empty key yields a sealer that reports
// Ready() == false so callers can reject credential writes with a clear message.
func NewSealer(key []byte) *Sealer { return &Sealer{key: key} }

// Ready reports whether a usable key is configured.
func (s *Sealer) Ready() bool { return s != nil && len(s.key) > 0 }

// Seal encrypts a JSON credential object for one provider. Empty input clears
// the stored credentials (the caller distinguishes an omitted field from {}).
func (s *Sealer) Seal(providerID int64, plaintext []byte) ([]byte, error) {
	if !s.Ready() {
		return nil, fmt.Errorf("credentials_key is not configured")
	}
	if len(plaintext) > 0 {
		var probe map[string]any
		if err := json.Unmarshal(plaintext, &probe); err != nil {
			return nil, fmt.Errorf("credentials must be a JSON object")
		}
	}
	return Encrypt(s.key, providerID, plaintext)
}

// KeyNames decrypts a credential blob only to report which field names are set.
// Values are never returned to the caller; this backs the admin UI's
// "configured: api_key, base_url" hint.
func (s *Sealer) KeyNames(providerID int64, ciphertext []byte) []string {
	if !s.Ready() || len(ciphertext) == 0 {
		return nil
	}
	plaintext, err := Decrypt(s.key, providerID, ciphertext)
	if err != nil {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(plaintext, &obj); err != nil {
		return nil
	}
	names := make([]string, 0, len(obj))
	for name := range obj {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// companyScope namespaces the company secrets sealed by CompanySealer. It is a constant here so the
// scope is one value for every writer and reader of those rows.
const companyScope = "feishu_app"

// CompanySealer seals one console-managed company's Feishu app secret (M93).
//
// It is a second type rather than a second method on Sealer because the AAD differs: a company
// secret must not be openable with a provider's id (and the other way round), which is exactly
// what the scoped AAD buys. Secrets are sealed and opened per request — the plaintext never lands
// in a struct, a log line or a JSON payload.
type CompanySealer struct {
	key []byte
}

// NewCompanySealer wraps a derived key. An empty key yields a sealer that reports Ready() == false,
// which the management API turns into "configure credentials_key, or keep using feishu.companies".
func NewCompanySealer(key []byte) *CompanySealer { return &CompanySealer{key: key} }

// Ready reports whether a usable key is configured.
func (s *CompanySealer) Ready() bool { return s != nil && len(s.key) > 0 }

// Seal encrypts one company's app secret. Empty input clears the stored secret.
func (s *CompanySealer) Seal(appRowID int64, secret string) ([]byte, error) {
	if !s.Ready() {
		return nil, fmt.Errorf("credentials_key is not configured")
	}
	if appRowID <= 0 {
		return nil, fmt.Errorf("a company row id is required to seal its secret")
	}
	return EncryptScoped(s.key, companyScope, appRowID, []byte(secret))
}

// Open decrypts one company's app secret. A nil/empty blob answers ("", nil): "no secret stored" is a
// state the caller reports, not an error.
func (s *CompanySealer) Open(appRowID int64, ciphertext []byte) (string, error) {
	if !s.Ready() {
		return "", fmt.Errorf("credentials_key is not configured")
	}
	if len(ciphertext) == 0 {
		return "", nil
	}
	if appRowID <= 0 {
		return "", fmt.Errorf("a company row id is required to open its secret")
	}
	plaintext, err := DecryptScoped(s.key, companyScope, appRowID, ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
