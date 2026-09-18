package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/winger/ai-gateway/internal/feishu"
)

type vector struct {
	Name        string `json:"name"`
	Secret      string `json:"secret"`
	Now         string `json:"now"`
	Wire        string `json:"wire,omitempty"`
	Ticket      *struct {
		Version   int    `json:"v"`
		Mode      string `json:"mode"`
		Tenant    string `json:"tenant"`
		KeyID     int64  `json:"key_id"`
		AccountID int64  `json:"account_id"`
		OpenID    string `json:"open_id"`
		Nonce     string `json:"nonce"`
		Expires   int64  `json:"exp"`
	} `json:"ticket,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func main() {
	secret := []byte("vector-secret-not-for-production")
	now := time.Unix(1758200000, 0).UTC()
	codec, err := feishu.NewTicketCodec(secret, 120*time.Second)
	if err != nil {
		panic(err)
	}
	codec.Now = func() time.Time { return now }

	out := []vector{}
	add := func(name string, t feishu.Ticket) {
		wire, err := feishu.TicketWire(secret, t)
		if err != nil {
			panic(err)
		}
		out = append(out, vector{Name: name, Secret: string(secret), Now: now.Format(time.RFC3339), Wire: wire})
	}
	add("valid", feishu.Ticket{Version: 1, Mode: feishu.TicketMode, Tenant: "alice",
		KeyID: 7, AccountID: 3, OpenID: "ou_alice", Nonce: "0123456789abcdef", Expires: now.Add(120 * time.Second).Unix()})
	add("expired", feishu.Ticket{Version: 1, Mode: feishu.TicketMode, Tenant: "alice",
		KeyID: 7, AccountID: 3, OpenID: "ou_alice", Nonce: "fedcba9876543210", Expires: now.Add(-time.Second).Unix()})
	add("future-expiry", feishu.Ticket{Version: 1, Mode: feishu.TicketMode, Tenant: "alice",
		KeyID: 7, AccountID: 3, OpenID: "ou_alice", Nonce: "aaaabbbbccccdddd", Expires: now.Add(time.Hour).Unix()})
	add("unknown-mode", feishu.Ticket{Version: 1, Mode: "session", Tenant: "alice",
		KeyID: 7, AccountID: 3, OpenID: "ou_alice", Nonce: "1111222233334444", Expires: now.Add(60 * time.Second).Unix()})
	add("wrong-version", feishu.Ticket{Version: 2, Mode: feishu.TicketMode, Tenant: "alice",
		KeyID: 7, AccountID: 3, OpenID: "ou_alice", Nonce: "5555666677778888", Expires: now.Add(60 * time.Second).Unix()})
	add("empty-tenant", feishu.Ticket{Version: 1, Mode: feishu.TicketMode, Tenant: "",
		KeyID: 7, AccountID: 3, OpenID: "ou_alice", Nonce: "9999aaaabbbbcccc", Expires: now.Add(60 * time.Second).Unix()})
	// A tampered wire: signed with the right key but a payload that was modified after
	// signing is caught by the signature; this vector is signed with a different key.
	add("other-key", feishu.Ticket{Version: 1, Mode: feishu.TicketMode, Tenant: "alice",
		KeyID: 7, AccountID: 3, OpenID: "ou_alice", Nonce: "ddddeeeeffff0000", Expires: now.Add(60 * time.Second).Unix()})

	// The last vector's signature must be produced with a different secret to be refusable.
	out[len(out)-1].Secret = "some-other-secret"

	doc := map[string]any{
		"_comment": "Shared ticket vectors (M60/M61). aigw signs tickets (internal/feishu) and the DSH gateway verifies them (internal/dshgw/feishu); the two implementations must agree bit for bit, so both tests read this file. Regenerate with: go run ./cmd/gen-feishu-vectors internal/dshgw/contract/testdata/feishu_ticket_vectors.json",
		"vectors":  out,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[1], append(data, '\n'), 0o644); err != nil {
		panic(err)
	}
	fmt.Println("wrote", os.Args[1])
}
