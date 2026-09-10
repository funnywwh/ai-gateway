-- 0001_init.sql - initial schema for ai-gateway
-- Conventions: money in int64 micro-USD (*_micros), time in UTC unix seconds,
-- booleans as 0/1, JSON blobs as TEXT.

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS accounts (
    id                           INTEGER PRIMARY KEY AUTOINCREMENT,
    name                         TEXT    NOT NULL UNIQUE,
    billing_mode                 TEXT    NOT NULL DEFAULT 'postpaid',
    balance_micros               INTEGER NOT NULL DEFAULT 0,
    credit_limit_micros          INTEGER NOT NULL DEFAULT 0,
    low_balance_threshold_micros INTEGER NOT NULL DEFAULT 0,
    price_overrides_json         TEXT    NOT NULL DEFAULT '',
    markup_override_bp           INTEGER NOT NULL DEFAULT 0,
    auto_suspend                 INTEGER NOT NULL DEFAULT 0,
    auto_resume                  INTEGER NOT NULL DEFAULT 0,
    inflight_policy_override     TEXT    NOT NULL DEFAULT '',
    overdraft_limit_micros       INTEGER NOT NULL DEFAULT 0,
    status                       TEXT    NOT NULL DEFAULT 'active',
    note                         TEXT    NOT NULL DEFAULT '',
    created_at                   INTEGER NOT NULL DEFAULT 0,
    updated_at                   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS providers (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    name                 TEXT    NOT NULL UNIQUE,
    kind                 TEXT    NOT NULL,
    display_name         TEXT    NOT NULL DEFAULT '',
    config_json          TEXT    NOT NULL DEFAULT '',
    config_version       INTEGER NOT NULL DEFAULT 1,
    credentials_enc      BLOB,
    state_dir            TEXT    NOT NULL DEFAULT '',
    meta_json            TEXT    NOT NULL DEFAULT '',
    discovered_json      TEXT    NOT NULL DEFAULT '',
    health_json          TEXT    NOT NULL DEFAULT '',
    last_error           TEXT    NOT NULL DEFAULT '',
    enabled              INTEGER NOT NULL DEFAULT 1,
    priority             INTEGER NOT NULL DEFAULT 100,
    weight               INTEGER NOT NULL DEFAULT 100,
    max_inflight         INTEGER NOT NULL DEFAULT 0,
    timeout_overrides    TEXT    NOT NULL DEFAULT '',
    degradation          TEXT    NOT NULL DEFAULT '',
    cooldown_until       INTEGER,
    draining             INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL DEFAULT 0,
    updated_at           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS provider_models (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id           INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    public_model          TEXT    NOT NULL,
    upstream_model        TEXT    NOT NULL,
    enabled               INTEGER NOT NULL DEFAULT 1,
    priority              INTEGER NOT NULL DEFAULT 100,
    weight                INTEGER NOT NULL DEFAULT 100,
    context_window        INTEGER NOT NULL DEFAULT 0,
    max_output_tokens     INTEGER NOT NULL DEFAULT 0,
    pricing_rules_json    TEXT    NOT NULL DEFAULT '',
    capabilities_json     TEXT    NOT NULL DEFAULT '',
    capabilities_override TEXT    NOT NULL DEFAULT '',
    source                TEXT    NOT NULL DEFAULT 'manual',
    updated_at            INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_provider_models_unique ON provider_models(provider_id, public_model);

CREATE TABLE IF NOT EXISTS models (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    public_name       TEXT    NOT NULL UNIQUE,
    display_name      TEXT    NOT NULL DEFAULT '',
    aliases_json      TEXT    NOT NULL DEFAULT '',
    enabled           INTEGER NOT NULL DEFAULT 1,
    sale_pricing_json TEXT    NOT NULL DEFAULT '',
    policy_json       TEXT    NOT NULL DEFAULT '',
    created_at        INTEGER NOT NULL DEFAULT 0,
    updated_at        INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS model_mappings (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    kind                  TEXT    NOT NULL,
    pattern               TEXT    NOT NULL,
    target_model          TEXT    NOT NULL DEFAULT '',
    target_provider_id    INTEGER NOT NULL DEFAULT 0,
    target_upstream_model TEXT    NOT NULL DEFAULT '',
    priority              INTEGER NOT NULL DEFAULT 100,
    enabled               INTEGER NOT NULL DEFAULT 1,
    note                  TEXT    NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_model_mappings_priority ON model_mappings(enabled, priority);

CREATE TABLE IF NOT EXISTS routes (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    model_id       INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
    provider_id    INTEGER NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    upstream_model TEXT    NOT NULL DEFAULT '',
    priority       INTEGER NOT NULL DEFAULT 100,
    weight         INTEGER NOT NULL DEFAULT 100,
    enabled        INTEGER NOT NULL DEFAULT 1,
    policy_json    TEXT    NOT NULL DEFAULT '',
    cooldown_until INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_routes_unique ON routes(model_id, provider_id);

CREATE TABLE IF NOT EXISTS tags (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    description  TEXT    NOT NULL DEFAULT '',
    grants_json  TEXT    NOT NULL DEFAULT '',
    policy_json  TEXT    NOT NULL DEFAULT '',
    priority     INTEGER NOT NULL DEFAULT 100,
    created_at   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS api_keys (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id         INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name               TEXT    NOT NULL,
    key_prefix         TEXT    NOT NULL,
    key_hash           TEXT    NOT NULL,
    tags_json          TEXT    NOT NULL DEFAULT '',
    grants_json        TEXT    NOT NULL DEFAULT '',
    policy_json        TEXT    NOT NULL DEFAULT '',
    record_input_mode  TEXT    NOT NULL DEFAULT 'inherit',
    record_output_text INTEGER NOT NULL DEFAULT 0,
    record_reasoning   INTEGER NOT NULL DEFAULT 0,
    status             TEXT    NOT NULL DEFAULT 'active',
    expires_at         INTEGER,
    last_used_at       INTEGER,
    created_by         TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_prefix ON api_keys(key_prefix);
CREATE INDEX IF NOT EXISTS idx_api_keys_account ON api_keys(account_id);

CREATE TABLE IF NOT EXISTS mcp_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id   INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    token_hash   TEXT    NOT NULL,
    token_prefix TEXT    NOT NULL,
    status       TEXT    NOT NULL DEFAULT 'active',
    last_used_at INTEGER,
    expires_at   INTEGER,
    created_by   TEXT    NOT NULL DEFAULT '',
    note         TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_tokens_prefix ON mcp_tokens(token_prefix);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id           INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    api_key_id           INTEGER,
    kind                 TEXT    NOT NULL,
    amount_micros        INTEGER NOT NULL,
    balance_after_micros INTEGER NOT NULL,
    ref_type             TEXT    NOT NULL DEFAULT '',
    ref_id               TEXT    NOT NULL DEFAULT '',
    idem_key             TEXT    NOT NULL,
    rebuild_seq          INTEGER NOT NULL DEFAULT 0,
    note                 TEXT    NOT NULL DEFAULT '',
    actor                TEXT    NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_idem ON ledger_entries(idem_key);
CREATE INDEX IF NOT EXISTS idx_ledger_account_time ON ledger_entries(account_id, created_at);

CREATE TABLE IF NOT EXISTS invoices (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id         INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    period_start       INTEGER NOT NULL,
    period_end         INTEGER NOT NULL,
    status             TEXT    NOT NULL DEFAULT 'draft',
    currency           TEXT    NOT NULL DEFAULT 'USD',
    total_cost_micros  INTEGER NOT NULL DEFAULT 0,
    total_charge_micros INTEGER NOT NULL DEFAULT 0,
    issued_at          INTEGER,
    paid_at            INTEGER,
    voided_at          INTEGER,
    note               TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_invoices_period ON invoices(account_id, period_start, period_end);

CREATE TABLE IF NOT EXISTS invoice_lines (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    invoice_id    INTEGER NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    group_type    TEXT    NOT NULL,
    group_key     TEXT    NOT NULL,
    requests      INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micros   INTEGER NOT NULL DEFAULT 0,
    charge_micros INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_invoice_lines_invoice ON invoice_lines(invoice_id);

CREATE TABLE IF NOT EXISTS redemption_codes (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    code_hash              TEXT    NOT NULL UNIQUE,
    amount_micros          INTEGER NOT NULL,
    expires_at             INTEGER,
    redeemed_by_account_id INTEGER,
    redeemed_at            INTEGER,
    batch_id               TEXT    NOT NULL DEFAULT '',
    created_by             TEXT    NOT NULL DEFAULT '',
    note                   TEXT    NOT NULL DEFAULT '',
    created_at             INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS billing_failures (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id   TEXT    NOT NULL,
    attempt_no   INTEGER NOT NULL DEFAULT 0,
    account_id   INTEGER NOT NULL DEFAULT 0,
    payload_json TEXT    NOT NULL DEFAULT '',
    error        TEXT    NOT NULL DEFAULT '',
    retries      INTEGER NOT NULL DEFAULT 0,
    resolved_at  INTEGER,
    created_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_billing_failures_open ON billing_failures(resolved_at, created_at);

CREATE TABLE IF NOT EXISTS billing_reconciliations (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    period_start         INTEGER NOT NULL,
    period_end           INTEGER NOT NULL,
    kind                 TEXT    NOT NULL,
    usage_charge_micros  INTEGER NOT NULL DEFAULT 0,
    ledger_charge_micros INTEGER NOT NULL DEFAULT 0,
    diff_micros          INTEGER NOT NULL DEFAULT 0,
    missing_usage_count  INTEGER NOT NULL DEFAULT 0,
    estimated_ratio_bp   INTEGER NOT NULL DEFAULT 0,
    details_json         TEXT    NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS usage_records (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id         TEXT    NOT NULL,
    attempt_no         INTEGER NOT NULL DEFAULT 1,
    account_id         INTEGER NOT NULL DEFAULT 0,
    api_key_id         INTEGER NOT NULL DEFAULT 0,
    model              TEXT    NOT NULL DEFAULT '',
    resolved_model     TEXT    NOT NULL DEFAULT '',
    provider_id        INTEGER NOT NULL DEFAULT 0,
    dimensions_json    TEXT    NOT NULL DEFAULT '',
    cost_micros        INTEGER NOT NULL DEFAULT 0,
    charge_micros      INTEGER NOT NULL DEFAULT 0,
    overshoot_cost_micros INTEGER NOT NULL DEFAULT 0,
    pricing_snapshot_json TEXT NOT NULL DEFAULT '',
    latency_ms         INTEGER NOT NULL DEFAULT 0,
    ttft_ms            INTEGER NOT NULL DEFAULT 0,
    status             TEXT    NOT NULL DEFAULT '',
    error_code         TEXT    NOT NULL DEFAULT '',
    degraded_features_json TEXT NOT NULL DEFAULT '',
    usage_source       TEXT    NOT NULL DEFAULT '',
    terminated_reason  TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_request ON usage_records(request_id);
CREATE INDEX IF NOT EXISTS idx_usage_account_time ON usage_records(account_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_key_time ON usage_records(api_key_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_model_time ON usage_records(model, created_at);

CREATE TABLE IF NOT EXISTS request_logs (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id           TEXT    NOT NULL,
    api_key_id           INTEGER NOT NULL DEFAULT 0,
    account_id           INTEGER NOT NULL DEFAULT 0,
    endpoint             TEXT    NOT NULL DEFAULT '',
    request_json         TEXT    NOT NULL DEFAULT '',
    response_reasoning   TEXT    NOT NULL DEFAULT '',
    response_text        TEXT    NOT NULL DEFAULT '',
    reasoning_recorded   INTEGER NOT NULL DEFAULT 0,
    output_text_recorded INTEGER NOT NULL DEFAULT 0,
    request_bytes        INTEGER NOT NULL DEFAULT 0,
    response_bytes       INTEGER NOT NULL DEFAULT 0,
    truncated            INTEGER NOT NULL DEFAULT 0,
    record_input_mode    TEXT    NOT NULL DEFAULT '',
    record_reasoning     INTEGER NOT NULL DEFAULT 0,
    record_output_text   INTEGER NOT NULL DEFAULT 0,
    status               TEXT    NOT NULL DEFAULT '',
    created_at           INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_request_logs_time ON request_logs(created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_request_logs_request ON request_logs(request_id);

CREATE TABLE IF NOT EXISTS responses (
    id            TEXT PRIMARY KEY,
    api_key_id    INTEGER NOT NULL DEFAULT 0,
    account_id    INTEGER NOT NULL DEFAULT 0,
    model         TEXT    NOT NULL DEFAULT '',
    provider_id   INTEGER NOT NULL DEFAULT 0,
    status        TEXT    NOT NULL DEFAULT '',
    request_json  TEXT    NOT NULL DEFAULT '',
    output_json   TEXT    NOT NULL DEFAULT '',
    usage_json    TEXT    NOT NULL DEFAULT '',
    instructions  TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL DEFAULT 0,
    completed_at  INTEGER,
    expires_at    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_responses_account ON responses(account_id, created_at);

CREATE TABLE IF NOT EXISTS billing_reservations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id      TEXT    NOT NULL,
    account_id      INTEGER NOT NULL DEFAULT 0,
    reserved_micros INTEGER NOT NULL DEFAULT 0,
    settled_micros  INTEGER NOT NULL DEFAULT 0,
    state           TEXT    NOT NULL DEFAULT 'active',
    expires_at      INTEGER,
    created_at      INTEGER NOT NULL DEFAULT 0,
    updated_at      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_reservations_request ON billing_reservations(request_id);

CREATE TABLE IF NOT EXISTS hooks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    type            TEXT    NOT NULL DEFAULT 'webhook',
    url             TEXT    NOT NULL DEFAULT '',
    secret          TEXT    NOT NULL DEFAULT '',
    events_json     TEXT    NOT NULL DEFAULT '',
    include_content INTEGER NOT NULL DEFAULT 0,
    max_bytes       INTEGER NOT NULL DEFAULT 0,
    sample_rate     REAL    NOT NULL DEFAULT 1.0,
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS usage_counters (
    account_id    INTEGER NOT NULL DEFAULT 0,
    api_key_id    INTEGER NOT NULL DEFAULT 0,
    tag           TEXT    NOT NULL DEFAULT '',
    period        TEXT    NOT NULL,
    requests      INTEGER NOT NULL DEFAULT 0,
    tokens        INTEGER NOT NULL DEFAULT 0,
    cost_micros   INTEGER NOT NULL DEFAULT 0,
    charge_micros INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (account_id, api_key_id, tag, period)
);

CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value_json TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS audit_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    actor       TEXT    NOT NULL DEFAULT '',
    action      TEXT    NOT NULL,
    target_type TEXT    NOT NULL DEFAULT '',
    target_id   TEXT    NOT NULL DEFAULT '',
    changes_json TEXT   NOT NULL DEFAULT '',
    result      TEXT    NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_logs(created_at);

CREATE TABLE IF NOT EXISTS admin_users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL DEFAULT 'admin',
    created_at    INTEGER NOT NULL DEFAULT 0,
    last_login_at INTEGER
);

CREATE TABLE IF NOT EXISTS admin_sessions (
    id         TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    token_hash TEXT    NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS backup_jobs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at   INTEGER NOT NULL DEFAULT 0,
    finished_at  INTEGER,
    path         TEXT    NOT NULL DEFAULT '',
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    status       TEXT    NOT NULL DEFAULT '',
    quick_check  TEXT    NOT NULL DEFAULT '',
    triggered_by TEXT    NOT NULL DEFAULT '',
    note         TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL DEFAULT 0
);
