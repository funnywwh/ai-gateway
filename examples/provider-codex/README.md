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
  "health_prompt": "hi",
  "health_model": "",
  "models": [
    {"id": "gpt-5.6-luna", "upstream_model": "gpt-5.6-luna",
     "capabilities": {"stream": true, "tools": true}}
  ]
}
```

Model IDs are **not** interchangeable. On a ChatGPT-account subscription the backend answers
`The '<id>' model is not supported when using Codex with a ChatGPT account.` for ids such as
`gpt-5`, `gpt-5-codex`, `codex-mini-latest`, `o3` and `gpt-5.1-codex`, while `gpt-5.6-luna`
works. Beware: `GET /backend-api/codex/models?client_version=…` returns `{"models":[]}` for such
an account even when a model does work, so that endpoint cannot tell you whether the account is
entitled — exercise the model instead (the health probe does exactly that).

`health_model` defaults to the first entry of `models`; `health_prompt` defaults to `hi`.

The subscription backend also rejects `max_output_tokens` for **every** value
(`{"detail":"Unsupported parameter: max_output_tokens"}`), so the adapter does not forward it:
a client that sets a cap gets its request served without one rather than a hard 400. The
gateway's own in-flight reservation (`billing.reservation_mode`) still applies for accounting.

## Actions (admin console → Providers → detail)

| Action | What it does |
|---|---|
| `whoami` | reports mode, expiry and which credentials are present (never their values) |
| `refresh_session` | forces a refresh now and reports the new expiry |
| `set_token` | replaces stored credentials: `{access_token?, refresh_token?, session_cookie?, account_id?}` |

## Behaviour notes

- Usage is reported with cache hits split out and reasoning tokens separate, which is
  what the pricing engine needs to apply a different rate.
- The upstream only offers streaming, so `Complete` is implemented by consuming the
  same stream: there is one event-translation path, not two. It also insists on
  `stream: true` (`{"detail":"Stream must be set to true"}`), which the adapter always does.
- **The health probe is a real streaming completion** (it sends `health_prompt` to
  `health_model`), not a status ping: this backend has no trustworthy liveness endpoint —
  the `/me` style probe it used to call was answered by a Cloudflare challenge, so the
  console showed a working provider as broken. The probe counts as healthy only when the
  stream reaches a terminal usage event, so a truncated stream is not mistaken for health.
  It runs only when an operator probes the provider, and it spends a few tokens.
- Error envelopes are read from `error.message`, `detail` and `message`, in that order:
  the upstream uses the FastAPI `detail` shape for parameter and model errors, and dropping
  it turned an actionable message into a bare "upstream returned 400".
- A Cloudflare challenge (403 + `cf-mitigated: challenge`, or an HTML body) is reported as
  retryable `upstream_challenge`, **not** as `token_expired`: the credentials are fine, the
  egress is blocked, and calling it a credential problem sends operators off re-minting tokens.
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
