# provider-codex (reference plugin)

Wraps a **subscription-backed Responses endpoint** (ChatGPT/Codex style) as a gateway
plugin. It is a reference implementation, not a supported integration.

> **Unofficial and unsupported.** It talks to endpoints that are not part of any
> public API, may change or break without notice, and using them may conflict with
> the upstream provider's terms of service. Review those terms yourself before
> enabling it, and keep it disabled unless you have decided the risk is yours.

## How it authenticates

Credentials are resolved in this order, so the most self-maintaining one wins:

| Mode | Credential | How a token is obtained | Renewal |
|---|---|---|---|
| C | `refresh_token` | POST to the OAuth token endpoint (`grant_type=refresh_token`) | automatic |
| A | `session_cookie` | GET `https://chatgpt.com/api/auth/session` with the browser cookie | automatic while the cookie lives |
| B | `access_token` | used as-is | none |

`client_id`, `token_endpoint` and `account_id` can be overridden in the credentials when
the upstream expects different values. `token_file` reads a CLI-style auth file once
(for example a local `auth.json` containing `tokens.access_token` and
`tokens.refresh_token`) and copies it into the plugin's own state directory.

Tokens live in `$GW_PLUGIN_STATE_DIR/session.json` (mode 0600) and are replaced
atomically. When the token endpoint returns a **rotated** refresh token it is written
back immediately: without that write the next refresh would fail with `invalid_grant`.
Concurrent requests share a single refresh.

## Configuration

```json
{
  "base_url": "https://chatgpt.com/backend-api/codex",
  "account_id": "",
  "reasoning_effort": "medium",
  "store": false,
  "health_path": "/me",
  "models": [
    {"id": "gpt-5-codex", "upstream_model": "gpt-5-codex", "context_window": 272000, "max_output_tokens": 128000,
     "capabilities": {"stream": true, "tools": true, "reasoning": true}}
  ]
}
```

## Actions (admin console → Providers → detail)

| Action | What it does |
|---|---|
| `whoami` | reports mode, expiry and which credentials are present (never their values) |
| `refresh_session` | forces a refresh now and reports the new expiry |
| `set_token` | replaces stored credentials: `{access_token?, refresh_token?, session_cookie?, account_id?}` |

## Behaviour notes

- The upstream only offers streaming, so `Complete` is implemented by consuming the
  same stream: there is one event-translation path, not two.
- Usage is reported with cache hits split out and reasoning tokens separate, which is
  what the pricing engine needs to apply a different rate.
- 401/403 → fatal `token_expired` (no failover: another provider would fail the same
  way, this is a credential problem); 429 → quota exhaustion with the `Retry-After`
  deadline; 5xx → retryable; 4xx → fatal with the upstream code.

## Build and use

```sh
source scripts/goenv.sh
go build -o plugins/provider-codex ./examples/provider-codex
```

Then create a provider with kind `plugin:provider-codex`, keep it **disabled** until the
credentials are in place, run the `whoami` action to confirm the mode and expiry, and
only then enable it.
