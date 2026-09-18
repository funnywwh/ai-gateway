// Command dshgw is the standalone multi-tenant DeepSeek Harness gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

var (
	version  = "dev"
	revision = "none"
	date     = "unknown"
)

type cli struct {
	stdin      io.Reader
	stdout     io.Writer
	stderr     io.Writer
	configPath string
}

func main() { os.Exit(execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
func execute(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("dshgw", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "/etc/dshgw/config.yaml", "path to strict YAML configuration")
	showVersion := flags.Bool("version", false, "print version information")
	flags.Usage = func() { printUsage(stderr) }
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "dshgw %s (revision %s, built %s)\n", version, revision, date)
		return 0
	}
	rest := flags.Args()
	if len(rest) == 0 {
		printUsage(stderr)
		return 2
	}
	app := &cli{stdin: stdin, stdout: stdout, stderr: stderr, configPath: *configPath}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var err error
	switch rest[0] {
	case "serve":
		cancel()
		err = app.serve()
	case "admin-serve":
		cancel()
		err = app.adminServe(context.Background())
	case "tenant":
		err = app.tenant(ctx, rest[1:])
	case "bind":
		err = app.bind(rest[1:])
	case "login-url":
		err = app.loginURL(rest[1:])
	case "sync-models":
		err = app.syncModels(ctx, rest[1:])
	case "revalidate":
		err = app.revalidate(ctx, rest[1:])
	case "capture-url":
		err = app.captureURL(ctx, rest[1:])
	case "sandbox-exec":
		err = app.sandboxExec(rest[1:])
	case "contract":
		cancel()
		err = app.contract(context.Background(), rest[1:])
	case "doctor":
		err = app.doctor(ctx, rest[1:])
	case "backup":
		err = app.backup(ctx, rest[1:])
	case "help", "--help", "-h":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "dshgw: unknown command %q\n", rest[0])
		printUsage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "dshgw:", redactError(err))
		if exitCodeOf(err) == 2 || strings.HasPrefix(err.Error(), "usage:") {
			return 2
		}
		return 1
	}
	return 0
}
func printUsage(w io.Writer) {
	fmt.Fprint(w, `usage: dshgw [--config PATH] COMMAND [OPTIONS]

Commands:
  serve                                  run loopback gateway
  admin-serve                            root-only local tenant provisioning channel (UNIX socket; requires admin_socket config)
  tenant create|list|rotate-key|restart|remove
  bind PREFIX TENANT                     bind an additional public key prefix
  login-url [PREFIX]                    print the portal or tenant URL
  sync-models TENANT                     refresh DSH models from aigw
  revalidate [TENANT]                    validate stored tenant key(s)
  capture-url TENANT                     print the worker startup URL the runner captured
  sandbox-exec [--print] TENANT          run a tenant worker inside its bubblewrap profile (worker unit ExecStart)
  contract [all|dsh|aigw]                run external integration contracts
  doctor                                 validate deployment invariants
  backup                                 create a mode-0600 backup archive

API keys are read from --key-file or standard input and are never accepted as argv values.
`)
}

var sensitiveErrorTokenRE = regexp.MustCompile(`(?i)(Bearer[ \t]+|token=|sk-)[^ \t\r\n]+`)

func redactError(err error) string {
	return sensitiveErrorTokenRE.ReplaceAllStringFunc(err.Error(), func(value string) string {
		lower := strings.ToLower(value)
		switch {
		case strings.HasPrefix(lower, "bearer "):
			return value[:7] + "[REDACTED]"
		case strings.HasPrefix(lower, "token="):
			return value[:6] + "[REDACTED]"
		default:
			return "[REDACTED]"
		}
	})
}

type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }
func exitCodeOf(err error) int {
	var coded *codedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}
