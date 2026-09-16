package proxy

import (
	"errors"
	"path/filepath"
	"strings"

	"github.com/winger/ai-gateway/internal/dshgw/aigw"
	"github.com/winger/ai-gateway/internal/dshgw/config"
	"github.com/winger/ai-gateway/internal/dshgw/securefile"
)

type KeySource interface{ Key(string) (string, error) }
type FileKeySource struct{ Root string }

func (s FileKeySource) Key(tenant string) (string, error) {
	if !config.ValidTenantName(tenant) {
		return "", errors.New("invalid tenant name")
	}
	path := filepath.Join(s.Root, tenant, "gateway.key")
	if err := securefile.CheckPermissions(path, 0o640); err != nil {
		return "", err
	}
	data, err := securefile.ReadLimitedRegular(path, 8193)
	if err != nil {
		return "", err
	}
	return aigw.NormalizeKey(strings.TrimSpace(string(data)))
}
