package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/winger/ai-gateway/internal/config"
	"github.com/winger/ai-gateway/internal/mcpsrv"
	"github.com/winger/ai-gateway/internal/registry"
	"github.com/winger/ai-gateway/internal/store"
)

// runMCPServe speaks the MCP server protocol over stdio for one account.
//
// It exists so a local agent (Claude Desktop, an editor plugin, a shell script) can
// query the gateway without exposing an HTTP endpoint. The account is chosen on the
// command line instead of by token: whoever can run this binary already has the
// database. Every tool still enforces the account scope, and the tool set is exactly
// the one served over HTTP because both use the same mcpsrv.Service.
func runMCPServe(arguments []string) int {
	flags := flag.NewFlagSet("mcp-serve", flag.ContinueOnError)
	configPath := flags.String("config", "config.yaml", "path to the YAML configuration file")
	accountName := flags.String("account", "", "account name whose data may be queried (required)")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if strings.TrimSpace(*accountName) == "" {
		fmt.Fprintln(os.Stderr, "aigw mcp-serve: --account is required")
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aigw mcp-serve: configuration error:", err)
		return 2
	}
	ctx := context.Background()
	db, err := store.Open(ctx, cfg.Database)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aigw mcp-serve: open database:", err)
		return 1
	}
	defer func() { _ = db.Close() }()

	account, err := db.GetAccountByName(ctx, strings.TrimSpace(*accountName))
	if err != nil {
		fmt.Fprintln(os.Stderr, "aigw mcp-serve: account not found:", *accountName)
		return 1
	}
	reg := registry.New(db)
	if _, err := reg.Reload(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "aigw mcp-serve: registry reload:", err)
		return 1
	}
	service := mcpsrv.New(db, reg, mcpsrv.Config{
		MaxRows: cfg.MCP.MaxQueryRows, WindowDays: cfg.MCP.RequestWindowDays, Currency: cfg.Billing.Currency,
	})

	fmt.Fprintf(os.Stderr, "aigw mcp-serve: serving account %q over stdio (%d tools)\n",
		account.Name, len(service.Tools()))
	reader := bufio.NewReaderSize(os.Stdin, 1<<20)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()
	for {
		line, err := reader.ReadBytes(byte(0x0A))
		trimmed := strings.TrimSpace(string(line))
		if trimmed != "" {
			response := service.Handle(ctx, account.ID, []byte(trimmed))
			if response != nil {
				encoded, marshalErr := json.Marshal(response)
				if marshalErr != nil {
					fmt.Fprintln(os.Stderr, "aigw mcp-serve: encode response:", marshalErr)
				} else {
					_, _ = writer.Write(append(encoded, byte(0x0A)))
					_ = writer.Flush()
				}
			}
		}
		if err != nil {
			break // EOF or a read error ends the session
		}
	}
	return 0
}
