package billing

import (
	"context"
	"testing"
	"time"

	"github.com/winger/ai-gateway/internal/domain"
)

// proxyStore embeds the store port and counts the calls the service makes. It proves
// the service talks to the port rather than to *store.DB: swapping in this wrapper (or
// any other implementation of ServiceStore) is enough to observe and replace behaviour.
type proxyStore struct {
	ServiceStore
	counts map[string]int
}

func newProxyStore(inner ServiceStore) *proxyStore {
	return &proxyStore{ServiceStore: inner, counts: map[string]int{}}
}

func (p *proxyStore) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	p.counts["GetAccount"]++
	return p.ServiceStore.GetAccount(ctx, id)
}

func (p *proxyStore) AppendLedger(ctx context.Context, entries []*domain.LedgerEntry) (int, error) {
	p.counts["AppendLedger"]++
	return p.ServiceStore.AppendLedger(ctx, entries)
}

func (p *proxyStore) GetBalance(ctx context.Context, accountID int64) (int64, error) {
	p.counts["GetBalance"]++
	return p.ServiceStore.GetBalance(ctx, accountID)
}

func TestServiceDependsOnlyOnItsPort(t *testing.T) {
	ctx := context.Background()
	db := newStore(t)
	accountID := seedAccount(t, db, 0)
	proxy := newProxyStore(db)
	service := NewService(ctx, proxy, ServiceConfig{Writer: Config{BatchSize: 2}, ReservationTTL: 0}, nil)
	t.Cleanup(func() { service.Close(time.Second) })

	if _, _, err := service.Grant(ctx, CreditRequest{
		AccountID: accountID, Kind: "topup", AmountMicros: 1000, RefID: "proxy_1",
	}); err != nil {
		t.Fatalf("grant through the proxy: %v", err)
	}
	for _, method := range []string{"GetAccount", "AppendLedger"} {
		if proxy.counts[method] == 0 {
			t.Errorf("the service did not go through the port method %s (counts: %v)", method, proxy.counts)
		}
	}

	balance, err := service.Balance(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if balance != 1000 {
		t.Fatalf("balance = %d, want 1000", balance)
	}
	if proxy.counts["GetBalance"] == 0 {
		t.Errorf("reading the balance bypassed the port: %v", proxy.counts)
	}
}
